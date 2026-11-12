package qoder

import (
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// 真实 403 排队响应（DeepSeek-V4-Pro 排队 60s，token 完全有效）。
const realQueue403 = `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"errorCode\\\":0,\"isQueued\\\":true,\"modelKey\\\":\\\"dmodel\\\",\\\"queueCount\\\":1,\\\"queueType\\\":\\\"p4\\\",\\\"retryAfterSeconds\\\":60,\\\"serviceAvailable\\\":true,\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\",\\\"waitTime\\\":60}\",\"subCode\":\"RESOURCE_LIMIT\"}"}`

// TestClassifyQueue403IsSoftRate 回归（线上事故）：Qoder 把「排队 60 秒 / 资源限流」
// 包在 HTTP 403 里。旧实现「401/403 → session_dead」会把健康账号一次性永久禁用
// （面板显示「12153 session dead」，实际 token 仍有效、userinfo 200）。
// 排队是可自愈的临时状态，必须归 soft_rate。
func TestClassifyQueue403IsSoftRate(t *testing.T) {
	if got := Classify(403, realQueue403); got != upstream.ErrSoftRate {
		t.Errorf("403 queue body -> %v want ErrSoftRate", got)
	}
	if got := Classify(400, realQueue403); got != upstream.ErrSoftRate {
		t.Errorf("queue marker 不应依赖状态码，400 -> %v want ErrSoftRate", got)
	}
}

// TestClassifyUnauthorizedIsSessionDead 401 仍是凭证失效（重登前不可恢复）。
func TestClassifyUnauthorizedIsSessionDead(t *testing.T) {
	if got := Classify(401, `{"code":"401","message":"unauthorized"}`); got != upstream.ErrSessionDead {
		t.Errorf("401 -> %v want ErrSessionDead", got)
	}
}

// TestClassifyBare403IsNotSessionDead 无 session 语义的 403 不得永久禁用账号：
// 403 可能是风控/模型权限/地区限制，一次就杀掉号代价过高。
func TestClassifyBare403IsNotSessionDead(t *testing.T) {
	if got := Classify(403, `{"code":"403","message":"forbidden"}`); got == upstream.ErrSessionDead {
		t.Errorf("bare 403 -> ErrSessionDead；403 不等于凭证失效")
	}
	if got := Classify(403, `{"code":"403","message":"forbidden"}`); got != upstream.ErrSoftRate {
		t.Errorf("bare 403 -> %v want ErrSoftRate（软冷却后重试，不永久禁用）", got)
	}
}

// TestClassifyRequestIllegal403IsAccountFault request illegal 是授权封禁，走账号故障。
func TestClassifyRequestIllegal403IsAccountFault(t *testing.T) {
	if got := Classify(403, `{"code":"403","message":"request illegal"}`); got != upstream.ErrAccountFault {
		t.Errorf("403 request illegal -> %v want ErrAccountFault", got)
	}
}

// TestClassifyCreditIsHardCredit 余额耗尽仍然最优先判。
func TestClassifyCreditIsHardCredit(t *testing.T) {
	if got := Classify(402, `{"code":"402","message":"payment required"}`); got != upstream.ErrHardCredit {
		t.Errorf("402 -> %v want ErrHardCredit", got)
	}
	if got := Classify(400, `insufficient credits`); got != upstream.ErrHardCredit {
		t.Errorf("insufficient credits -> %v want ErrHardCredit", got)
	}
}

// TestParseRetryAfterSeconds 从排队错误体里取出重试墙钟（秒）。
func TestParseRetryAfterSeconds(t *testing.T) {
	now := time.Now()
	at, ok := ParseRetryAfter(realQueue403)
	if !ok {
		t.Fatal("ParseRetryAfter(realQueue403) ok=false")
	}
	d := at.Sub(now)
	if d < 55*time.Second || d > 65*time.Second {
		t.Errorf("retryAt 距现在 %v want ~60s", d)
	}
	if _, ok := ParseRetryAfter(`{"message":"no timing here"}`); ok {
		t.Error("无 retryAfterSeconds/waitTime 时应 ok=false")
	}
}

// TestParseRetryAfterZero 0 秒是合法值（上游说立即可重试），不得当成解析失败。
func TestParseRetryAfterZero(t *testing.T) {
	if _, ok := ParseRetryAfter(`{"retryAfterSeconds":0}`); !ok {
		t.Error("retryAfterSeconds:0 应解析成功")
	}
}
