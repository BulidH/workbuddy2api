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

var aliasTable = map[string]qoder.ModelAlias{
	"deepseek-flash": {WorkBuddy: "deepseek-v4.1-flash", Qoder: "DeepSeek-Flash"},
}

func readModel(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, _ := io.ReadAll(r.Body)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		obj = nil
	}
	if obj == nil || obj["model"] == nil {
		// Qoder 的请求体是加密信封：先解信封再取 model。
		if plain, derr := qoder.DecodeBody(string(raw)); derr == nil {
			_ = json.Unmarshal(plain, &obj)
		}
	}
	m, _ := obj["model"].(string)
	if m == "" {
		// Qoder 上游体把模型放在 model_config.key（display_name/目录 key）。
		mc, _ := obj["model_config"].(map[string]any)
		m, _ = mc["key"].(string)
	}
	return m
}

func TestAliasRewritesWorkBuddyModelName(t *testing.T) {
	var gotModel string
	const sse = `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n" +
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotModel = readModel(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	p := pool.New("")
	p.Add(&auth.Auth{UID: "wb-1", AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	h := NewHandler(Config{Pool: p, Upstream: up, QoderAliases: aliasTable})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "deepseek-v4.1-flash" {
		t.Fatalf("outbound model=%q, want deepseek-v4.1-flash", gotModel)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAliasRewritesQoderModelName(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent_chat_generation") {
			http.NotFound(w, r)
			return
		}
		gotModel = readModel(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(qoderEnvelope(qoderChunk1) + qoderEnvelope(qoderChunk2) + qoderEnvelope("[DONE]")))
	}))
	defer srv.Close()

	qc := qoder.New(qoder.Config{
		Endpoints: map[string]qoder.Endpoint{"cn": {OpenAPI: srv.URL, Gateway: srv.URL, Website: srv.URL}},
		Timeout:   5 * time.Second,
	})
	qp := pool.New("")
	qa := auth.NewQoderAuth("q-1", "qoder", "at-q", "rt-q", "", "cn", "test")
	qa.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	qp.Add(qa)
	h := NewHandler(Config{
		Pool: pool.New(""), Upstream: &upstream.Client{},
		Qoder: qc, QoderPool: qp, QoderWeight: 1, QoderAliases: aliasTable,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder:deepseek-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "dfmodel" {
		t.Fatalf("outbound model key=%q, want dfmodel (DeepSeek-Flash)", gotModel)
	}
}

func TestAppendAliasModels(t *testing.T) {
	h := &Handler{cfg: Config{QoderAliases: aliasTable}}
	out := h.appendAliasModels([]map[string]any{{"id": "cn:deepseek-v4.1-flash"}})
	if len(out) != 2 {
		t.Fatalf("out=%#v", out)
	}
	e := out[1]
	if e["id"] != "deepseek-flash" || e["provider"] != "merged" ||
		e["qoder_model"] != "DeepSeek-Flash" || e["workbuddy_model"] != "deepseek-v4.1-flash" {
		t.Fatalf("entry=%#v", e)
	}
	// Qoder 独有模型已以 display_name 入列时，不重复追加，而是升级为 merged。
	out = h.appendAliasModels([]map[string]any{{
		"id": "DeepSeek-Flash", "owned_by": "qoder", "provider": "qoder",
	}})
	if len(out) != 1 {
		t.Fatalf("duplicate appended: %#v", out)
	}
	if e := out[0]; e["owned_by"] != "merged" || e["provider"] != "merged" ||
		e["qoder_model"] != "DeepSeek-Flash" || e["workbuddy_model"] != "deepseek-v4.1-flash" {
		t.Fatalf("not upgraded: %#v", e)
	}
	// 非 Qoder 条目同名时保持原样。
	out = h.appendAliasModels([]map[string]any{{"id": "deepseek-flash", "owned_by": "workbuddy"}})
	if len(out) != 1 || out[0]["owned_by"] != "workbuddy" {
		t.Fatalf("unexpected overwrite: %#v", out)
	}
}
