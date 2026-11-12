package server

import (
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

func bridgeTestHandler(t *testing.T) *Handler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent_chat_generation") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(qoderEnvelope(qoderChunk1) + qoderEnvelope(qoderChunk2) +
			qoderEnvelope("[DONE]")))
	}))
	t.Cleanup(srv.Close)
	qc := qoder.New(qoder.Config{
		Endpoints: map[string]qoder.Endpoint{"cn": {OpenAPI: srv.URL, Gateway: srv.URL, Website: srv.URL}},
		Timeout:   5 * time.Second,
	})
	qp := pool.New("")
	qa := auth.NewQoderAuth("q-1", "qoder", "at-q", "rt-q", "", "cn", "test")
	qa.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	qp.Add(qa)
	return NewHandler(Config{
		Pool: pool.New(""), Upstream: &upstream.Client{},
		Qoder: qc, QoderPool: qp, QoderWeight: 1,
	})
}

func TestAnthropicMessagesNonStream(t *testing.T) {
	h := bridgeTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"message"`) || !strings.Contains(out, "hi from qoder") {
		t.Fatalf("anthropic body=%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop_reason missing: %s", out)
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	h := bridgeTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "text_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "hi from qoder") {
		t.Fatalf("content missing:\n%s", out)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	h := bridgeTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","messages":[{"role":"user","content":"你好"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestResponsesNonStream(t *testing.T) {
	h := bridgeTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"object":"response"`) || !strings.Contains(out, "hi from qoder") {
		t.Fatalf("responses body=%s", out)
	}
}

func TestResponsesStream(t *testing.T) {
	h := bridgeTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"event: response.created", "response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAnthropicAuthRequired(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}, APIKey: "k"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}],"max_tokens":1}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"messages":[]}`))
	req.Header.Set("x-api-key", "k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("authorized invalid request code=%d body=%s", rec.Code, rec.Body.String())
	}
}