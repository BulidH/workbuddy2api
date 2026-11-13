package loomyops

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
)

// fakeLoomyAccount 模拟 Loomy 账号服务（getPuKey + byPwd），用真实 RSA 密钥，
// 校验客户端确实把口令按 PKCS#1 v1.5 加密后提交。
type fakeLoomyAccount struct {
	priv     *rsa.PrivateKey
	password string
	userID   string
	logins   int
}

func newFakeLoomyAccount(t *testing.T, password, userID string) *fakeLoomyAccount {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	return &fakeLoomyAccount{priv: priv, password: password, userID: userID}
}

func (f *fakeLoomyAccount) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login/account/getPuKey", func(w http.ResponseWriter, r *http.Request) {
		der, err := x509.MarshalPKIXPublicKey(&f.priv.PublicKey)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// 上游下发的是 DER 的 base64（不是 PEM），客户端按此解析。
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{
			"pukey": base64.StdEncoding.EncodeToString(der), "rcode": "rc-1",
		}})
	})
	mux.HandleFunc("/login/account/byPwd", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Param struct {
				LoginID  string `json:"loginid"`
				Password string `json:"password"`
			} `json:"param"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		enc, err := base64.StdEncoding.DecodeString(body.Param.Password)
		if err != nil {
			writeJSON(w, map[string]any{"code": "100001", "desc": "bad payload"})
			return
		}
		raw, err := rsa.DecryptPKCS1v15(nil, f.priv, enc)
		if err != nil || string(raw) != f.password {
			writeJSON(w, map[string]any{"code": "100001", "desc": "账号或密码错误"})
			return
		}
		f.logins++
		writeJSON(w, map[string]any{"code": "000000", "data": map[string]any{
			"session": "sess-" + f.userID, "userid": f.userID, "phone": body.Param.LoginID,
		}})
	})
	return mux
}

// loginTestService 起账号服务 + 模型网关，返回 Service 与目录。
func loginTestService(t *testing.T, acct *fakeLoomyAccount) (*Service, *pool.Pool, string) {
	t.Helper()
	asrv := httptest.NewServer(acct.handler())
	t.Cleanup(asrv.Close)
	model := newFakeLoomy()
	msrv := httptest.NewServer(model.handler())
	t.Cleanup(msrv.Close)

	dir := t.TempDir()
	p := pool.New("")
	client := loomy.New(loomy.Config{AccountBase: asrv.URL, ModelBase: msrv.URL})
	svc := New(Config{
		Pool:    p,
		Client:  client,
		AuthDir: dir,
		Gap:     time.Millisecond,
		TaskGap: time.Millisecond,
	})
	return svc, p, dir
}

// TestAddAccountLoginsAndPersists 面板登录：口令加密提交 → 落盘 → 入池 → 余额入库。
func TestAddAccountLoginsAndPersists(t *testing.T) {
	acct := newFakeLoomyAccount(t, "s3cret", "u-100")
	svc, p, dir := loginTestService(t, acct)

	a, err := svc.AddAccount(context.Background(), " 13800008000 ", "s3cret", "我的号")
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if acct.logins != 1 {
		t.Fatalf("上游登录次数 %d", acct.logins)
	}
	if a.UID != "u-100" || a.Nickname != "我的号" {
		t.Fatalf("account=%+v", a)
	}
	if a.LoginID != "13800008000" || a.Password != "s3cret" {
		t.Fatalf("凭证不该被裁剪或丢失: loginid=%q", a.LoginID)
	}
	if st, ok := p.Status("u-100"); !ok || st.Credits != 500 {
		t.Fatalf("应入池并写入余额: %+v ok=%v", st, ok)
	}
	auths, err := auth.LoadLoomyDir(dir)
	if err != nil || len(auths) != 1 {
		t.Fatalf("落盘账号数 %d err=%v", len(auths), err)
	}
	if auths[0].LoginID != "13800008000" || auths[0].Password != "s3cret" {
		t.Fatalf("账号文件缺少密码（14 天后无法续期）: %+v", auths[0])
	}
	if auths[0].ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt 未写入: %d", auths[0].ExpiresAt)
	}
}

// TestAddAccountReLoginKeepsUID 同一手机号再次添加 = 重新登录：沿用 uid，不产生重复账号。
func TestAddAccountReLoginKeepsUID(t *testing.T) {
	acct := newFakeLoomyAccount(t, "s3cret", "u-100")
	svc, p, dir := loginTestService(t, acct)

	first, err := svc.AddAccount(context.Background(), "13800008000", "s3cret", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.AddAccount(context.Background(), "13800008000", "s3cret", "改个名")
	if err != nil {
		t.Fatal(err)
	}
	if first.UID != second.UID {
		t.Fatalf("uid 变了: %s → %s", first.UID, second.UID)
	}
	if second.Nickname != "改个名" {
		t.Fatalf("昵称未更新: %q", second.Nickname)
	}
	if auths, _ := auth.LoadLoomyDir(dir); len(auths) != 1 {
		t.Fatalf("目录里出现了 %d 个账号文件", len(auths))
	}
	if n := len(p.List()); n != 1 {
		t.Fatalf("池内账号数 %d", n)
	}
	if first.LoomyDevID == "" || second.LoomyDevID != first.LoomyDevID {
		t.Fatalf("设备指纹应沿用: %q → %q", first.LoomyDevID, second.LoomyDevID)
	}
}

// TestAddAccountMissingCredentials 空输入不发上游请求。
func TestAddAccountMissingCredentials(t *testing.T) {
	acct := newFakeLoomyAccount(t, "s3cret", "u-100")
	svc, _, _ := loginTestService(t, acct)
	if _, err := svc.AddAccount(context.Background(), " ", "s3cret", ""); err == nil {
		t.Fatal("空手机号应报错")
	}
	if _, err := svc.AddAccount(context.Background(), "13800008000", "", ""); err == nil {
		t.Fatal("空密码应报错")
	}
	if acct.logins != 0 {
		t.Fatal("参数不完整不该打上游")
	}
}

// TestImportAccounts 粘贴 loomy-*.json：解析、落盘、入池；坏数据逐条报错。
func TestImportAccounts(t *testing.T) {
	acct := newFakeLoomyAccount(t, "s3cret", "u-100")
	svc, p, dir := loginTestService(t, acct)

	good := []byte(fmt.Sprintf(`{"uid":"u-200","loginid":"13900001111","password":"p",
		"accessToken":"s","expiresAt":%d,"nickname":"导入号"}`,
		time.Now().Add(24*time.Hour).Unix()))
	bad := []byte(`{"uid":"u-300"}`)
	imported, errs := svc.ImportAccounts([][]byte{good, bad})
	if len(imported) != 1 || len(errs) != 1 {
		t.Fatalf("imported=%d errs=%d", len(imported), len(errs))
	}
	if imported[0].UID != "u-200" || imported[0].Nickname != "导入号" {
		t.Fatalf("account=%+v", imported[0])
	}
	if _, ok := p.Status("u-200"); !ok {
		t.Fatal("导入账号应入池")
	}
	if auths, _ := auth.LoadLoomyDir(dir); len(auths) != 1 {
		t.Fatalf("落盘账号数 %d", len(auths))
	}
}
