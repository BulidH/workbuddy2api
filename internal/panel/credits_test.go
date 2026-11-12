package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// TestAccountsExposeCreditsAt 面板账号视图必须透出 credits_at：积分是「快照余额 −
// 实时扣减」，前端据此告诉用户这份数字有多旧、要不要点「查询余额」重新校准。
func TestAccountsExposeCreditsAt(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500)
	p.NoteCreditSpend("u1", 3)

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
	accts := out["accounts"].([]any)
	if len(accts) != 1 {
		t.Fatalf("accounts=%v", accts)
	}
	row := accts[0].(map[string]any)
	if row["credits"].(float64) != 497 {
		t.Errorf("credits=%v want 497 (500 snapshot − 3 real-time spend)", row["credits"])
	}
	if s, _ := row["credits_at"].(string); s == "" {
		t.Errorf("credits_at missing: %v", row)
	}
}

// TestAccountsQoderCreditsFallbackNotOverrideCalibratedZero Qoder 凭证文件里的旧余额
// 只在池内从未有过权威快照（credits_at 为空）时才兜底。一旦池内已经有快照，0 就是
// 真实的 0——继续用文件里的旧值覆盖会把"已花完的号"显示成有余额。
func TestAccountsQoderCreditsFallbackNotOverrideCalibratedZero(t *testing.T) {
	qdir := filepath.Join(t.TempDir(), "qoder_auths")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"uid":         "q1",
		"nickname":    "one",
		"accessToken": "at1",
		"realm":       "cn",
		"credits":     map[string]any{"remain_effective": 888}, // 文件里的旧快照
	})
	if err := os.WriteFile(filepath.Join(qdir, "qoder-q1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// ① 池内无该账号（从未有过权威快照）→ 允许文件兜底。
	pn := New(Deps{QoderEnabled: true, QoderAuthDir: qdir, AuthDir: t.TempDir()})
	_, out := doPanel(t, pn, http.MethodGet, "/panel/api/accounts", "")
	if got := pickCredits(t, out, "q1"); got != 888 {
		t.Errorf("uncalibrated fallback credits=%v want 888", got)
	}

	// ② 池内已校准为 0（花完了）→ 不得再用文件旧值覆盖。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "q1"})
	p.SetCredits("q1", 0)
	pn2 := New(Deps{
		QoderEnabled: true,
		QoderAuthDir: qdir,
		AuthDir:      t.TempDir(),
		StatusJSON: func() map[string]any {
			return map[string]any{"accounts": p.List()}
		},
	})
	_, out2 := doPanel(t, pn2, http.MethodGet, "/panel/api/accounts", "")
	if got := pickCredits(t, out2, "q1"); got != 0 {
		t.Errorf("calibrated zero credits=%v want 0 (stale auth-file value must not override)", got)
	}
}

// pickCredits 从 /accounts 响应中取指定 uid 的 credits。
func pickCredits(t *testing.T, out map[string]any, uid string) float64 {
	t.Helper()
	rows, _ := out["accounts"].([]any)
	for _, it := range rows {
		m, ok := it.(map[string]any)
		if !ok || m["uid"] != uid {
			continue
		}
		v, _ := m["credits"].(float64)
		return v
	}
	t.Fatalf("uid %s not in %v", uid, out["accounts"])
	return 0
}
