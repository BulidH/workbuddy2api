package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomyops"
)

// newLoomySMSPanel 构造只启用 Loomy 短信登录的面板，回调由测试注入。
func newLoomySMSPanel(t *testing.T, status string) *Panel {
	t.Helper()
	var ticket string
	return New(Deps{
		LoomyEnabled: true,
		LoomyAuthDir: t.TempDir(),
		LoomySMSStart: func() (string, int64, error) {
			ticket = "t-123"
			return ticket, time.Now().Add(15 * time.Minute).Unix(), nil
		},
		LoomySMSSend: func(_ context.Context, got, phone string) (loomyops.SMSSendResult, error) {
			if got != ticket {
				return loomyops.SMSSendResult{}, &loomyops.SMSLoginError{Kind: loomyops.SMSKindNotFound, Message: "bad ticket"}
			}
			return loomyops.SMSSendResult{PhoneMasked: "138****8000", ResendIn: 60}, nil
		},
		LoomySMSVerify: func(_ context.Context, got, code, nickname string) (*auth.Auth, error) {
			if code != "123456" {
				return nil, &loomyops.SMSLoginError{Kind: loomyops.SMSKindNoCode, Message: "验证码错误"}
			}
			a := auth.NewLoomyAuth("u-sms", nickname, "13800008000", "", "sess", time.Now().Add(14*24*time.Hour).Unix())
			a.Source = "sms"
			a.Credits = map[string]any{"available": float64(888), "remain": float64(888)}
			return a, nil
		},
		LoomySMSStatus: func(string) loomyops.SMSLoginStatus {
			return loomyops.SMSLoginStatus{State: status, Message: "m", PhoneMasked: "138****8000"}
		},
		ReloadLoomyAuths: func() (int, error) { return 1, nil },
	})
}

// TestPanelLoomySMSFlow start/send/verify 三步都能走通，响应不回显密码类字段。
func TestPanelLoomySMSFlow(t *testing.T) {
	p := newLoomySMSPanel(t, "sent")

	code, out := doPanel(t, p, "POST", "/panel/api/loomy/sms/start", `{}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("start: code=%d out=%+v", code, out)
	}
	ticket, _ := out["ticket"].(string)
	if ticket != "t-123" {
		t.Fatalf("ticket=%v", out["ticket"])
	}
	url, _ := out["url"].(string)
	if !strings.Contains(url, "/panel/loomy-login?t=t-123") {
		t.Fatalf("url=%q", url)
	}

	code, out = doPanel(t, p, "POST", "/panel/api/loomy/sms/send",
		`{"ticket":"t-123","phone":"13800008000"}`)
	if code != 200 || out["phone"] != "138****8000" {
		t.Fatalf("send: code=%d out=%+v", code, out)
	}
	if out["resend_in"] != float64(60) {
		t.Fatalf("resend_in=%v", out["resend_in"])
	}

	code, out = doPanel(t, p, "POST", "/panel/api/loomy/sms/verify",
		`{"ticket":"t-123","code":"123456","nickname":"免密号"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("verify: code=%d out=%+v", code, out)
	}
	acct, _ := out["account"].(map[string]any)
	if acct["uid"] != "u-sms" || acct["nickname"] != "免密号" {
		t.Fatalf("account=%+v", acct)
	}
	if acct["mode"] != "sms" || acct["source"] != "sms" {
		t.Fatalf("免密账号应标 mode=sms: %+v", acct)
	}
	if acct["loginid"] != "138****8000" {
		t.Fatalf("手机号未脱敏: %v", acct["loginid"])
	}
	raw := mustJSON(t, out)
	if strings.Contains(raw, "sess") {
		t.Fatal("响应里不应出现 session")
	}
}

// TestPanelLoomySMSStatus status 轮询在 done 时附带账号摘要。
func TestPanelLoomySMSStatus(t *testing.T) {
	p := newLoomySMSPanel(t, "done")
	code, out := doPanel(t, p, "GET", "/panel/api/loomy/sms/status?ticket=t-123", ``)
	if code != 200 || out["state"] != "done" {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	if out["phone_masked"] != "138****8000" {
		t.Fatalf("phone=%v", out["phone_masked"])
	}
}

// TestPanelLoomySMSDisabled 未启用时四个接口都返回 501。
func TestPanelLoomySMSDisabled(t *testing.T) {
	p := New(Deps{})
	for _, tc := range []struct{ method, path string }{
		{"POST", "/panel/api/loomy/sms/start"},
		{"POST", "/panel/api/loomy/sms/send"},
		{"POST", "/panel/api/loomy/sms/verify"},
		{"GET", "/panel/api/loomy/sms/status?ticket=x"},
	} {
		if code, _ := doPanel(t, p, tc.method, tc.path, `{}`); code != http.StatusNotImplemented {
			t.Fatalf("%s %s code=%d want 501", tc.method, tc.path, code)
		}
	}
}

// TestPanelLoomyLoginPage 登录页可访问，且内嵌 HTML 引用了短信登录接口。
func TestPanelLoomyLoginPage(t *testing.T) {
	p := New(Deps{})
	req := httptest.NewRequest(http.MethodGet, "/panel/loomy-login?t=abc", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Loomy 免密登录", "/panel/api/loomy/sms/send", "/panel/api/loomy/sms/verify"} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %q", want)
		}
	}
}

// TestPanelLoomySMSVerifyError 验证码错误 → 400 且带上游/会话文案。
func TestPanelLoomySMSVerifyError(t *testing.T) {
	p := newLoomySMSPanel(t, "sent")
	code, out := doPanel(t, p, "POST", "/panel/api/loomy/sms/verify", `{"ticket":"t-123","code":"bad"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d", code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "验证码错误") {
		t.Fatalf("error=%v", out["error"])
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "sess") {
		t.Fatal("错误响应不应泄漏 session")
	}
}
