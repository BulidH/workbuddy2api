package loomy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// RSAEncrypt 用上游下发的 DER/SPKI 公钥做 RSA/PKCS#1 v1.5 加密，返回 base64 密文。
// 上游下发的是 1024 位公钥，Go 标准库直接可用。
func RSAEncrypt(pubkeyDERB64, plaintext string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(padBase64(pubkeyDERB64))
	if err != nil {
		return "", fmt.Errorf("loomy: decode public key: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		// 兼容裸 RSAPublicKey（PKCS#1）形态。
		rsaPub, err2 := x509.ParsePKCS1PublicKey(der)
		if err2 != nil {
			return "", fmt.Errorf("loomy: parse public key: %w", err)
		}
		return encryptWith(rsaPub, plaintext)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("loomy: public key is not RSA")
	}
	return encryptWith(rsaPub, plaintext)
}

func encryptWith(pub *rsa.PublicKey, plaintext string) (string, error) {
	msg := []byte(plaintext)
	size := pub.Size()
	if len(msg) > size-11 {
		return "", fmt.Errorf("loomy: password too long for RSA-%d", size*8)
	}
	// 手工 PKCS#1 v1.5 填充（与 iFlytek 客户端的实现逐字节一致）。
	ps := make([]byte, size-len(msg)-3)
	for {
		if _, err := rand.Read(ps); err != nil {
			return "", err
		}
		nonZero := true
		for _, b := range ps {
			if b == 0 {
				nonZero = false
				break
			}
		}
		if nonZero {
			break
		}
	}
	block := make([]byte, 0, size)
	block = append(block, 0x00, 0x02)
	block = append(block, ps...)
	block = append(block, 0x00)
	block = append(block, msg...)
	c := new(big.Int).SetBytes(block)
	e := big.NewInt(int64(pub.E))
	m := new(big.Int).Exp(c, e, pub.N)
	out := make([]byte, size)
	m.FillBytes(out)
	return base64.StdEncoding.EncodeToString(out), nil
}

// padBase64 补齐缺失的 base64 padding（上游有时省略）。
func padBase64(s string) string {
	if n := len(s) % 4; n != 0 {
		return s + strings.Repeat("=", 4-n)
	}
	return s
}
