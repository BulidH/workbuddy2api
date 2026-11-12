package qoder

import (
	"strings"

	"workbuddy2api/internal/upstream"
)

// Classify 把 Qoder 上游 HTTP 错误映射到网关统一的错误分类（复用冷却/熔断策略）。
func Classify(status int, body string) upstream.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == 401 || status == 403:
		return upstream.ErrSessionDead
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
