package bridge

import (
	"crypto/rand"
	"encoding/hex"
)

func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	out := hex.EncodeToString(buf)
	if len(out) > n {
		out = out[:n]
	}
	return out
}
