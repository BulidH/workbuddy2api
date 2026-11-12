package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestDisableRecordsDisabledAt 禁用时刻只在「未禁用 → 禁用」那次跃迁写入；
// 已禁用的号再次收到 session_dead 不应刷新时刻（否则老号永远压在新号前面）。
func TestDisableRecordsDisabledAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	before := time.Now().Add(-time.Second)
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledAt.IsZero() || st.DisabledAt.Before(before) {
		t.Fatalf("禁用应记录 disabled_at: %+v", st)
	}
	first := st.DisabledAt

	time.Sleep(2 * time.Millisecond)
	p.Disable("u1", "again")
	st2, _ := p.Status("u1")
	if !st2.DisabledAt.Equal(first) {
		t.Errorf("重复禁用不该刷新时刻: first=%v second=%v", first, st2.DisabledAt)
	}
}

// TestReviveClearsDisabledAt 解禁后禁用时刻归零，账号重新出现在健康组。
func TestReviveClearsDisabledAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "r")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.Disabled || !st.DisabledAt.IsZero() {
		t.Errorf("解禁应清掉 disabled_at: %+v", st)
	}
}

// TestLegacyDisabledFallsBackToLastErr 旧 state.json 没有 disabled_at：
// 用 last_err 兜底，历史禁用号也能落进时间线而不是全部挤末尾。
func TestLegacyDisabledFallsBackToLastErr(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	lastErr := time.Now().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	raw, _ := json.Marshal(map[string]any{
		"accounts": map[string]any{
			"old": map[string]any{"disabled": true, "reason": "legacy", "last_err": lastErr},
		},
	})
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "old"})
	st, _ := p.Status("old")
	if !st.Disabled || st.DisabledAt.IsZero() {
		t.Fatalf("旧禁用号应有兜底时刻: %+v", st)
	}
	if d := time.Since(st.DisabledAt); d < 2*time.Hour || d > 4*time.Hour {
		t.Errorf("兜底时刻应取 last_err: %v", st.DisabledAt)
	}
}

// TestDisabledAtPersists 禁用时刻落盘，重启后「刚被禁用」仍排在最前。
func TestDisabledAtPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()

	want, _ := p.Status("u1")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	got, _ := p2.Status("u1")
	if !got.Disabled || got.DisabledAt.IsZero() {
		t.Fatalf("重启后 disabled_at 丢失: %+v", got)
	}
	if !got.DisabledAt.Equal(want.DisabledAt) {
		t.Errorf("disabled_at 不一致: %v vs %v", got.DisabledAt, want.DisabledAt)
	}
}
