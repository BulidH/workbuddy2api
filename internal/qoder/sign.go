package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// COSY 协议常量（对齐 qoder2api-hub / 官方 qodercli 1.1.75）。
const (
	defaultCosyVersion    = "1.1.75"
	defaultCosyClientType = "5"
	defaultCosyUA         = "undici"
	defaultUserType       = "personal_professional_trial"
	defaultClientIP       = "169.254.198.161"
)

// Qoder 自定义 Base64 变体（出站请求体编码）。
const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var (
	stdToCustom  [128]byte
	customToStd  [128]byte
	initAlphabet sync.Once
)

func initAlphabetTables() {
	initAlphabet.Do(func() {
		for i := 0; i < 64; i++ {
			stdToCustom[stdAlphabet[i]] = customAlphabet[i]
			customToStd[customAlphabet[i]] = stdAlphabet[i]
		}
		stdToCustom['='] = customPad
		customToStd[customPad] = '='
	})
}

// EncodeBody 把明文编码成 Qoder 自定义 Base64 变体。
// 规则：标准 base64 后按 尾/中/首 三段轮转，再逐字符映射到自定义字母表（'=' -> '$'）。
func EncodeBody(plain []byte) string {
	initAlphabetTables()
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, len(rearranged))
	for i := 0; i < len(rearranged); i++ {
		out[i] = stdToCustom[rearranged[i]]
	}
	return string(out)
}

// DecodeBody 是 EncodeBody 的逆运算（测试与调试用）。
func DecodeBody(encoded string) ([]byte, error) {
	initAlphabetTables()
	std := make([]byte, len(encoded))
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		if c >= 128 {
			return nil, fmt.Errorf("qoder: invalid encoded byte %q", c)
		}
		std[i] = customToStd[c]
	}
	n := len(std)
	a := n / 3
	original := string(std[n-a:]) + string(std[a:n-a]) + string(std[:a])
	return base64.StdEncoding.DecodeString(original)
}

// rsaPublicPEM 是官方客户端硬编码的 Qoder 服务端公钥（1024 位，协议固定）。
const rsaPublicPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var (
	serverPubOnce sync.Once
	serverPubKey  *rsa.PublicKey
	serverPubErr  error
)

func loadServerPublicKey() (*rsa.PublicKey, error) {
	serverPubOnce.Do(func() {
		block, _ := pem.Decode([]byte(rsaPublicPEM))
		if block == nil {
			serverPubErr = fmt.Errorf("qoder: decode public key pem")
			return
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			serverPubErr = err
			return
		}
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			serverPubErr = fmt.Errorf("qoder: public key is not RSA")
			return
		}
		serverPubKey = pub
	})
	return serverPubKey, serverPubErr
}

// rsaEncodeKey 用官方公钥包裹会话密钥（PKCS#1 v1.5）。
func rsaEncodeKey(tempKey []byte) ([]byte, error) {
	pub, err := loadServerPublicKey()
	if err != nil {
		return nil, err
	}
	return rsa.EncryptPKCS1v15(rand.Reader, pub, tempKey)
}

