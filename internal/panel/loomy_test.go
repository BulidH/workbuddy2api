package panel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newLoomyPanel 构造只启用 Loomy 的面板；add 回调可注入登录结果。
func newLoomyPanel(t *testing.T, add func(ctx context.Context, loginID, password, nickname string) (*auth.Auth, error),
	imp func(items [][]byte) ([]*auth.Auth, []error)) *Panel {
	t.Helper()
	dir := t.TempDir()
	return New(Deps{
		LoomyEnabled:        true,
		LoomyAuthDir:        dir,
		LoomyAddAccount:     add,
		LoomyImportAccounts: imp,
		ReloadLoomyAuths:    func() (int, error) { return 1, nil },
	})
}

// TestPanelLoomyLogin 面板「添加账号 · Loomy」：手机号+密码走注入的登录回调，
// 返回脱敏后的账号摘要，且不把密码回显给前端。
func TestPanelLoomyLogin(t *testing.T) {
	var gotPhone, gotPwd, gotName string
	p := newLoomyPanel(t, func(_ context.Context, loginID, password, nickname string) (*auth.Auth, error) {
		gotPhone, gotPwd, gotName = loginID, password, nickname
		a := auth.NewLoomyAuth("u-1", nickname, loginID, password, "sess", time.Now().Add(14*24*time.Hour).Unix())
		a.Credits = map[string]any{"available": float64(1234), "remain": float64(1234)}
		return a, nil
	}, nil)

	code, out := doPanel(t, p, "POST", "/panel/api/loomy/login",
		`{"loginid":" 13800008000 ","password":"p@ss","nickname":"备注"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	if gotPhone != "13800008000" || gotPwd != "p@ss" || gotName != "备注" {
		t.Fatalf("回调入参 phone=%q pwd=%q name=%q", gotPhone, gotPwd, gotName)
	}
	acct, _ := out["account"].(map[string]any)
	if acct["uid"] != "u-1" || acct["nickname"] != "备注" {
		t.Fatalf("account=%+v", acct)
	}
	if acct["loginid"] != "138****8000" {
		t.Fatalf("手机号未脱敏: %v", acct["loginid"])
	}
	if acct["credits"] != float64(1234) {
		t.Fatalf("credits=%v", acct["credits"])
	}
	raw := mustJSON(t, out)
	if strings.Contains(raw, "p@ss") {
		t.Fatal("响应里不应出现密码")
	}
}

// mustJSON 把响应载荷序列化，用于断言某些字段没有泄漏。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestPanelLoomyLoginValidation 缺字段直接 400，不打上游。
func TestPanelLoomyLoginValidation(t *testing.T) {
	called := false
	p := newLoomyPanel(t, func(context.Context, string, string, string) (*auth.Auth, error) {
		called = true
		return nil, nil
	}, nil)
	for _, body := range []string{`{"password":"x"}`, `{"loginid":"13800008000"}`, `{}`} {
		if code, _ := doPanel(t, p, "POST", "/panel/api/loomy/login", body); code != 400 {
			t.Fatalf("body=%s code=%d want 400", body, code)
		}
	}
	if called {
		t.Fatal("参数不完整不该调用登录回调")
	}
}

// TestPanelLoomyLoginUpstreamError 上游密码错误 → 400 且带原文案（前端就地展示）。
func TestPanelLoomyLoginUpstreamError(t *testing.T) {
	p := newLoomyPanel(t, func(context.Context, string, string, string) (*auth.Auth, error) {
		return nil, errors.New("账号或密码错误")
	}, nil)
	code, out := doPanel(t, p, "POST", "/panel/api/loomy/login", `{"loginid":"13800008000","password":"bad"}`)
	if code != 400 {
		t.Fatalf("code=%d", code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "账号或密码错误") {
		t.Fatalf("error=%v", out["error"])
	}
}

// TestPanelLoomyImport 账号文件导入：对象/数组/accounts 包装都走同一条路径。
func TestPanelLoomyImport(t *testing.T) {
	var got int
	p := newLoomyPanel(t, nil, func(items [][]byte) ([]*auth.Auth, []error) {
		got = len(items)
		out := make([]*auth.Auth, 0, len(items))
		for range items {
			out = append(out, auth.NewLoomyAuth("u-1", "导入号", "13900001111", "p", "s", 0))
		}
		return out, nil
	})
	code, out := doPanel(t, p, "POST", "/panel/api/loomy/import",
		`{"accounts":[{"uid":"a"},{"uid":"b"}]}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	if got != 2 {
		t.Fatalf("导入条数 %d", got)
	}
	if views, _ := out["imported"].([]any); len(views) != 2 {
		t.Fatalf("imported=%v", out["imported"])
	}
}

// TestPanelLoomyDisabled 未启用时两个入口都返回 501，而不是 500。
func TestPanelLoomyDisabled(t *testing.T) {
	p := New(Deps{})
	for _, path := range []string{"/panel/api/loomy/login", "/panel/api/loomy/import"} {
		if code, _ := doPanel(t, p, "POST", path, `{}`); code != 501 {
			t.Fatalf("%s code=%d want 501", path, code)
		}
	}
}
