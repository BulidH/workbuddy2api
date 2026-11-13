package loomy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// hexN 生成 n 个小写十六进制字符（n 为偶数时恰好 n/2 字节）。
func hexN(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n, n)
	}
	return hex.EncodeToString(b)[:n]
}

// newUUID 生成标准 UUIDv4 字符串。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
