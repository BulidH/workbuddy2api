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
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/upstream"
)

const loomySSE = `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n" +
	`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"points_consumed":7}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestLoomyChatEndToEnd 全链路：裸名别名 → Loomy（priority 2 首选）→ outbound 模型名
// 改写成 Loomy 侧名字 → SSE 透传，且 points_consumed 被映射成 credit。
func TestLoomyChatEndToEnd(t *testing.T) {
	var gotModel, gotAuth, gotPurpose string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		gotModel, _ = body["model"].(string)
		gotAuth = r.Header.Get("Authorization")
		gotPurpose = r.Header.Get("X-Loomy-Request-Purpose")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(loomySSE))
	}))
	defer srv.Close()

	lp := pool.New("")
	la := &auth.Auth{UID: "l-1", AccessToken: "sess-1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	la.SetProvider(auth.ProviderLoomy)
	lp.Add(la)

	// WorkBuddy 池保持空：只要能落到 Loomy，就证明 Loomy 被首选且真的打通了。
	h := NewHandler(Config{
		Pool:          pool.New(""),
		Upstream:      upstream.New(),
		Loomy:         loomy.New(loomy.Config{ModelBase: srv.URL}),
		LoomyPool:     lp,
		LoomyPriority: 2,
		QoderAliases: map[string]qoder.ModelAlias{
			"deepseek-flash": {Loomy: "deepseek-v4-flash-0731"},
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "deepseek-v4-flash-0731" {
		t.Fatalf("outbound model=%q", gotModel)
	}
	if gotAuth != "Bearer sess-1" {
		t.Fatalf("Authorization=%q（Loomy 需要 Bearer + token 双头）", gotAuth)
	}
	if gotPurpose == "" {
		t.Error("缺少 X-Loomy-Request-Purpose 头")
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"ok"`) {
		t.Fatalf("响应体异常: %s", out)
	}
	// 成本记账口径：points_consumed → credit。
	if !strings.Contains(out, `"credit":7`) {
		t.Fatalf("usage.credit 未被映射: %s", out)
	}
}

// TestLoomyChatForcedPrefix loomy: 前缀在裸名候选之外也能直接命中。
func TestLoomyChatForcedPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(loomySSE))
	}))
	defer srv.Close()

	lp := pool.New("")
	la := &auth.Auth{UID: "l-1", AccessToken: "sess-1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	la.SetProvider(auth.ProviderLoomy)
	lp.Add(la)

	h := NewHandler(Config{
		Pool:      pool.New(""),
		Upstream:  upstream.New(),
		Loomy:     loomy.New(loomy.Config{ModelBase: srv.URL}),
		LoomyPool: lp,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"loomy:deepseek-v4-flash-0731","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestLoomyHardCreditCooldownsAccount 积分耗尽的 402 必须把账号冷却到次日 04:00，
// 这样轮转循环才会在下次请求里跳过它、落到其它上游。
func TestLoomyHardCreditCooldownsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"code":"100003","desc":"insufficient points"}`))
	}))
	defer srv.Close()

	lp := pool.New("")
	la := &auth.Auth{UID: "l-1", AccessToken: "sess-1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	la.SetProvider(auth.ProviderLoomy)
	lp.Add(la)

	h := NewHandler(Config{
		Pool:      pool.New(""),
		Upstream:  upstream.New(),
		Loomy:     loomy.New(loomy.Config{ModelBase: srv.URL}),
		LoomyPool: lp,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"loomy:deepseek-v4-flash-0731","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无可用账号时应回 503，得 %d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := lp.Status("l-1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if !st.Cooling {
		t.Fatal("积分耗尽后账号应进入冷却")
	}
}
