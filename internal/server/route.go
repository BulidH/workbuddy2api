package server

import (
	"math/rand"
	"sort"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
)

// resolveRoute 解析模型名的 provider 路由协议：
//
//	qoder:<model>   → 强制 Qoder（realm 由账号决定）
//	loomy:<model>   → 强制 Loomy（恒 cn）
//	cn:<model>      → 强制 WorkBuddy CN（现状）
//	global:<model>  → 强制 WorkBuddy global（现状）
//	裸名             → provider=""（合并调度，按各上游目录归一化匹配）
//
// 前缀大小写敏感（与既有 cn:/global: 协议一致）。
func resolveRoute(model string) (provider, realm, bare string) {
	if idx := strings.IndexByte(model, ':'); idx >= 0 {
		switch prefix := model[:idx]; prefix {
		case auth.ProviderQoder:
			return auth.ProviderQoder, "", model[idx+1:]
		case auth.ProviderLoomy:
			return auth.ProviderLoomy, "cn", model[idx+1:]
		case "cn", "global":
			return auth.ProviderWorkBuddy, prefix, model[idx+1:]
		}
	}
	return "", "cn", model
}

// ResolveRoute 是 resolveRoute 的导出面（cmd/server 粘性闭包需要）。
func ResolveRoute(model string) (provider, realm, bare string) { return resolveRoute(model) }

// providerRank 一个候选 Provider 的调度档位。
type providerRank struct {
	provider string
	weight   float64
	// priority 数值大者先试；同优先级内按 weight 加权随机。
	priority int
}

// providerCandidates 返回本次请求可尝试的 Provider 顺序。
//
// 强制前缀只返回目标 Provider（无账号时由轮转循环回 503）；
// 裸名合并调度按「优先级档位 + 档内加权随机」排序，且只保留
// 「支持该模型且存在健康账号」的 Provider。前一个 Provider 的账号全部冷却/耗尽时，
// 轮转循环会继续尝试后面的 Provider——这就是「Loomy 用完再回落」的实现方式。
func (h *Handler) providerCandidates(forced, bare string) []string {
	switch forced {
	case auth.ProviderQoder:
		return []string{auth.ProviderQoder}
	case auth.ProviderLoomy:
		return []string{auth.ProviderLoomy}
	case auth.ProviderWorkBuddy:
		return []string{auth.ProviderWorkBuddy}
	}
	var ranks []providerRank
	if h.cfg.Pool != nil && h.cfg.Pool.ServableNow() {
		ranks = append(ranks, providerRank{auth.ProviderWorkBuddy, 1, h.cfg.WorkBuddyPriority})
	}
	if h.qoderEnabled() && h.cfg.QoderPool.ServableNow() && h.qoderSupports(bare) {
		w := h.cfg.QoderWeight
		if w <= 0 {
			w = 1.0
		}
		ranks = append(ranks, providerRank{auth.ProviderQoder, w, h.cfg.QoderPriority})
	}
	// LoomyPriority < 0 = 不参与裸名调度（仅 loomy: 前缀显式可用）。
	if h.cfg.LoomyPriority >= 0 && h.loomyEnabled() && h.cfg.LoomyPool.ServableNow() && h.loomySupports(bare) {
		w := h.cfg.LoomyWeight
		if w <= 0 {
			w = 1.0
		}
		ranks = append(ranks, providerRank{auth.ProviderLoomy, w, h.cfg.LoomyPriority})
	}
	out := orderByPriority(ranks)
	if len(out) == 0 {
		// 全部不可用：保留 WorkBuddy 兜底，让轮转循环给出统一的 503 文案
		// （而不是空列表被当成配置错误）。
		return []string{auth.ProviderWorkBuddy}
	}
	return out
}

