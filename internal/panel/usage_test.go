package panel

import (
	"testing"
	"time"
)

// TestAggregateUsageProviders 分上游聚合：各上游的调用/消耗/今日/活跃账号正确，
// 且固定展示顺序（workbuddy → qoder → loomy）。
func TestAggregateUsageProviders(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, usageLoc)
	recs := []UsageRecord{
		{UID: "w1", Nickname: "W1", Provider: "workbuddy", Time: now.Add(-2 * time.Hour), Credit: 1.5, Model: "m1"},
		{UID: "w2", Nickname: "W2", Provider: "workbuddy", Time: now.Add(-30 * time.Minute), Credit: 0, Model: "m1"}, // 免费
		{UID: "q1", Nickname: "Q1", Provider: "qoder", Time: now.Add(-1 * time.Hour), Credit: 3.25, Model: "m2"},
		{UID: "l1", Nickname: "L1", Provider: "loomy", Time: now.Add(-10 * time.Minute), Credit: 0.5, Model: "m3"},
		{UID: "l1", Nickname: "L1", Provider: "loomy", Time: now.Add(-5 * time.Minute), Credit: 0.25, Model: "m3"},
	}
	agg := aggregateUsage(recs, nil, now)

	if len(agg.Providers) != 3 {
		t.Fatalf("应有 3 个上游，got %d: %+v", len(agg.Providers), agg.Providers)
	}
	want := []struct {
		provider                          string
		calls, credit                     int
		creditVal                         float64
		callsToday, creditToday           int
		creditTodayVal                    float64
		activeAccts                       int
	}{
		{"workbuddy", 2, 1, 1.5, 2, 1, 1.5, 2},
		{"qoder", 1, 1, 3.25, 1, 1, 3.25, 1},
		{"loomy", 2, 2, 0.75, 2, 2, 0.75, 1},
	}
	for i, w := range want {
		p := agg.Providers[i]
		if p.Provider != w.provider {
			t.Errorf("[%d].Provider=%s want %s", i, p.Provider, w.provider)
		}
		if p.Calls != w.calls || p.Credit != w.creditVal {
			t.Errorf("[%d] %s calls/credit=%d/%v want %d/%v", i, w.provider, p.Calls, p.Credit, w.calls, w.creditVal)
		}
		if p.CallsToday != w.callsToday || p.CreditToday != w.creditTodayVal {
			t.Errorf("[%d] %s today=%d/%v want %d/%v", i, w.provider, p.CallsToday, p.CreditToday, w.callsToday, w.creditTodayVal)
		}
		if p.ActiveAccts != w.activeAccts {
			t.Errorf("[%d] %s active_accts=%d want %d", i, w.provider, p.ActiveAccts, w.activeAccts)
		}
	}
	if agg.TotalCalls != 5 || agg.TotalCredit != 5.5 {
		t.Errorf("总计 calls/credit=%d/%v want 5/5.5", agg.TotalCalls, agg.TotalCredit)
	}
}

// TestAggregateUsageProviderUnknown 未标注 provider 的事件归入 unknown，不丢数据。
func TestAggregateUsageProviderUnknown(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, usageLoc)
	recs := []UsageRecord{
		{UID: "x1", Provider: "", Time: now.Add(-time.Hour), Credit: 1},
	}
	agg := aggregateUsage(recs, nil, now)
	if len(agg.Providers) != 1 || agg.Providers[0].Provider != "unknown" {
		t.Fatalf("未标注 provider 应归入 unknown，got %+v", agg.Providers)
	}
}

// TestAggregateUsageProviderOrder 消耗大的上游不改变固定展示顺序（workbuddy 恒首）。
func TestAggregateUsageProviderOrder(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, usageLoc)
	recs := []UsageRecord{
		{UID: "q1", Provider: "qoder", Time: now.Add(-time.Hour), Credit: 100}, // qoder 消耗远大
		{UID: "w1", Provider: "workbuddy", Time: now.Add(-time.Hour), Credit: 1},
		{UID: "l1", Provider: "loomy", Time: now.Add(-time.Hour), Credit: 2},
	}
	agg := aggregateUsage(recs, nil, now)
	if len(agg.Providers) != 3 {
		t.Fatalf("got %d providers", len(agg.Providers))
	}
	if agg.Providers[0].Provider != "workbuddy" || agg.Providers[1].Provider != "qoder" || agg.Providers[2].Provider != "loomy" {
		t.Errorf("固定顺序被破坏: %s %s %s",
			agg.Providers[0].Provider, agg.Providers[1].Provider, agg.Providers[2].Provider)
	}
}
