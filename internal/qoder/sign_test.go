package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// 黄金向量由 qoder2api-hub 的 Python 参考实现生成（qoder_sign.py），
// 用于锁定自定义 Base64 变体与 AES-128-CBC 的字节级一致性。
func TestEncodeBodyGolden(t *testing.T) {
	cases := []struct {
		plain []byte
		want  string
	}{
		{[]byte{}, ""},
		{[]byte("{}"), "$kwm"},
		{[]byte("hello world"), "YuHp$Hq&J(WPHFru"},
		{[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, "R_wlRf$$d&NBopnG__To_fpg"},
		{[]byte("模型目录"), "hQu(FDyGQQhQs*iQ"},
	}
	for _, c := range cases {
		if got := EncodeBody(c.plain); got != c.want {
			t.Errorf("EncodeBody(%x) = %q, want %q", c.plain, got, c.want)
		}
	}
}

func TestDecodeBodyRoundTrip(t *testing.T) {
	for _, plain := range []string{"", "{}", "hello world", "模型目录", strings.Repeat("qoder", 40)} {
		enc := EncodeBody([]byte(plain))
		got, err := DecodeBody(enc)
		if err != nil {
			t.Fatalf("DecodeBody(%q): %v", enc, err)
		}
		if string(got) != plain {
			t.Errorf("round trip %q -> %q", plain, got)
		}
	}
}

func TestAESCBCGolden(t *testing.T) {
	key := []byte("0123456789abcdef")
	cases := []struct {
		plain []byte
		want  string
	}{
		{[]byte(`{"a":1}`), "81d1d63cc7591e4417a201a35ae6f603"},
		{[]byte("hello world 1234"), "f5f7f98eb6941bd8ecfaa1a3e9ad9e3104760e0878b423e9b55df25240b068d7"},
		{[]byte(""), "ed47fee0545c3fa7dd070d44b86e98d9"},
	}
	for _, c := range cases {
		got, err := aesCBCEncrypt(key, key, c.plain)
		if err != nil {
			t.Fatalf("aesCBCEncrypt: %v", err)
		}
		if hex.EncodeToString(got) != c.want {
			t.Errorf("aesCBCEncrypt(%q) = %x, want %s", c.plain, got, c.want)
		}
	}
}

func TestJSONSortedCompactStrings(t *testing.T) {
	got := jsonSortedCompactStrings(map[string]string{"b": "1", "a": "x", "c": ""})
	want := `{"a":"x","b":"1","c":""}`
	if string(got) != want {
		t.Errorf("jsonSortedCompactStrings = %s, want %s", got, want)
	}
}

func TestFingerprintGolden(t *testing.T) {
	const uid = "user-12345678-90ab-cdef-1234-567890abcdef"
	if got, want := deriveID(uid, "machine"), "71e67de6-f459-4537-a042-b9497ea28d3c"; got != want {
		t.Errorf("deriveID = %q, want %q", got, want)
	}
	if got, want := deriveMachineToken(uid), "YvKEKa8ndgA9Zsit8ImkbrLPcZmc8uk3CQJmXFa_vr2"; got != want {
		t.Errorf("deriveMachineToken = %q, want %q", got, want)
	}
	if got, want := deriveMachineType(uid), "c8b60444413b486eaf"; got != want {
		t.Errorf("deriveMachineType = %q, want %q", got, want)
	}
	if got, want := machineHostname(uid), "DESKTOP-E9D2036"; got != want {
		t.Errorf("machineHostname = %q, want %q", got, want)
	}
}

// TestNewSessionWrapsKeyAndInfo 校验 RSA 包裹的会话密钥长度，以及 info 可被同一
// tempKey 解开并还原身份 JSON（服务端解包逻辑的本地镜像）。
func TestNewSessionWrapsKeyAndInfo(t *testing.T) {
	s, err := NewSession("uid-1", "nick", "access-token", "refresh-token",
		"personal_professional_trial", "org-1", "Org One")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	rawKey, err := base64.StdEncoding.DecodeString(s.CosyKey)
	if err != nil {
		t.Fatalf("decode cosy key: %v", err)
	}
	if len(rawKey) != 128 { // 1024-bit RSA
		t.Fatalf("cosy key length = %d, want 128", len(rawKey))
	}
	if len(s.tempKey) != 16 {
		t.Fatalf("tempKey length = %d, want 16", len(s.tempKey))
	}

	infoRaw, err := base64.StdEncoding.DecodeString(s.Info)
	if err != nil {
		t.Fatalf("decode info: %v", err)
	}
	block, err := aes.NewCipher([]byte(s.tempKey))
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	plain := make([]byte, len(infoRaw))
	cipher.NewCBCDecrypter(block, []byte(s.tempKey)).CryptBlocks(plain, infoRaw)
	pad := int(plain[len(plain)-1])
	plain = plain[:len(plain)-pad]

	for _, kv := range []string{`"uid":"uid-1"`, `"aid":"uid-1"`, `"name":"nick"`,
		`"security_oauth_token":"access-token"`, `"refresh_token":"refresh-token"`,
		`"organization_id":"org-1"`, `"organization_name":"Org One"`,
		`"user_type":"personal_professional_trial"`, `"yx_uid":""`} {
		if !bytes.Contains(plain, []byte(kv)) {
			t.Errorf("decrypted info missing %s: %s", kv, plain)
		}
	}
}

// TestBearerSignatureFormula 用返回的 payload/date 重算 md5，锁定「payload+\n+key+
// \n+date+\n+body+\n+path(去 /algo)」的签名格式。
func TestBearerSignatureFormula(t *testing.T) {
	s := &Session{UID: "u1", CosyKey: "RSAKEY", Info: "INFO"}
	body := `{"hello":"world"}`
	rawURL := "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation"

	payloadB64, date, bearer := s.Bearer(body, rawURL)
	raw := strings.Join([]string{payloadB64, s.CosyKey, date, body,
		"/api/v2/service/pro/sse/agent_chat_generation"}, "\n")
	sum := md5.Sum([]byte(raw))
	want := "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:])
	if bearer != want {
		t.Errorf("bearer mismatch\n got %s\nwant %s", bearer, want)
	}

	h := s.Headers(body, rawURL, "dmodel", true, "1.1.75")
	if h["x-model-key"] != "dmodel" || h["x-model-source"] != "system" {
		t.Errorf("model headers missing: %v", h)
	}
	if _, ok := h["cosy-machinehostname"]; !ok || h["cache-control"] != "no-cache" {
		t.Errorf("sse headers missing: %v", h)
	}
	if h["cosy-version"] != "1.1.75" {
		t.Errorf("cosy-version = %q", h["cosy-version"])
	}
}
