package loomyops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
)

// fakeLoomySMS 模拟 Loomy 账号服务的短信登录两段：sendMsgCode + checkCode。
type fakeLoomySMS struct {
	mu      sync.Mutex
	sends   int
	phone   string
	msgID   string
	code    string
	userID  string
	logins  int
	lastMsg string
}

func (f *fakeLoomySMS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(loomy.PathSendSMS, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Param struct {
				Phone string `json:"phone"`
			} `json:"param"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sends++
		f.phone = body.Param.Phone
		f.msgID = "msg-1"
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{"msgid": "msg-1"}})
	})
	mux.HandleFunc(loomy.PathCheckSMS, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Param struct {
				Phone string `json:"phone"`
				Code  string `json:"mcode"`
				MsgID string `json:"msgid"`
			} `json:"param"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastMsg = body.Param.MsgID
		if body.Param.Code != f.code {
			writeJSON(w, map[string]any{"code": "100002", "desc": "验证码错误"})
			return
		}
		f.logins++
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{
			"session": "sess-sms-" + f.userID, "userid": f.userID, "phone": body.Param.Phone,
		}})
	})
	return mux
}

func smsTestService(t *testing.T, acct *fakeLoomySMS) (*Service, *pool.Pool, string) {
	t.Helper()
	asrv := httptest.NewServer(acct.handler())
	t.Cleanup(asrv.Close)
	msrv := httptest.NewServer(newFakeLoomy().handler())
	t.Cleanup(msrv.Close)
	dir := t.TempDir()
	p := pool.New("")
	client := loomy.New(loomy.Config{AccountBase: asrv.URL, ModelBase: msrv.URL})
	return New(Config{Pool: p, Client: client, AuthDir: dir, Gap: time.Millisecond, TaskGap: time.Millisecond}), p, dir
}

// TestSMSLoginCreatesSessionOnlyAccount 免密登录：发码 → 校验 → 落盘（无密码）→ 入池。
func TestSMSLoginCreatesSessionOnlyAccount(t *testing.T) {
	acct := &fakeLoomySMS{code: "123456", userID: "u-sms"}
	svc, p, dir := smsTestService(t, acct)

	ticket, exp := svc.StartSMSLogin()
	if ticket == "" || exp <= time.Now().Unix() {
		t.Fatalf("ticket=%q exp=%d", ticket, exp)
	}
	st := svc.SMSLoginStatus(ticket)
	if st.State != "waiting" {
		t.Fatalf("初始状态 %q", st.State)
	}

	res, err := svc.SendSMS(context.Background(), ticket, "13800008000")
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if res.PhoneMasked != "138****8000" {
		t.Fatalf("手机号未脱敏: %q", res.PhoneMasked)
	}
	if acct.sends != 1 {
		t.Fatalf("上游发码次数 %d", acct.sends)
	}
	if st := svc.SMSLoginStatus(ticket); st.State != "sent" {
		t.Fatalf("发送后状态 %q", st.State)
	}

	if _, err := svc.VerifySMS(context.Background(), ticket, "000000", ""); err == nil {
		t.Fatal("错误验证码不应登录成功")
	}

	a, err := svc.VerifySMS(context.Background(), ticket, "123456", "我的 Loomy")
	if err != nil {
		t.Fatalf("VerifySMS: %v", err)
	}
	if a.UID != "u-sms" || a.Nickname != "我的 Loomy" || a.LoginID != "13800008000" {
		t.Fatalf("account=%+v", a)
	}
	if a.AccessToken != "sess-sms-u-sms" {
		t.Fatalf("session=%q", a.AccessToken)
	}
	if a.Password != "" || a.Source != "sms" {
		t.Fatalf("免密账号不该存密码: password=%q source=%q", a.Password, a.Source)
	}
	if acct.lastMsg != "msg-1" {
		t.Fatalf("校验未带 msgid: %q", acct.lastMsg)
	}

	auths, err := auth.LoadLoomyDir(dir)
	if err != nil || len(auths) != 1 {
		t.Fatalf("落盘账号数 %d err=%v", len(auths), err)
	}
	if auths[0].Password != "" || auths[0].AccessToken == "" {
		t.Fatalf("账号文件应只有 session: %+v", auths[0])
	}
	if st, ok := p.Status("u-sms"); !ok || st.Credits != 500 {
		t.Fatalf("应入池并写入余额: %+v ok=%v", st, ok)
	}
	st = svc.SMSLoginStatus(ticket)
	if st.State != "done" || st.Account == nil {
		t.Fatalf("完成后状态 %+v", st)
	}
}

