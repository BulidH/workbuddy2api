package server

import (
	"math/rand"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// resolveRoute 解析模型名的 provider 路由协议：
//
//	qoder:<model>   → 强制 Qoder（realm 由账号决定）
//	cn:<model>      → 强制 WorkBuddy CN（现状）
//	global:<model>  → 强制 WorkBuddy global（现状）
//	裸名             → provider=""（合并调度，按两个目录归一化匹配）
//
// 前缀大小写敏感（与既有 cn:/global: 协议一致）。
func resolveRoute(model string) (provider, realm, bare string) {
	if idx := strings.IndexByte(model, ':'); idx >= 0 {
		switch prefix := model[:idx]; prefix {
		case auth.ProviderQoder:
			return auth.ProviderQoder, "", model[idx+1:]
		case "cn", "global":
			return auth.ProviderWorkBuddy, prefix, model[idx+1:]
		}
	}
	return "", "cn", model
}

// ResolveRoute 是 resolveRoute 的导出面（cmd/server 粘性闭包需要）。
func ResolveRoute(model string) (provider, realm, bare string) { return resolveRoute(model) }

// providerCandidates 返回本次请求可尝试的 Provider 顺序。
//
// 强制前缀只返回目标 Provider（无账号时由轮转循环回 503）；
// 裸名合并调度按权重随机排序，且只保留「支持该模型且存在健康账号」的 Provider。
func (h *Handler) providerCandidates(forced, bare string) []string {
	if forced == auth.ProviderQoder {
		return []string{auth.ProviderQoder}
	}
	if forced == auth.ProviderWorkBuddy {
		return []string{auth.ProviderWorkBuddy}
	}
	wbOK := h.cfg.Pool != nil && h.cfg.Pool.ServableNow()
	qOK := h.qoderEnabled() && h.cfg.QoderPool.ServableNow() && h.qoderSupports(bare)
	var out []string
	if wbOK {
		out = append(out, auth.ProviderWorkBuddy)
	}
	if qOK {
		out = append(out, auth.ProviderQoder)
	}
	if len(out) <= 1 {
		return out
	}
	// 加权随机决定先后：权重只影响优先概率，失败后仍会降级到另一 Provider。
	wbWeight := 1.0
	qWeight := h.cfg.QoderWeight
	if qWeight <= 0 {
		qWeight = 1.0
	}
	if rand.Float64()*float64(wbWeight+qWeight) >= wbWeight {
		out[0], out[1] = out[1], out[0]
	}
	return out
}

func (h *Handler) qoderEnabled() bool {
	return h.cfg.Qoder != nil && h.cfg.QoderPool != nil
}

// qoderSupports 报告 Qoder 目录（任一区域）是否认识该模型名。
func (h *Handler) qoderSupports(bare string) bool {
	if h.cfg.Qoder == nil {
		return false
	}
	return len(h.cfg.Qoder.Catalog.LookupAny(bare)) > 0
}

// poolFor 返回 Provider 对应的账号池（未知 Provider 回落 WorkBuddy 池）。
func (h *Handler) poolFor(provider string) *pool.Pool {
	if provider == auth.ProviderQoder {
		return h.cfg.QoderPool
	}
	return h.cfg.Pool
}

// stickyPool 定位粘性绑定号所属的池与 Provider；两池都不存在该 UID 时返回 nil。
func (h *Handler) stickyPool(uid string) (*pool.Pool, string) {
	if h.cfg.Pool != nil && h.cfg.Pool.AuthByUID(uid) != nil {
		return h.cfg.Pool, auth.ProviderWorkBuddy
	}
	if h.qoderEnabled() && h.cfg.QoderPool.AuthByUID(uid) != nil {
		return h.cfg.QoderPool, auth.ProviderQoder
	}
	return nil, ""
}
