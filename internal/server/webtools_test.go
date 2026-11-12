package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/upstream"
)

// fakeWebToolGateway 第一轮返回 web_fetch 工具调用，第二轮返回最终文本。
func fakeWebToolGateway(t *testing.T, calls *int32, secretURL string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent_chat_generation") {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			chunk := `{"id":"q1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_w1","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\""}}]}}]}`
			chunk2 := `{"id":"q1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"` + secretURL + `\"}"}}]}}]}`
			_, _ = w.Write([]byte(qoderEnvelope(chunk) + qoderEnvelope(chunk2) + qoderEnvelope("[DONE]")))
			return
		}
		final := `{"id":"q2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"final answer"}}]}`
		_, _ = w.Write([]byte(qoderEnvelope(final) + qoderEnvelope("[DONE]")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func webToolHandler(t *testing.T, enabled bool) (*Handler, *int32) {
	t.Helper()
	var calls int32
	// 抓取目标指向 loopback：网关必须拒绝，且把错误作为工具结果回灌。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should-not-be-reached"))
	}))
	t.Cleanup(target.Close)
	srv := fakeWebToolGateway(t, &calls, target.URL)
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
		Qoder: qc, QoderPool: qp, QoderWeight: 1,
		QoderWebTools: enabled,
	})
	return h, &calls
}

func webToolBody(stream bool) string {
	return `{"model":"qoder:Qwen3.8-Max","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
		`,"tools":[{"type":"web_fetch","name":"web_fetch"}],"messages":[{"role":"user","content":"read it"}]}`
}

func TestWebToolsLoopNonStream(t *testing.T) {
	h, calls := webToolHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(webToolBody(false))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls=%d, want 2 (tool round + final)", got)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("not json: %s", rec.Body.String())
	}
	msg := chatMessage(obj)
	if msg["content"] != "final answer" {
		t.Fatalf("content=%v", msg["content"])
	}
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("web tool call leaked to client: %#v", msg)
	}
}

func TestWebToolsLoopStream(t *testing.T) {
	h, calls := webToolHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(webToolBody(true))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "data: ") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("not sse: %s", out)
	}
	if !strings.Contains(out, "final answer") {
		t.Fatalf("final content missing: %s", out)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls=%d", got)
	}
}

// 关闭开关时必须走原有直通逻辑，不做本地工具轮。
func TestWebToolsDisabledPassthrough(t *testing.T) {
	h, calls := webToolHandler(t, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(webToolBody(false))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls=%d, want 1", got)
	}
}

// 裸名（可能落 WorkBuddy）不得启用本地 web 工具。
func TestWebToolsOnlyForQoderRoute(t *testing.T) {
	h, _ := webToolHandler(t, true)
	if h.tryQoderWebTools(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil),
		[]byte(webToolBody(false)), "Qwen3.8-Max", false) {
		t.Fatal("bare model name enabled local web tools")
	}
}

func TestWriteChatAsSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	writeChatAsSSE(rec, map[string]any{
		"id":      "x",
		"model":   "m",
		"created": float64(1),
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": "hello"},
			"finish_reason": "stop",
		}},
	})
	out := rec.Body.String()
	for _, want := range []string{"chat.completion.chunk", `"content":"hello"`, `"finish_reason":"stop"`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%s", ct)
	}
}

func TestWebCallsFiltersForeignTools(t *testing.T) {
	msg := map[string]any{"tool_calls": []any{
		map[string]any{"id": "1", "function": map[string]any{"name": "web_search", "arguments": "{}"}},
		map[string]any{"id": "2", "function": map[string]any{"name": "get_weather", "arguments": "{}"}},
	}}
	calls := webCalls(msg, map[string]bool{qoder.WebSearchName: true})
	if len(calls) != 1 || calls[0]["id"] != "1" {
		t.Fatalf("calls=%#v", calls)
	}
}
