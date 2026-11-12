package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// TestAccountsExposeDisabledAt /accounts 必须把禁用时刻透出成前端 Date 可解析的
// RFC3339，否则「刚被禁用的排前头」无从排序。
func TestAccountsExposeDisabledAt(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Disable("u2", "12153 session dead")

	pn := New(Deps{
		AuthDir: t.TempDir(),
		StatusJSON: func() map[string]any {
			return map[string]any{"accounts": p.List()}
		},
	})
	code, out := doPanel(t, pn, http.MethodGet, "/panel/api/accounts", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d out=%v", code, out)
	}
	rows := out["accounts"].([]any)
	byUID := map[string]map[string]any{}
	for _, it := range rows {
		m := it.(map[string]any)
		byUID[m["uid"].(string)] = m
	}
	if s, _ := byUID["u2"]["disabled_at"].(string); s == "" {
		t.Fatalf("禁用账号缺 disabled_at: %v", byUID["u2"])
	} else if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("disabled_at 前端不可解析: %q err=%v", s, err)
	}
	if s, _ := byUID["u1"]["disabled_at"].(string); s != "" {
		t.Errorf("健康账号不该有 disabled_at: %q", s)
	}
}

// TestAccountsDisabledAtLegacyFallback 旧 state.json 的禁用号没有 disabled_at 字段时，
// 池状态用 last_err 兜底，面板照常拿到可排序时间。
func TestAccountsDisabledAtLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	lastErr := time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	raw, _ := json.Marshal(map[string]any{
		"accounts": map[string]any{
			"old": map[string]any{"disabled": true, "reason": "legacy", "last_err": lastErr},
		},
	})
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := pool.New(fp)
	p.Add(&auth.Auth{UID: "old"})
	pn := New(Deps{
		AuthDir: t.TempDir(),
		StatusJSON: func() map[string]any {
			return map[string]any{"accounts": p.List()}
		},
	})
	_, out := doPanel(t, pn, http.MethodGet, "/panel/api/accounts", "")
	row := out["accounts"].([]any)[0].(map[string]any)
	s, _ := row["disabled_at"].(string)
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("旧禁用号兜底时刻不可用: %q err=%v", s, err)
	}
}
