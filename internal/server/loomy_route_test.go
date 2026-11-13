package server

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
)

// TestResolveRouteLoomy loomy: 前缀强制 Loomy 且 realm 恒 cn。
func TestResolveRouteLoomy(t *testing.T) {
	provider, realm, bare := resolveRoute("loomy:deepseek-v4-flash-0731")
	if provider != auth.ProviderLoomy || realm != "cn" || bare != "deepseek-v4-flash-0731" {
		t.Fatalf("resolveRoute loomy = (%q,%q,%q)", provider, realm, bare)
	}
}

// TestOrderByPriority 优先级分档：高档整体在前，同档内权重决定顺序。
// 这是「deepseek 优先走 Loomy，Loomy 用完再走其它」的核心保证。
func TestOrderByPriority(t *testing.T) {
	// Loomy 独档 2，WorkBuddy/Qoder 同档 1 → Loomy 必在最前。
	for i := 0; i < 20; i++ {
		got := orderByPriority([]providerRank{
			{auth.ProviderWorkBuddy, 1, 1},
			{auth.ProviderQoder, 1, 1},
			{auth.ProviderLoomy, 1, 2},
		})
		if got[0] != auth.ProviderLoomy {
			t.Fatalf("Loomy 应排第一，得 %v", got)
		}
		if len(got) != 3 {
			t.Fatalf("应保留全部候选，得 %v", got)
		}
	}
	// 全同档且权重偏向 WorkBuddy：统计上 WorkBuddy 排第一的次数应明显更多，
	// 但不能恒为它（否则就是硬优先级，不是加权）。
	// 权重 3:1 → 理论上 P(WorkBuddy 第一)=75%，200 次里落在 120–190 之外的概率可忽略。
	firstWB := 0
	for i := 0; i < 200; i++ {
		got := orderByPriority([]providerRank{
			{auth.ProviderWorkBuddy, 3, 1},
			{auth.ProviderQoder, 1, 1},
		})
		if got[0] == auth.ProviderWorkBuddy {
			firstWB++
		}
	}
	if firstWB < 120 || firstWB > 190 {
		t.Fatalf("加权抽签分布异常：WorkBuddy 第一 %d/200", firstWB)
	}
}

// TestOrderByPriorityEmpty 无候选时返回空（由调用方决定兜底文案）。
func TestOrderByPriorityEmpty(t *testing.T) {
	if got := orderByPriority(nil); got != nil {
		t.Fatalf("应返回 nil，得 %v", got)
	}
}

