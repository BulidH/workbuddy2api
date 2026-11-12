package server

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
)

func TestResolveRoute(t *testing.T) {
	cases := []struct {
		in        string
		wantProv  string
		wantRealm string
		wantBare  string
	}{
		{"deepseek-v4-pro", "", "cn", "deepseek-v4-pro"},
		{"cn:deepseek-v4-pro", auth.ProviderWorkBuddy, "cn", "deepseek-v4-pro"},
		{"global:gpt-5.4", auth.ProviderWorkBuddy, "global", "gpt-5.4"},
		{"qoder:deepseek-v4-pro", auth.ProviderQoder, "", "deepseek-v4-pro"},
		{"qoder:Qwen3.8-Max", auth.ProviderQoder, "", "Qwen3.8-Max"},
		{"QODER:x", "", "cn", "QODER:x"}, // 前缀大小写敏感
		{"deepseek:v3", "", "cn", "deepseek:v3"},
		{"", "", "cn", ""},
	}
	for _, c := range cases {
		p, r, b := resolveRoute(c.in)
		if p != c.wantProv || r != c.wantRealm || b != c.wantBare {
			t.Errorf("resolveRoute(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, p, r, b, c.wantProv, c.wantRealm, c.wantBare)
		}
	}
}

// newRouteTestHandler 构造带两个上游池的 handler，用于 Provider 候选与粘性测试。
func newRouteTestHandler(t *testing.T) (*Handler, *pool.Pool, *pool.Pool) {
	t.Helper()
	wb := pool.New(filepath.Join(t.TempDir(), "wb.json"))
	wb.SyncToDir([]*auth.Auth{{UID: "wb-1", Nickname: "wb"}})

	qp := pool.New(filepath.Join(t.TempDir(), "q.json"))
	qp.SyncToDir([]*auth.Auth{
		auth.NewQoderAuth("q-1", "qoder", "token", "refresh", "", "cn", "test"),
	})

	h := NewHandler(Config{
		Pool:        wb,
		Qoder:       qoder.New(qoder.Config{}),
		QoderPool:   qp,
		QoderWeight: 1.0,
	})
	return h, wb, qp
}

func TestProviderCandidates(t *testing.T) {
	h, wb, qp := newRouteTestHandler(t)
	defer wb.Close()
	defer qp.Close()

	containsBoth := func(list []string) bool {
		seen := map[string]bool{}
		for _, p := range list {
			seen[p] = true
		}
		return seen[auth.ProviderWorkBuddy] && seen[auth.ProviderQoder]
	}

	// 裸名且 Qoder 目录认识：两池都可服务 → 两个候选（顺序随机）。
	if got := h.providerCandidates("", "deepseek-v4-pro"); !containsBoth(got) {
		t.Errorf("merged candidates = %v, want both providers", got)
	}
	// Qoder 目录不认识：只有 WorkBuddy。
	if got := h.providerCandidates("", "totally-unknown-model"); len(got) != 1 || got[0] != auth.ProviderWorkBuddy {
		t.Errorf("unknown model candidates = %v", got)
	}
	// 强制前缀只返回目标 Provider。
	if got := h.providerCandidates(auth.ProviderQoder, "deepseek-v4-pro"); len(got) != 1 || got[0] != auth.ProviderQoder {
		t.Errorf("forced qoder candidates = %v", got)
	}
	if got := h.providerCandidates(auth.ProviderWorkBuddy, "deepseek-v4-pro"); len(got) != 1 || got[0] != auth.ProviderWorkBuddy {
		t.Errorf("forced wb candidates = %v", got)
	}

	// Qoder 账号全部禁用后自动降级为纯 WorkBuddy。
	qp.Disable("q-1", "test")
	if got := h.providerCandidates("", "deepseek-v4-pro"); len(got) != 1 || got[0] != auth.ProviderWorkBuddy {
		t.Errorf("after qoder disable candidates = %v", got)
	}
}

func TestStickyPoolRoutesProvider(t *testing.T) {
	h, wb, qp := newRouteTestHandler(t)
	defer wb.Close()
	defer qp.Close()

	p, prov := h.stickyPool("q-1")
	if p != qp || prov != auth.ProviderQoder {
		t.Errorf("stickyPool(q-1) = %p/%q, want qoder pool", p, prov)
	}
	p, prov = h.stickyPool("wb-1")
	if p != wb || prov != auth.ProviderWorkBuddy {
		t.Errorf("stickyPool(wb-1) = %p/%q, want wb pool", p, prov)
	}
	if p, prov = h.stickyPool("nope"); p != nil || prov != "" {
		t.Errorf("stickyPool(nope) = %p/%q, want nil", p, prov)
	}
}
