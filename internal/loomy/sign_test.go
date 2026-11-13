package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// TestStringToSignShape 待签串固定 9 行（最后两行空串也不能省）——这是账号服务
// 校验签名时最容易踩的坑，所以单独钉住形状。
func TestStringToSignShape(t *testing.T) {
	sts := StringToSign("post", "/api/v1/getPuKey", `{"a":1}`, "application/json",
		"Mon, 02 Jan 2006 15:04:05 GMT", "nonce-1")
	lines := strings.Split(sts, "\n")
	if len(lines) != 9 {
		t.Fatalf("stringToSign 应为 9 行，实得 %d 行: %q", len(lines), sts)
	}
	if lines[0] != "POST" {
		t.Errorf("method 应大写，得 %q", lines[0])
	}
	if lines[1] != "/api/v1/getPuKey" {
		t.Errorf("path=%q", lines[1])
	}
	if lines[2] != "" {
		t.Errorf("query 应空，得 %q", lines[2])
	}
	sum := md5.Sum([]byte(`{"a":1}`))
	if lines[3] != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Errorf("content-md5 不符: %q", lines[3])
	}
	if lines[7] != "" || lines[8] != "" {
		t.Errorf("signed/canonicalized headers 应为空: %q %q", lines[7], lines[8])
	}
}

// TestStringToSignEmptyBody 空 body 的 Content-MD5 必须是空串（不是 MD5("")）。
func TestStringToSignEmptyBody(t *testing.T) {
	sts := StringToSign("GET", "/x", "", "application/json", "D", "N")
	lines := strings.Split(sts, "\n")
	if lines[3] != "" {
		t.Errorf("空 body 的 Content-MD5 应为空，得 %q", lines[3])
	}
}

// TestSignKnownVector 与 Python 参考实现（loomy2api/signer.py）逐字节一致。
// 期望值由 Python 侧同样的输入算出，改动签名逻辑会立刻红。
func TestSignKnownVector(t *testing.T) {
	sts := StringToSign("POST", "/api/v1/getPuKey", `{"base":{"appid":"GM3LOOMY"}}`,
		"application/json", "Thu, 13 Nov 2026 00:00:00 GMT", "0f8fad5b-d9cb-469f-a165-70867728950e")
	got := Sign("zsak6eadrbawz683wf5r3m2snrwj868r", sts)
	mac := hmac.New(sha1.New, []byte("zsak6eadrbawz683wf5r3m2snrwj868r"))
	mac.Write([]byte(sts))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("sign=%q want %q", got, want)
	}
	if len(got) != 28 { // base64(20 字节 SHA1) = 28 字符
		t.Errorf("签名长度应为 28，得 %d", len(got))
	}
}

// TestEscapedPath 路径分段转义：保留斜杠、去掉尾斜杠、补前导斜杠。
func TestEscapedPath(t *testing.T) {
	cases := map[string]string{
		"/api/v1/chat": "/api/v1/chat",
		"api/v1/chat/": "/api/v1/chat",
		"/a b/c":       "/a%20b/c",
		"/api/(v1)/x":  "/api/%28v1%29/x",
		"/":            "/",
	}
	for in, want := range cases {
		if got := escapedPath(in); got != want {
			t.Errorf("escapedPath(%q)=%q want %q", in, got, want)
		}
	}
}

// TestAccountHeaders 头的形状：Authorization 前缀、Date 格式、Nonce、有 body 才带 Content-MD5。
func TestAccountHeaders(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	h := AccountHeaders("ak", "sk", "POST", "/api/v1/getPuKey", `{"x":1}`, now)
	if !strings.HasPrefix(h["Authorization"], "account ak:") {
		t.Errorf("Authorization=%q", h["Authorization"])
	}
	if h["Date"] != "Fri, 13 Nov 2026 00:00:00 GMT" {
		t.Errorf("Date=%q", h["Date"])
	}
	if h["Nonce"] == "" {
		t.Error("Nonce 不能为空")
	}
	if h["Content-MD5"] == "" {
		t.Error("非空 body 必须带 Content-MD5")
	}
	if h2 := AccountHeaders("ak", "sk", "GET", "/models", "", now); h2["Content-MD5"] != "" {
		t.Error("空 body 不应带 Content-MD5")
	}
}

// TestRSAEncryptRoundTrip 用本地生成的密钥加密后用私钥解密，验证 PKCS#1 v1.5
// 填充结构（0x00 0x02 PS 0x00 msg）能被标准库解开。
func TestRSAEncryptRoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(der)
	const secret = "p@ssw0rd-中文"
	enc, err := RSAEncrypt(b64, secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != key.Size() {
		t.Fatalf("密文长度 %d != 模长 %d", len(raw), key.Size())
	}
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != secret {
		t.Fatalf("解出 %q want %q", plain, secret)
	}
}

// TestRSAEncryptPKCS1Key 兼容裸 PKCS#1 公钥（部分下发形态）。
func TestRSAEncryptPKCS1Key(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	enc, err := RSAEncrypt(base64.StdEncoding.EncodeToString(der), "x")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	if _, err := rsa.DecryptPKCS1v15(rand.Reader, key, raw); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
}

// TestRSAEncryptBadKey 非法公钥要报错而不是静默返回空串。
func TestRSAEncryptBadKey(t *testing.T) {
	if _, err := RSAEncrypt("bm90LWEta2V5", "x"); err == nil {
		t.Fatal("非法公钥应当报错")
	}
}

// TestPadBase64 缺失 padding 时补齐（上游偶有省略）。
func TestPadBase64(t *testing.T) {
	if got := padBase64("YWI"); got != "YWI=" {
		t.Errorf("padBase64=%q", got)
	}
	if got := padBase64("YWJj"); got != "YWJj" {
		t.Errorf("已是 4 倍数不应改: %q", got)
	}
}

// TestPEMCompatibility 上游也可能下发 PEM 包装的公钥（含头尾行）——确认返回明确错误
// 而不是 panic（RSAEncrypt 只接受裸 base64 DER）。
func TestPEMCompatibility(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if _, err := RSAEncrypt(pemText, "x"); err == nil {
		t.Fatal("PEM 文本应当解析失败")
	}
}
