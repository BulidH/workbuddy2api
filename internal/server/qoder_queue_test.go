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

// qoderQueue403Body 线上真实抓到的排队响应（token 完全有效时 DeepSeek-V4-Pro 排队 60s）。
const qoderQueue403Body = `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"errorCode\\\":0,\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"dmodel\\\",\\\"queueCount\\\":1,\\\"queueType\\\":\\\"p4\\\",\\\"retryAfterSeconds\\\":60,\\\"serviceAvailable\\\":true,\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\",\\\"waitTime\\\":60}\",\"subCode\":\"RESOURCE_LIMIT\"}"}`

// newQoderProbeHandler 构造一个 Qoder 池只有单号、上游固定返回 status/body 的 handler。
func newQoderProbeHandler(t *testing.T, status int, body string) (*Handler, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	qc := qoder.New(qoder.Config{
		Endpoints:   map[string]qoder.Endpoint{"cn": {OpenAPI: srv.URL, Gateway: srv.URL, Website: srv.URL}},
		CosyVersion: "1.1.75",
		Timeout:     5 * time.Second,
	})
	qp := pool.New("")
	qa := auth.NewQoderAuth("q-1", "qoder", "at-q", "rt-q", "", "cn", "test")
	qa.ExpiresAt = time.Now().Add(24 * time.Hour).Unix() // 不触发 refresh 分支
	qp.Add(qa)
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}, Qoder: qc, QoderPool: qp, QoderWeight: 1})
	return h, qp
}

func postQoderChat(h *Handler) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder:Qwen3.8-Max","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	return rec.Code
}

// TestQoderQueue403CoolsDownInsteadOfDisabling 回归（线上事故：nick8134694625 被禁用）：
// Qoder 把「排队 60 秒 / 资源限流」包在 HTTP 403 里，旧分类把它当 session_dead 一次性
// 永久禁用。正确行为是软冷却到上游给出的 retryAfterSeconds（60s），账号留在池里。
func TestQoderQueue403CoolsDownInsteadOfDisabling(t *testing.T) {
	h, qp := newQoderProbeHandler(t, http.StatusForbidden, qoderQueue403Body)
	postQoderChat(h)

	st, ok := qp.Status("q-1")
	if !ok {
		t.Fatal("q-1 missing")
	}
	if st.Disabled {
		t.Fatalf("排队 403 不得禁用账号: reason=%q", st.Reason)
	}
	if !st.Cooling {
		t.Fatalf("排队 403 应软冷却: %+v", st)
	}
	if st.CoolRemaining < 50 || st.CoolRemaining > 65 {
		t.Errorf("cool_remaining=%ds want ~60s（对齐上游 retryAfterSeconds）", st.CoolRemaining)
	}
}

// TestQoder401DisablesOnlyAfterConsecutive Hits Qoder 的 401 走连续计数（与
// keepalive/checkin 对 12153 同口径）：单次不杀号，连续 3 次才禁用。
func TestQoder401DisablesOnlyAfterConsecutiveHits(t *testing.T) {
	h, qp := newQoderProbeHandler(t, http.StatusUnauthorized, `{"code":"401","message":"unauthorized"}`)

	postQoderChat(h)
	if st, _ := qp.Status("q-1"); st.Disabled {
		t.Fatalf("首次 401 不得禁用账号: %+v", st)
	}
	postQoderChat(h)
	if st, _ := qp.Status("q-1"); st.Disabled {
		t.Fatalf("第二次 401 不得禁用账号: %+v", st)
	}
	postQoderChat(h)
	st, _ := qp.Status("q-1")
	if !st.Disabled {
		t.Fatalf("连续 %d 次 401 应禁用账号: %+v", pool.SessionDeadThreshold(), st)
	}
}