// TestSMSLoginReloginKeepsUIDAndIdentity 同一手机号再次免密登录 = 续期，不产生重复账号。
func TestSMSLoginReloginKeepsUIDAndIdentity(t *testing.T) {
	acct := &fakeLoomySMS{code: "654321", userID: "u-sms"}
	svc, p, dir := smsTestService(t, acct)

	first := func() *auth.Auth {
		ticket, _ := svc.StartSMSLogin()
		if _, err := svc.SendSMS(context.Background(), ticket, "13800008000"); err != nil {
			t.Fatal(err)
		}
		a, err := svc.VerifySMS(context.Background(), ticket, "654321", "")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a1 := first()
	a2 := first()
	if a1.UID != a2.UID {
		t.Fatalf("uid 变了: %s → %s", a1.UID, a2.UID)
	}
	if a1.LoomyDevID == "" || a1.LoomyDevID != a2.LoomyDevID {
		t.Fatalf("设备指纹应沿用: %q → %q", a1.LoomyDevID, a2.LoomyDevID)
	}
	if n := len(p.List()); n != 1 {
		t.Fatalf("池内账号数 %d", n)
	}
	if auths, _ := auth.LoadLoomyDir(dir); len(auths) != 1 {
		t.Fatalf("目录里出现了 %d 个账号文件", len(auths))
	}
}

// TestSMSLoginCooldownValidation 冷却、非法手机号、未知 ticket 都不打上游。
func TestSMSLoginCooldownValidation(t *testing.T) {
	acct := &fakeLoomySMS{code: "111111", userID: "u-sms"}
	svc, _, _ := smsTestService(t, acct)
	ctx := context.Background()

	if _, err := svc.SendSMS(ctx, "nope", "13800008000"); err == nil {
		t.Fatal("未知 ticket 应报错")
	}
	if _, err := svc.SendSMS(ctx, "nope", "12345"); err == nil {
		t.Fatal("非法手机号应报错")
	}
	if acct.sends != 0 {
		t.Fatalf("参数错误不该打上游，sends=%d", acct.sends)
	}

	ticket, _ := svc.StartSMSLogin()
	if _, err := svc.SendSMS(ctx, ticket, "13800008000"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.SendSMS(ctx, ticket, "13800008000")
	kind, ok := IsSMSLoginError(err)
	if !ok || kind != SMSKindCooldown {
		t.Fatalf("冷却应返回 cooldown，得 kind=%q err=%v", kind, err)
	}
	if acct.sends != 1 {
		t.Fatalf("冷却期内不该重发，sends=%d", acct.sends)
	}
}

// TestSMSLoginKeepsExistingPassword 已有密码的账号走验证码重登时，密码必须保留
// （否则会把「可自动续期」的账号降级成免密账号）。
func TestSMSLoginKeepsExistingPassword(t *testing.T) {
	acct := &fakeLoomySMS{code: "777777", userID: "u-sms"}
	svc, p, dir := smsTestService(t, acct)

	pre := auth.NewLoomyAuth("u-sms", "密码号", "13800008000", "pw-secret", "old-session", time.Now().Add(time.Hour).Unix())
	pre.Source = "login"
	pre.FilePath = auth.LoomyFilePath(dir, pre.UID)
	if err := pre.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p.Add(pre)

	ticket, _ := svc.StartSMSLogin()
	if _, err := svc.SendSMS(context.Background(), ticket, "13800008000"); err != nil {
		t.Fatal(err)
	}
	a, err := svc.VerifySMS(context.Background(), ticket, "777777", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Password != "pw-secret" || a.Source != "login" {
		t.Fatalf("密码被清掉/来源被改: password=%q source=%q", a.Password, a.Source)
	}
	if a.AccessToken != "sess-sms-u-sms" {
		t.Fatalf("session 未更新: %q", a.AccessToken)
	}
	if auths, _ := auth.LoadLoomyDir(dir); len(auths) != 1 || auths[0].Password != "pw-secret" {
		t.Fatalf("落盘后密码丢失: %+v", auths)
	}
}

// TestSMSLoginExpires ticket 到期后状态为 expired，且不能继续校验。
func TestSMSLoginExpires(t *testing.T) {
	acct := &fakeLoomySMS{code: "222222", userID: "u-sms"}
	svc, _, _ := smsTestService(t, acct)
	ticket, _ := svc.StartSMSLogin()

	svc.smsMu.Lock()
	svc.smsTickets[ticket].expiresAt = time.Now().Add(-time.Second)
	svc.smsMu.Unlock()

	if st := svc.SMSLoginStatus(ticket); st.State != "expired" {
		t.Fatalf("状态 %q", st.State)
	}
	if st := svc.SMSLoginStatus(ticket); st.State != "unknown" {
		t.Fatalf("过期会话应被清掉，状态 %q", st.State)
	}
}

// TestSMSLoginTooManyAttempts 验证码错误次数超限后拒绝继续校验。
func TestSMSLoginTooManyAttempts(t *testing.T) {
	acct := &fakeLoomySMS{code: "333333", userID: "u-sms"}
	svc, _, _ := smsTestService(t, acct)
	ticket, _ := svc.StartSMSLogin()
	if _, err := svc.SendSMS(context.Background(), ticket, "13800008000"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < SMSMaxAttempts; i++ {
		if _, err := svc.VerifySMS(context.Background(), ticket, "999999", ""); err == nil {
			t.Fatalf("第 %d 次错误验证码不该成功", i+1)
		}
	}
	_, err := svc.VerifySMS(context.Background(), ticket, "333333", "")
	kind, ok := IsSMSLoginError(err)
	if !ok || kind != SMSKindTooMany {
		t.Fatalf("应拒绝继续校验，得 kind=%q err=%v", kind, err)
	}
}