// orderByPriority 按优先级降序分组，组内做加权随机排列。
//
// 分组而非全局排序：同一档位内的两个上游轮流分担流量，跨档位则严格有序——
// 「deepseek 优先走 Loomy，Loomy 用完才走其它」就是把 Loomy 放进更高一档。
func orderByPriority(ranks []providerRank) []string {
	if len(ranks) == 0 {
		return nil
	}
	sort.SliceStable(ranks, func(i, j int) bool { return ranks[i].priority > ranks[j].priority })
	out := make([]string, 0, len(ranks))
	for i := 0; i < len(ranks); {
		j := i
		for j < len(ranks) && ranks[j].priority == ranks[i].priority {
			j++
		}
		out = append(out, weightedOrder(ranks[i:j])...)
		i = j
	}
	return out
}

// weightedOrder 对同一档位的一组 Provider 做加权随机排序（不放回抽签）。
func weightedOrder(group []providerRank) []string {
	pool := append([]providerRank{}, group...)
	out := make([]string, 0, len(pool))
	for len(pool) > 0 {
		total := 0.0
		for _, r := range pool {
			total += r.weight
		}
		pick := 0
		if total > 0 && len(pool) > 1 {
			x := rand.Float64() * total
			acc := 0.0
			for i, r := range pool {
				acc += r.weight
				if x < acc {
					pick = i
					break
				}
			}
		}
		out = append(out, pool[pick].provider)
		pool = append(pool[:pick], pool[pick+1:]...)
	}
	return out
}

func (h *Handler) qoderEnabled() bool {
	return h.cfg.Qoder != nil && h.cfg.QoderPool != nil
}

func (h *Handler) loomyEnabled() bool {
	return h.cfg.Loomy != nil && h.cfg.LoomyPool != nil
}

// qoderSupports 报告 Qoder 目录（任一区域）是否认识该模型名。
// 配置了别名时以 Qoder 侧目标名为准。
func (h *Handler) qoderSupports(bare string) bool {
	if h.cfg.Qoder == nil {
		return false
	}
	if target := h.providerModel(auth.ProviderQoder, bare); target != bare {
		return len(h.cfg.Qoder.Catalog.LookupAny(target)) > 0
	}
	return len(h.cfg.Qoder.Catalog.LookupAny(bare)) > 0
}

// loomySupports 报告 Loomy 目录是否认识该模型名（命中别名时以 Loomy 侧目标名为准）。
func (h *Handler) loomySupports(bare string) bool {
	if h.cfg.Loomy == nil || h.cfg.Loomy.Catalog == nil {
		return false
	}
	target := h.providerModel(auth.ProviderLoomy, bare)
	_, ok := h.cfg.Loomy.Catalog.Lookup(target)
	return ok
}

// providerModel 返回该 Provider 出站时应使用的模型名：命中别名用目标名，
// 否则原样返回。别名让同名但字面不同的多个模型仍能按一个裸名混合调度
// （例如 deepseek 混合名同时映射到 WorkBuddy / Qoder / Loomy 三家的实际模型）。
func (h *Handler) providerModel(provider, bare string) string {
	if a, ok := qoder.LookupAlias(h.cfg.QoderAliases, bare); ok {
		if target := a.Target(provider); target != "" {
			return target
		}
	}
	return bare
}

// poolFor 返回 Provider 对应的账号池（未知 Provider 回落 WorkBuddy 池）。
func (h *Handler) poolFor(provider string) *pool.Pool {
	switch provider {
	case auth.ProviderQoder:
		return h.cfg.QoderPool
	case auth.ProviderLoomy:
		return h.cfg.LoomyPool
	}
	return h.cfg.Pool
}

// stickyPool 定位粘性绑定号所属的池与 Provider；各池都不存在该 UID 时返回 nil。
func (h *Handler) stickyPool(uid string) (*pool.Pool, string) {
	if h.cfg.Pool != nil && h.cfg.Pool.AuthByUID(uid) != nil {
		return h.cfg.Pool, auth.ProviderWorkBuddy
	}
	if h.qoderEnabled() && h.cfg.QoderPool.AuthByUID(uid) != nil {
		return h.cfg.QoderPool, auth.ProviderQoder
	}
	if h.loomyEnabled() && h.cfg.LoomyPool.AuthByUID(uid) != nil {
		return h.cfg.LoomyPool, auth.ProviderLoomy
	}
	return nil, ""
}
