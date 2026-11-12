package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestChatDebitsCreditsFromStreamUsage 流式成功请求的 usage.credit 必须实时扣减账号余额，
// 否则两次权威快照（签到 / 查询余额）之间面板积分一直停在旧值——用户抱怨的
// 「积分显示不正确，点一下查询余额才正确」正是这个。
func TestChatDebitsCreditsFromStreamUsage(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\"," +
		"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
		"\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":10,\"credit\":0.5}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 两次扣 0.5 = 1 积分：小数余量攒够 1 后必须落到余额上。
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("attempt %d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if st.Credits != 999 {
		t.Errorf("credits=%d want 999 (1000 snapshot − 2×0.5 real-time spend)", st.Credits)
	}
}

// TestChatNoDebitWhenCreditMissing usage 存在但缺 credit 字段时不得扣减（缺失≠0）：
// 扣了就把"没观测到"当成"免费"，面板数字会莫名其妙往下掉。
func TestChatNoDebitWhenCreditMissing(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	h.ServeHTTP(rec, req)

	st, _ := p.Status("u1")
	if st.Credits != 1000 {
		t.Errorf("credits=%d want 1000 (usage without credit must not debit)", st.Credits)
	}
}
