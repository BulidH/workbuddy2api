package pool

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestNoteCreditSpendDebits 实时扣减：整数扣费立即反映到 credits 与 Status。
func TestNoteCreditSpendDebits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	p.NoteCreditSpend("u1", 7)

	st, _ := p.Status("u1")
	if st.Credits != 93 {
		t.Errorf("credits=%d want 93", st.Credits)
	}
}

// TestNoteCreditSpendAccumulatesFraction 小数扣费累积：Qoder 单次 ~0.01 的扣费不能因
// int64 截断而全部丢失——连续小扣费攒够 1 之后必须真实落到余额上。
func TestNoteCreditSpendAccumulatesFraction(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	for i := 0; i < 99; i++ { // 99 × 0.0132 = 1.3068 < 2，尚未攒到 2
		p.NoteCreditSpend("u1", 0.0132)
	}
	st, _ := p.Status("u1")
	if st.Credits != 99 {
		t.Fatalf("after 99×0.0132 credits=%d want 99 (accumulated ≥1)", st.Credits)
	}
	for i := 0; i < 100; i++ { // 再 100 次 ≈ +1.32，累计 2.6 → 总扣 2
		p.NoteCreditSpend("u1", 0.0132)
	}
	st, _ = p.Status("u1")
	if st.Credits != 98 {
		t.Errorf("after 199×0.0132 credits=%d want 98", st.Credits)
	}
}

// TestNoteCreditSpendIgnoresInvalid 非有效观测不改余额：0 / 负数 / NaN 一律忽略，
// 缺失的 credit（调用方不会调用）与异常值不得让估算值抖动。
func TestNoteCreditSpendIgnoresInvalid(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 50)

	p.NoteCreditSpend("u1", 0)
	p.NoteCreditSpend("u1", -3)
	p.NoteCreditSpend("u1", math.NaN()) // 上游异常值
	p.NoteCreditSpend("", 5)            // 空 uid
	p.NoteCreditSpend("nope", 5)        // 不存在账号

	st, _ := p.Status("u1")
	if st.Credits != 50 {
		t.Errorf("credits=%d want 50 (invalid observations must not change it)", st.Credits)
	}
}

// TestNoteCreditSpendFloorsAtZero 估算值不允许变负（负积分会污染选号权重与进度条）。
func TestNoteCreditSpendFloorsAtZero(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 3)

	p.NoteCreditSpend("u1", 100)

	st, _ := p.Status("u1")
	if st.Credits != 0 {
		t.Errorf("credits=%d want 0 (floored)", st.Credits)
	}
}

// TestNoteCreditSpendClampsExpiring 快过期子集是总量的一部分，随扣减一起收缩。
func TestNoteCreditSpendClampsExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 10, 10)

	p.NoteCreditSpend("u1", 4)

	p.mu.RLock()
	e := p.byUID["u1"]
	credits, expiring := e.credits, e.creditsExpiring
	p.mu.RUnlock()
	if credits != 6 || expiring != 6 {
		t.Errorf("credits=%d expiring=%d want 6/6 (expiring clamped to total)", credits, expiring)
	}
}

// TestCreditsAtSetBySnapshotAndSurvivesRestart 权威快照记时刻、实时扣减不改时刻，
// 且该时刻随 state.json 持久化（重启后面板仍能显示"这份余额有多旧"）。
func TestCreditsAtSetBySnapshotAndSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})

	before := time.Now().Add(-time.Second)
	p.SetCredits("u1", 100)
	st, _ := p.Status("u1")
	if st.CreditsAt.IsZero() || st.CreditsAt.Before(before) {
		t.Fatalf("SetCredits 后 credits_at=%v want 最近时刻", st.CreditsAt)
	}
	snapAt := st.CreditsAt

	time.Sleep(5 * time.Millisecond)
	p.NoteCreditSpend("u1", 5)
	st, _ = p.Status("u1")
	if !st.CreditsAt.Equal(snapAt) {
		t.Errorf("实时扣减不得改 credits_at: %v -> %v", snapAt, st.CreditsAt)
	}

	p.Flush()
	p.Close()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(raw), `"credits_at"`) {
		t.Fatalf("state.json 缺 credits_at:\n%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st2, ok := p2.Status("u1")
	if !ok {
		t.Fatal("u1 missing after restart")
	}
	if st2.Credits != 95 {
		t.Errorf("restored credits=%d want 95 (snapshot minus real-time spend)", st2.Credits)
	}
	if !st2.CreditsAt.Equal(snapAt.Truncate(time.Nanosecond)) {
		t.Errorf("restored credits_at=%v want %v", st2.CreditsAt, snapAt)
	}
	p2.Close()
}

// TestCreditsAtClearedFracOnResnapshot 权威快照刷新时清掉实时扣减的小数余量：
// 余额已与上游对齐，旧余量再累加就是重复扣减。
func TestCreditsAtClearsFractionOnResnapshot(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.NoteCreditSpend("u1", 0.9) // 不足 1，留在余量里

	p.SetCredits("u1", 100) // 快照对齐
	p.NoteCreditSpend("u1", 0.2)

	st, _ := p.Status("u1")
	if st.Credits != 100 {
		t.Errorf("credits=%d want 100 (snapshot reset the sub-unit remainder)", st.Credits)
	}
}
