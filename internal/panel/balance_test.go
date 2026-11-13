package panel

import (
	"context"
	"errors"
	"testing"

	"workbuddy2api/internal/loomyops"
	"workbuddy2api/internal/qoderops"
)

// TestBalanceCoversAllProviders 账号页的「查询余额」必须把三个上游都刷一遍；
// 曾经的实现只回调 WorkBuddy，另两家的积分在页面上永远不动。
func TestBalanceCoversAllProviders(t *testing.T) {
	wb := int64(100)
	qc := int64(200)
	lc := int64(300)
	p := New(Deps{
		QoderEnabled: true,
		LoomyEnabled: true,
		QueryBalances: func() ([]BalanceOutcome, error) {
			return []BalanceOutcome{{UID: "w-1", Nickname: "WB", Realm: "cn", Credits: &wb}}, nil
		},
		QoderBalance: func(context.Context, []string) ([]qoderops.BalanceOutcome, error) {
			return []qoderops.BalanceOutcome{{UID: "q-1", Nickname: "Q", Realm: "cn", Credits: &qc}}, nil
		},
		LoomyBalance: func(context.Context, []string) ([]loomyops.BalanceOutcome, error) {
			return []loomyops.BalanceOutcome{{UID: "l-1", Nickname: "L", Credits: &lc, Detail: "available=300"}}, nil
		},
	})

	code, out := doPanel(t, p, "POST", "/panel/api/balance", `{}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	outs, _ := out["outcomes"].([]any)
	if len(outs) != 3 {
		t.Fatalf("应返回三个上游共 3 条，得 %d", len(outs))
	}
	got := map[string]float64{}
	for _, it := range outs {
		m := it.(map[string]any)
		got[strOf(m["provider"])] = float64(int64Of(m["credits"]))
	}
	for prov, want := range map[string]float64{"workbuddy": 100, "qoder": 200, "loomy": 300} {
		if got[prov] != want {
			t.Errorf("%s credits=%v want %v", prov, got[prov], want)
		}
	}
	sum, _ := out["summary"].(map[string]any)
	byProv, _ := sum["by_provider"].(map[string]any)
	if len(byProv) != 3 {
		t.Fatalf("by_provider=%v", byProv)
	}
}

// TestBalanceToleratesProviderFailure 单个上游整体失败不应让整次查询报错，
// 也不能挡住其它两家的结果。
func TestBalanceToleratesProviderFailure(t *testing.T) {
	wb := int64(10)
	p := New(Deps{
		QoderEnabled: true,
		LoomyEnabled: true,
		QueryBalances: func() ([]BalanceOutcome, error) {
			return []BalanceOutcome{{UID: "w-1", Credits: &wb}}, nil
		},
		QoderBalance: func(context.Context, []string) ([]qoderops.BalanceOutcome, error) {
			return nil, errors.New("qoder upstream down")
		},
		LoomyBalance: func(context.Context, []string) ([]loomyops.BalanceOutcome, error) {
			return nil, errors.New("loomy upstream down")
		},
	})
	code, out := doPanel(t, p, "POST", "/panel/api/balance", `{}`)
	if code != 200 {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	outs, _ := out["outcomes"].([]any)
	if len(outs) != 3 {
		t.Fatalf("成功 1 条 + 失败 2 条，得 %d", len(outs))
	}
	sum, _ := out["summary"].(map[string]any)
	if intOf(sum["ok"]) != 1 || intOf(sum["fail"]) != 2 {
		t.Fatalf("summary=%v", sum)
	}
}

// TestBalanceDisabledProvidersSkipped 未启用的上游不参与查询。
func TestBalanceDisabledProvidersSkipped(t *testing.T) {
	wb := int64(10)
	called := false
	p := New(Deps{
		QueryBalances: func() ([]BalanceOutcome, error) {
			return []BalanceOutcome{{UID: "w-1", Credits: &wb}}, nil
		},
		QoderBalance: func(context.Context, []string) ([]qoderops.BalanceOutcome, error) {
			called = true
			return nil, nil
		},
		LoomyBalance: func(context.Context, []string) ([]loomyops.BalanceOutcome, error) {
			called = true
			return nil, nil
		},
	})
	if code, _ := doPanel(t, p, "POST", "/panel/api/balance", `{}`); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if called {
		t.Fatal("未启用的上游不该被调用")
	}
}
