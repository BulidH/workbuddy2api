package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// escape 按 RFC3986 逐段转义；与 Python 的 quote(safe="-_.~") 再补 !'()* 对齐。
func escape(s string) string {
	e := url.QueryEscape(s)
	// QueryEscape 把空格转成 '+'，RFC3986 要求 %20；并把 ~ 保留（Go 1.8+ 保留）。
	e = strings.ReplaceAll(e, "+", "%20")
	for raw, enc := range map[string]string{"!": "%21", "'": "%27", "(": "%28", ")": "%29", "*": "%2A"} {
		e = strings.ReplaceAll(e, raw, enc)
	}
	return e
}

func escapedPath(path string) string {
	p := path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg != "" {
			segs[i] = escape(seg)
		}
	}
	return strings.Join(segs, "/")
}

func contentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// StringToSign 构造账号服务的待签字符串（固定 9 行，最后两行为空串时也不可省略）。
func StringToSign(method, path, body, contentType, date, nonce string) string {
	return strings.Join([]string{
		strings.ToUpper(method),
		escapedPath(path),
		"", // escaped query（账号服务请求均无 query）
		contentMD5(body),
		contentType,
		date,
		nonce,
		"", // signed headers
		"", // canonicalized headers
	}, "\n")
}

// Sign 计算 base64(HMAC-SHA1(secret, stringToSign))。
func Sign(secret, stringToSign string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// NewNonce 生成一个 UUIDv4 形状的 nonce（账号服务要求）。
func NewNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("nonce-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// AccountHeaders 构造账号服务一次请求的完整头（含 Authorization/Date/Nonce/Content-MD5）。
func AccountHeaders(accessKeyID, accessKeySecret, method, path, body string, now time.Time) map[string]string {
	if now.IsZero() {
		now = time.Now()
	}
	const contentType = "application/json"
	date := now.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	nonce := NewNonce()
	sig := Sign(accessKeySecret, StringToSign(method, path, body, contentType, date, nonce))
	h := map[string]string{
		"Authorization": "account " + accessKeyID + ":" + sig,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  contentType,
		"Accept":        "application/json",
	}
	if md5 := contentMD5(body); md5 != "" {
		h["Content-MD5"] = md5
	}
	return h
}

// sortedKeys 返回 map 的排序键（测试/日志辅助）。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
