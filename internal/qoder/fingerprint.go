// Package qoder 是 Qoder 网关协议的最小 Go 实现：设备指纹派生、COSY 签名、
// 模型目录与 OpenAI Chat Completions 上游调用。
package qoder

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
)

// deriveID 由 uid+salt 稳定派生带连字符的 UUIDv4 形状标识（对齐 qoder_fingerprint.py）。
// 同一账号永远得到同一设备指纹，避免上游按机器码聚类风控。
func deriveID(uid, salt string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte(salt + ":" + uid))
	h := hex.EncodeToString(sum[:])
	h = h[:12] + "4" + h[13:16] + "a" + h[17:]
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// deriveMachineType 稳定派生 18 位去横线的 machine_type。
func deriveMachineType(uid string) string {
	return strings.ReplaceAll(deriveID(uid, "machinetype"), "-", "")[:18]
}

// deriveMachineToken 稳定派生 machine_token（base64url 外观，43 字符）。
func deriveMachineToken(uid string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + uid))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "=")[:43]
}

// machineHostname 稳定派生主机名（形如 DESKTOP-1A2B3C4）。
func machineHostname(uid string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte("hostname:" + uid))
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	return "DESKTOP-" + h[:7]
}

// defaultMachineOS 与官方客户端同形的 {arch}_{platform}；默认保持 x86_64_linux/win32，
// 避免 ARM/macOS 账号指纹漂移（与 qoder2api-hub 默认口径一致）。
func defaultMachineOS() string {
	if runtime.GOOS == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}

// newUUID4 生成标准 UUIDv4 字符串。
func newUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-a000-%012d", 0)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomHex 返回 n 字节的十六进制串（用于 COSY 会话密钥）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