// TestProviderCandidatesLoomyFirst 端到端：三个上游都可用且都认得该模型时，
// Loomy（priority 2）必须排在第一个候选，另两家仍在其后作降级。
func TestProviderCandidatesLoomyFirst(t *testing.T) {
	wb := pool.New("")
	wb.Add(&auth.Auth{UID: "wb-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	qp := pool.New("")
	qp.Add(&auth.Auth{UID: "q-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	qp.AuthByUID("q-1").SetProvider(auth.ProviderQoder)
	lp := pool.New("")
	la := &auth.Auth{UID: "l-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	la.SetProvider(auth.ProviderLoomy)
	lp.Add(la)

	aliases := map[string]qoder.ModelAlias{
		"deepseek-flash": {WorkBuddy: "deepseek-v4.1-flash", Qoder: "DeepSeek-Flash", Loomy: "deepseek-v4-flash-0731"},
	}
	lclient := loomy.New(loomy.Config{})
	qclient := qoder.New(qoder.Config{})
	qclient.Catalog.ReplaceDynamic("cn", []qoder.ModelInfo{{Key: "DeepSeek-Flash", DisplayName: "DeepSeek-Flash"}})

	h := NewHandler(Config{
		Pool:          wb,
		Qoder:         qclient,
		QoderPool:     qp,
		QoderAliases:  aliases,
		Loomy:         lclient,
		LoomyPool:     lp,
		LoomyPriority: 2,
	})
	// WorkBuddy 池必须也「认得」该模型，否则候选链会少一家（那是 Catalog 语义，不是优先级）。
	for i := 0; i < 10; i++ {
		got := h.providerCandidates("", "deepseek-flash")
		if len(got) == 0 || got[0] != auth.ProviderLoomy {
			t.Fatalf("Loomy 应为首选，得 %v", got)
		}
	}
}

// TestProviderCandidatesLoomyDisabledByNegativePriority priority<0 = 不参与裸名调度。
func TestProviderCandidatesLoomyDisabledByNegativePriority(t *testing.T) {
	wb := pool.New("")
	wb.Add(&auth.Auth{UID: "wb-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	lp := pool.New("")
	la := &auth.Auth{UID: "l-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	la.SetProvider(auth.ProviderLoomy)
	lp.Add(la)

	h := NewHandler(Config{
		Pool:          wb,
		Loomy:         loomy.New(loomy.Config{}),
		LoomyPool:     lp,
		LoomyPriority: -1,
	})
	for _, p := range h.providerCandidates("", "deepseek-v4-flash-0731") {
		if p == auth.ProviderLoomy {
			t.Fatal("priority<0 时 Loomy 不应进入裸名候选")
		}
	}
	// 显式前缀仍然可用。
	if got := h.providerCandidates(auth.ProviderLoomy, "deepseek-v4-flash-0731"); len(got) != 1 || got[0] != auth.ProviderLoomy {
		t.Fatalf("loomy: 前缀应强制 Loomy，得 %v", got)
	}
}

// TestProviderCandidatesLoomyExhaustedFallback Loomy 账号全部冷却（耗尽）时，
// 候选链里不再有 Loomy，请求自然落到其它上游。
func TestProviderCandidatesLoomyExhaustedFallback(t *testing.T) {
	wb := pool.New("")
	wb.Add(&auth.Auth{UID: "wb-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	lp := pool.New("")
	la := &auth.Auth{UID: "l-1", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	la.SetProvider(auth.ProviderLoomy)
	lp.Add(la)
	lp.CooldownUntilTomorrow4AM("l-1", "积分不足")

	h := NewHandler(Config{
		Pool:          wb,
		Loomy:         loomy.New(loomy.Config{}),
		LoomyPool:     lp,
		LoomyPriority: 2,
	})
	got := h.providerCandidates("", "deepseek-v4-flash-0731")
	if len(got) != 1 || got[0] != auth.ProviderWorkBuddy {
		t.Fatalf("Loomy 耗尽后应只剩 WorkBuddy，得 %v", got)
	}
}

// TestLoomySupports 目录匹配 + 别名改写。
func TestLoomySupports(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		Loomy:        loomy.New(loomy.Config{}),
		LoomyPool:    pool.New(""),
		QoderAliases: map[string]qoder.ModelAlias{"deepseek-flash": {Loomy: "deepseek-v4-flash-0731"}},
	})
	if !h.loomySupports("deepseek-v4-flash-0731") {
		t.Error("兜底目录应认得 deepseek-v4-flash-0731")
	}
	if !h.loomySupports("deepseek-flash") {
		t.Error("别名目标在 Loomy 目录内，应算支持")
	}
	if h.loomySupports("gpt-4o") {
		t.Error("非 Loomy 模型不应算支持")
	}
	if h.providerModel(auth.ProviderLoomy, "deepseek-flash") != "deepseek-v4-flash-0731" {
		t.Error("别名改写失败")
	}
}

// TestPoolForLoomy poolFor 返回 Loomy 池。
func TestPoolForLoomy(t *testing.T) {
	lp := pool.New("")
	h := NewHandler(Config{Pool: pool.New(""), Loomy: loomy.New(loomy.Config{}), LoomyPool: lp})
	if h.poolFor(auth.ProviderLoomy) != lp {
		t.Fatal("poolFor(loomy) 应返回 Loomy 池")
	}
}