func pkcs7Pad(plain []byte, blockSize int) []byte {
	pad := blockSize - len(plain)%blockSize
	return append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func aesCBCEncrypt(key, iv, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}

// jsonSortedCompactStrings 生成键排序、无空白的紧凑 JSON（服务端签名字节与此强绑定）。
// 仅用于身份体与 COSY payload（值全部为字符串）。
func jsonSortedCompactStrings(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsonString(k))
		buf.WriteByte(':')
		buf.WriteString(jsonString(m[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// jsonString 与 Python json.dumps(ensure_ascii=False) 的字符串编码保持一致。
func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// Session 单账号的 COSY 签名会话。machine 系列字段按 uid 稳定派生，
// tempKey/cosyKey/info 在会话创建时生成，access token 轮换后整体重建。
type Session struct {
	UID          string
	Nickname     string
	AccessToken  string
	RefreshToken string
	UserType     string
	OrgID        string
	OrgName      string

	MachineID       string
	MachineToken    string
	MachineType     string
	MachineHostname string

	tempKey string
	CosyKey string
	Info    string
}

// NewSession 构建一个 COSY 会话。
func NewSession(uid, nickname, accessToken, refreshToken, userType, orgID, orgName string) (*Session, error) {
	initAlphabetTables()
	if userType == "" {
		userType = defaultUserType
	}
	s := &Session{
		UID:             uid,
		Nickname:        nickname,
		AccessToken:     accessToken,
		RefreshToken:    refreshToken,
		UserType:        userType,
		OrgID:           orgID,
		OrgName:         orgName,
		MachineID:       deriveID(uid, "machine"),
		MachineToken:    deriveMachineToken(uid),
		MachineType:     deriveMachineType(uid),
		MachineHostname: machineHostname(uid),
		tempKey:         randomHex(8), // 16 个十六进制字符，作为 AES-128 key/iv
	}
	rsaKey, err := rsaEncodeKey([]byte(s.tempKey))
	if err != nil {
		return nil, err
	}
	s.CosyKey = base64.StdEncoding.EncodeToString(rsaKey)
	identity := map[string]string{
		"name":                 nickname,
		"aid":                  uid,
		"uid":                  uid,
		"yx_uid":               "",
		"organization_id":      orgID,
		"organization_name":    orgName,
		"user_type":            userType,
		"security_oauth_token": accessToken,
		"refresh_token":        refreshToken,
	}
	encrypted, err := aesCBCEncrypt([]byte(s.tempKey), []byte(s.tempKey), jsonSortedCompactStrings(identity))
	if err != nil {
		return nil, err
	}
	s.Info = base64.StdEncoding.EncodeToString(encrypted)
	return s, nil
}

// Bearer 生成一次请求的 COSY 签名（签名覆盖 body 与去掉 /algo 前缀的 path）。
func (s *Session) Bearer(body, rawURL string) (payloadB64, date, bearer string) {
	u, err := url.Parse(rawURL)
	path := "/"
	if err == nil && u.Path != "" {
		path = u.Path
	}
	path = strings.TrimPrefix(path, "/algo")
	payload := map[string]string{
		"cosyVersion": cosyVersionFor(s),
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   newUUID4(),
		"version":     "v1",
	}
	payloadB64 = base64.StdEncoding.EncodeToString(jsonSortedCompactStrings(payload))
	date = strconv.FormatInt(time.Now().Unix(), 10)
	raw := strings.Join([]string{payloadB64, s.CosyKey, date, body, path}, "\n")
	sum := md5.Sum([]byte(raw))
	return payloadB64, date, "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:])
}

// cosyVersionFor 目前版本固定；保留函数便于后续按实例覆盖。
func cosyVersionFor(*Session) string { return defaultCosyVersion }

// Headers 构造一次推理/模型接口的完整 COSY 签名头。
func (s *Session) Headers(body, rawURL, modelKey string, sse bool, version string) map[string]string {
	if version == "" {
		version = defaultCosyVersion
	}
	u, _ := url.Parse(rawURL)
	path := "/"
	if u != nil && u.Path != "" {
		path = u.Path
	}
	path = strings.TrimPrefix(path, "/algo")
	payload := map[string]string{
		"cosyVersion": version,
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   newUUID4(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(jsonSortedCompactStrings(payload))
	date := strconv.FormatInt(time.Now().Unix(), 10)
	sum := md5.Sum([]byte(strings.Join([]string{payloadB64, s.CosyKey, date, body, path}, "\n")))
	bearer := "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:])

	accept := "application/json"
	if sse {
		accept = "text/event-stream"
	}
	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-business-product": "cli",
		"cosy-business-type":    "agent",
		"cosy-scene":            "assistant",
		"cosy-clienttype":       defaultCosyClientType,
		"cosy-date":             date,
		"cosy-user":             s.UID,
		"cosy-key":              s.CosyKey,
		"accept":                accept,
		"authorization":         bearer,
		"accept-encoding":       "identity",
		"cosy-version":          version,
		"cosy-machineid":        s.MachineID,
		"cosy-machinetoken":     s.MachineToken,
		"cosy-machinetype":      s.MachineType,
		"cosy-clientip":         defaultClientIP,
		"login-version":         "v2",
		"user-agent":            defaultCosyUA,
		"cosy-machineos":        defaultMachineOS(),
	}
	if sse {
		h["cache-control"] = "no-cache"
		h["cosy-machinehostname"] = s.MachineHostname
	}
	if modelKey != "" {
		h["x-model-key"] = modelKey
		h["x-model-source"] = "system"
	}
	return h
}

// SessionCache 账号 UID -> COSY 会话的进程级缓存；access token 变化即失效。
type SessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

type sessionEntry struct {
	token string
	sess  *Session
}

// NewSessionCache 构建空缓存。
func NewSessionCache() *SessionCache {
	return &SessionCache{entries: make(map[string]sessionEntry)}
}

// Get 取（或创建）账号对应的会话。
func (c *SessionCache) Get(uid, nickname, accessToken, refreshToken, userType, orgID, orgName string) (*Session, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("qoder: empty access token")
	}
	key := uid
	if key == "" {
		key = accessToken[:16]
	}
	c.mu.Lock()
	if hit, ok := c.entries[key]; ok && hit.token == accessToken {
		c.mu.Unlock()
		return hit.sess, nil
	}
	c.mu.Unlock()
	sess, err := NewSession(uid, nickname, accessToken, refreshToken, userType, orgID, orgName)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = sessionEntry{token: accessToken, sess: sess}
	c.mu.Unlock()
	return sess, nil
}

// Invalidate 清除账号会话（token 刷新后必须重建）。
func (c *SessionCache) Invalidate(uid string) {
	c.mu.Lock()
	delete(c.entries, uid)
	c.mu.Unlock()
}
