package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/upstream"
)

// qoderEnvelope 把内层 OpenAI chunk 包成 Qoder SSE 信封帧。
func qoderEnvelope(inner string) string {
	raw, _ := json.Marshal(map[string]any{
		"headers":         map[string]any{},
		"body":            inner,
		"statusCodeValue": 200,
	})
	return "data: " + string(raw) + "\n\n"
}

const qoderChunk1 = `{"id":"q1","object":"chat.completion.chunk","created":1,"model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"role":"assistant","content":"hi from qoder"}}]}`
const qoderChunk2 = `{"id":"q1","object":"chat.completion.chunk","created":1,"model":"Qwen3.8-Max","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`

// TestChatQoderEndToEndNonStream 端到端：qoder: 前缀请求 → 强制走 Qoder 池 →
// COSY 签名请求打到 fake 网关 → 信封 SSE 解包 → 非流式本地聚合返回 200。
func TestChatQoderEndToEndNonStream(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotModelKey, gotAuthz string
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent_chat_generation") {
			http.NotFound(w, r)
			return
		}
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("cosy-key")
		gotModelKey = r.Header.Get("x-model-key")
		gotAuthz = r.Header.Get("authorization")
		encoded, _ := io.ReadAll(r.Body)
		if plain, err := qoder.DecodeBody(string(encoded)); err == nil {
			_ = json.Unmarshal(plain, &gotReq)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(qoderEnvelope(qoderChunk1) + qoderEnvelope(qoderChunk2) +
			qoderEnvelope("[DONE]")))
	}))
	defer srv.Close()

	qc := qoder.New(qoder.Config{
		Endpoints: map[string]qoder.Endpoint{
			"cn": {OpenAPI: srv.URL, Gateway: srv.URL, Website: srv.URL},
		},
		CosyVersion: "1.1.75",
		Timeout:     5 * time.Second,
	})
	qp := pool.New("")
	qa := auth.NewQoderAuth("q-1", "qoder", "at-q", "rt-q", "", "cn", "test")
	qa.ExpiresAt = time.Now().Add(24 * time.Hour).Unix() // 避免触发 refresh 分支
	qp.Add(qa)
	wb := pool.New("")
	h := NewHandler(Config{Pool: wb, Upstream: &upstream.Client{}, Qoder: qc, QoderPool: qp, QoderWeight: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hi from qoder") {
		t.Fatalf("aggregated body missing content: %s", rec.Body.String())
	}

	if gotPath != "/algo/api/v2/service/pro/sse/agent_chat_generation" {
		t.Errorf("gateway path = %q", gotPath)
	}
	// 请求体是自定义 Base64 变体，必须带 Encode=1 告诉网关按编码体解析；
	// 缺了它上游会按明文 JSON 解析并以 500 断开（空 SSE 流）。
	if gotQuery != "Encode=1" {
		t.Errorf("gateway query = %q, want Encode=1", gotQuery)
	}
	if gotKey == "" || !strings.HasPrefix(gotAuthz, "Bearer COSY.") {
		t.Errorf("missing COSY signing headers: cosy-key=%q authz=%q", gotKey, gotAuthz)
	}
	if gotModelKey == "" {
		t.Errorf("x-model-key should carry the catalog key, got empty")
	}
	if gotReq == nil {
		t.Fatalf("outbound request body is not decodable Qoder-encoded JSON")
	}
	// 网关侧前缀不得泄漏给上游：请求模型必须是裸名。
	if m, _ := gotReq["model_config"].(map[string]any); m == nil || m["key"] != gotModelKey {
		t.Errorf("model_config = %v (x-model-key=%q)", gotReq["model_config"], gotModelKey)
	}
	cc, _ := gotReq["chat_context"].(map[string]any)
	if txt, _ := cc["text"].(map[string]any); txt["text"] != "hi" {
		t.Errorf("chat_context.text = %v", txt["text"])
	}

	st, ok := qp.Status("q-1")
	if !ok || st.ErrTotal != 0 || st.SuccessCount != 1 {
		t.Errorf("pool status after success = %+v", st)
	}
}

// TestChatQoderEndToEndStream 流式：信封 SSE 解包后直接透传 OpenAI data: 行，
// 末尾 [DONE]，不经过聚合。
func TestChatQoderEndToEndStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(qoderEnvelope(qoderChunk1) + qoderEnvelope(qoderChunk2) +
			qoderEnvelope("[DONE]")))
	}))
	defer srv.Close()

	qc := qoder.New(qoder.Config{
		Endpoints: map[string]qoder.Endpoint{
			"cn": {OpenAPI: srv.URL, Gateway: srv.URL, Website: srv.URL},
		},
	})
	qp := pool.New("")
	qa := auth.NewQoderAuth("q-1", "qoder", "at-q", "rt-q", "", "cn", "test")
	qa.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	qp.Add(qa)
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}, Qoder: qc, QoderPool: qp})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "hi from qoder") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream body = %q", out)
	}
	if strings.Contains(out, "statusCodeValue") {
		t.Errorf("envelope leaked into client stream: %q", out)
	}
}

// TestChatQoderForcedPrefixUnknownModel 强制 qoder: 前缀但目录无此模型 → 400，
// 不打上游、不轮转。
func TestChatQoderForcedPrefixUnknownModel(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	qc := qoder.New(qoder.Config{Endpoints: map[string]qoder.Endpoint{
		"cn": {OpenAPI: srv.URL, Gateway: srv.URL, Website: srv.URL},
	}})
	qp := pool.New("")
	qa := auth.NewQoderAuth("q-1", "qoder", "at-q", "", "", "cn", "test")
	qa.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	qp.Add(qa)
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}, Qoder: qc, QoderPool: qp})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder:no-such-model","messages":[]}`)))
	if rec.Code != http.StatusBadRequest || !assertJSONErrorCode(t, rec.Body.String(), "model_not_found") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Errorf("upstream should not be called, hits=%d", hits)
	}
}
