package qoder

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// retryAfterRe 匹配 Qoder 错误体里的「多久后可重试」字段。
//
// 实测 DeepSeek-V4-Pro 排队时上游回 HTTP 403，body 形如：
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"errorCode\\\":0,
//	 \\\"isQueued\\\":true,\\\"modelKey\\\":\\\"dmodel\\\",\\\"queueCount\\\":1,
//	 \\\"queueType\\\":\\\"p4\\\",\\\"retryAfterSeconds\\\":60,
//	 \\\"serviceAvailable\\\":true,\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\",\\\"waitTime\\\":60}\",
//	 \"subCode\":\"RESOURCE_LIMIT\"}"}
//
// 两个字段都用秒表达，取先出现者。该错误被 JSON 转义了多层——键名的引号前可能带
// 一个或多个反斜杠（实测 `\\\"retryAfterSeconds\\\":60`）——故正则容忍任意反斜杠。
var retryAfterRe = regexp.MustCompile(`(?:retryAfterSeconds|waitTime)\\*"?\s*:\s*(\d+)`)

// queueRule 排队/资源限流的 marker。命中即判 soft_rate（可重试的临时状态），
// **不论 HTTP 状态码**——Qoder 用 403 承载排队，若先按 403 判 session_dead 会
// 把健康账号一次性永久禁用（本规则存在的原因）。
var queueRule = []string{
	"resource_limit",
	"resource limit",
	"isqueued",
	"retryafterseconds",
	"queuecount",
	"waittime",
}

// Classify 把 Qoder 上游 HTTP 错误映射到网关统一的错误分类（复用冷却/熔断策略）。
//
// 判定顺序（自「可自愈」到「不可自愈」）：
//  1. 排队/资源限流（RESOURCE_LIMIT / isQueued / retryAfterSeconds / waitTime）→ soft_rate。
//     必须最先判：这些文案出现在 **403** 里，而 403 的兜底分类是 session_dead（永久禁用）。
//     实测「DeepSeek-V4-Pro 排队 60 秒」就是 403 + RESOURCE_LIMIT——按 session_dead 处理
//     会让一个 token 完全有效的账号被一次性永久禁用（用户看到的「12153 session dead」）。
//  2. 402 / 余额关键词 / billing daily count exceeded → hard_credit（等签到恢复）。
//  3. 401 → session_dead：凭证失效，重登前不可恢复。
//     注意 Qoder 的 401 也只在 token 真失效时出现；刷新失败另走连续计数（见 handler）。
//  4. 402 之外的其他 403：request illegal → account_fault（授权封禁，重登前不可恢复）；
//     其余（无 session 语义的 403，如风控/权限）→ client（只换号，不永久禁用）。
//  5. 429 / 限流文案 → soft_rate；404 → not_found；5xx → server；其余 4xx → bad_params/client。
func Classify(status int, body string) upstream.ErrKind {
	lower := strings.ToLower(body)
	if hitsAny(lower, queueRule) {
		return upstream.ErrSoftRate
	}
	switch {
	case status == 401:
		return upstream.ErrSessionDead
	case status == 403:
		if strings.Contains(lower, "request illegal") {
			return upstream.ErrAccountFault
		}
		// 无 session 语义的 403：不永久禁用（403 可能是风控/模型权限/地区限制）。
		// 归 soft_rate → 有界软冷却后重试：既不像 ErrClient 那样无限轮转，
		// 也不像 session_dead 那样一次就永久杀号。
		return upstream.ErrSoftRate
	case status == 402:
		return upstream.ErrHardCredit
	case strings.Contains(lower, "insufficient") && (strings.Contains(lower, "credit") || strings.Contains(lower, "balance")):
		return upstream.ErrHardCredit
	case strings.Contains(lower, "billing daily count exceeded"):
		return upstream.ErrHardCredit
	case status == 429:
		return upstream.ErrSoftRate
	case strings.Contains(lower, "6004") || strings.Contains(lower, "11140") ||
		strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate-limiting"):
		return upstream.ErrSoftRate
	case status == 404:
		return upstream.ErrNotFound
	case status >= 500:
		return upstream.ErrServer
	case status >= 400:
		if strings.Contains(lower, "invalid") || strings.Contains(lower, "required") ||
			strings.Contains(lower, "unmarshal") {
			return upstream.ErrBadParams
		}
		return upstream.ErrClient
	}
	return upstream.ErrNone
}

// ParseRetryAfter 解析 Qoder 排队错误里的「多久后可重试」，返回重试墙钟。
// Qoder 用秒（retryAfterSeconds / waitTime），不是 WorkBuddy 的「将在 … 重置」文案，
// 故单独解析；解析不到返回 ok=false（调用方回落到有界退避）。
// 上限不做约束：冷却封顶由 pool 的 soft_rate_max 统一负责。
func ParseRetryAfter(body string) (time.Time, bool) {
	m := retryAfterRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	sec, err := strconv.Atoi(strings.TrimSpace(m[1]))
	if err != nil || sec < 0 {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(sec) * time.Second), true
}

// hitsAny 报告 lower 是否包含任一 marker。
func hitsAny(lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
