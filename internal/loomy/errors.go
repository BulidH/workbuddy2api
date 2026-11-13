package loomy

import (
	"net/http"
	"strings"

	"workbuddy2api/internal/upstream"
)

// Classify 把 Loomy 上游错误映射到网关统一错误分类（复用冷却/熔断策略）。
//
// 语义依据：
//   - 401/403、业务码 020002 / 100002 → 会话失效（session_dead，需重新登录）；
//     但账号若存了密码，客户端会在请求链路内自动重登重试，走到这里说明重登也失败。
//   - 余额/积分不足（402 / insufficient points / quota exceeded）→ hard_credit，
//     隔天签到后自动恢复。
//   - 429 / 频率限制文案 → soft_rate。
//   - 其它 4xx → bad_params（请求体问题，换号无用但仍轮转）；5xx → server。
func Classify(status int, body string) upstream.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "insufficient") &&
		(strings.Contains(lower, "point") || strings.Contains(lower, "credit") || strings.Contains(lower, "balance")):
		return upstream.ErrHardCredit
	case strings.Contains(lower, "020002") || strings.Contains(lower, "100002"):
		return upstream.ErrSessionDead
	case status == http.StatusUnauthorized:
		return upstream.ErrSessionDead
	case status == http.StatusPaymentRequired:
		return upstream.ErrHardCredit
	case status == http.StatusForbidden:
		// 403 无 session 语义时（风控/模型权限）不永久禁用，走有界软冷却。
		return upstream.ErrSoftRate
	case status == http.StatusTooManyRequests:
		return upstream.ErrSoftRate
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate-limit") ||
		strings.Contains(lower, "too many requests") || strings.Contains(lower, "quota"):
		return upstream.ErrSoftRate
	case status == http.StatusNotFound:
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
