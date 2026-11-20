package server

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/qoder"
)

// TestOrderedCandidatesAliasOrder 别名里配了 order 时，按 order 严格覆盖全局优先级。
func TestOrderedCandidatesAliasOrder(t *testing.T) {
	h := &Handler{cfg: Config{
		QoderAliases: map[string]qoder.ModelAlias{
			"deepseek-flash": {Order: []string{"workbuddy", "loomy"}},
		},
		WorkBuddyPriority: 1,
		QoderPriority:     1,
		LoomyPriority:     2, // 全局上 Loomy 最高，但 order 明确要求 WorkBuddy 先试
	}}
	ranks := []providerRank{
		{auth.ProviderWorkBuddy, 1, 1},
		{auth.ProviderQoder, 1, 1},
		{auth.ProviderLoomy, 1, 2},
	}
	for i := 0; i < 20; i++ {
		got := h.orderedCandidates("deepseek-flash", ranks)
		if len(got) != 3 {
			t.Fatalf("应保留全部候选，得 %v", got)
		}
		if got[0] != auth.ProviderWorkBuddy || got[1] != auth.ProviderLoomy {
			t.Fatalf("order 未生效：%v（期望 workbuddy 在前、loomy 次之）", got)
		}
		// 未列在 order 里的 qoder 排在最后。
		if got[2] != auth.ProviderQoder {
			t.Fatalf("未列出的 provider 应排最后：%v", got)
		}
	}
}

// TestOrderedCandidatesNoOrderFallsBack 未配 order 时行为与 orderByPriority 完全一致。
func TestOrderedCandidatesNoOrderFallsBack(t *testing.T) {
	h := &Handler{cfg: Config{
		QoderAliases: map[string]qoder.ModelAlias{"deepseek-flash": {}},
	}}
	ranks := []providerRank{
		{auth.ProviderWorkBuddy, 1, 1},
		{auth.ProviderLoomy, 1, 2},
	}
	for i := 0; i < 10; i++ {
		got := h.orderedCandidates("deepseek-flash", ranks)
		if got[0] != auth.ProviderLoomy {
			t.Fatalf("无 order 时应沿用全局优先级（Loomy 在前），得 %v", got)
		}
	}
}

// TestOrderedCandidatesUnknownAlias 裸名不在 aliases 里时也能正常按全局排序。
func TestOrderedCandidatesUnknownAlias(t *testing.T) {
	h := &Handler{cfg: Config{QoderAliases: map[string]qoder.ModelAlias{}}}
	got := h.orderedCandidates("some-other-model", []providerRank{
		{auth.ProviderWorkBuddy, 1, 1},
		{auth.ProviderLoomy, 1, 2},
	})
	if len(got) != 2 || got[0] != auth.ProviderLoomy {
		t.Fatalf("未知裸名应按全局优先级排序，得 %v", got)
	}
}

// TestOrderedCandidatesEmptyRanks 无候选返回 nil（调用方兜底）。
func TestOrderedCandidatesEmptyRanks(t *testing.T) {
	h := &Handler{cfg: Config{QoderAliases: map[string]qoder.ModelAlias{
		"m": {Order: []string{"loomy"}},
	}}}
	if got := h.orderedCandidates("m", nil); got != nil {
		t.Fatalf("无候选应返回 nil，得 %v", got)
	}
}
