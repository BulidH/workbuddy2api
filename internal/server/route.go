package server

import (
	"math/rand"
	"sort"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/panel"
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

// routeStep 候选链的一环：上游 + 该上游应使用的模型名。
//
// 为什么不再只是 provider 字符串：混合模型的候选可能包含同一上游的多个模型
// （如 Qoder 的 Qwen3.8-Flash 与 DeepSeek-Flash 各占一环），而每个上游在
// 不同环节要用不同模型名。Model 为空时回落该 provider 的别名目标/裸名。
type routeStep struct {
	provider string
	model    string // 出站模型名（已按别名/Steps 解析，空 = 用裸名）
}

// providerRank 一个候选 Provider 的调度档位。
type providerRank struct {
	provider string
	weight   float64
	// priority 数值大者先试；同优先级内按 weight 加权随机。
	priority int
}

// routeSteps 返回本次请求的有序候选链（provider + 模型名）。
//
// 优先级：Steps（显式候选链）> Order（每上游一次）> 全局 priority。
// 每一环都会做可用性过滤：池可服务 + 目录认识该模型名；被过滤掉的环不参与，
// 但仍会出现在面板预览里（带原因），便于用户排查「为什么没走它」。
func (h *Handler) routeSteps(forced, bare string) []routeStep {
	if forced != "" {
		return []routeStep{{provider: forced, model: h.providerModel(forced, bare)}}
	}
	alias, ok := qoder.LookupAlias(h.cfg.QoderAliases, bare)
	if ok && len(alias.Steps) > 0 {
		out := make([]routeStep, 0, len(alias.Steps))
		for _, s := range alias.Steps {
			p := strings.ToLower(strings.TrimSpace(s.Provider))
			if p == "" {
				continue
			}
			m := strings.TrimSpace(s.Model)
			if m == "" {
				m = h.providerModel(p, bare)
			}
			if !h.providerReady(p, bare, m) {
				continue
			}
			out = append(out, routeStep{provider: p, model: m})
		}
		if len(out) > 0 {
			return out
		}
	}
	return h.stepsFromRanks(bare, h.rawRanks(bare))
}

// stepsFromRanks 把按全局/Order 排好的 provider 顺序转成候选链。
func (h *Handler) stepsFromRanks(bare string, ranks []providerRank) []routeStep {
	ordered := h.orderedCandidates(bare, ranks)
	out := make([]routeStep, 0, len(ordered))
	for _, p := range ordered {
		out = append(out, routeStep{provider: p, model: h.providerModel(p, bare)})
	}
	return out
}

// providerReady 判断某上游能否承接（provider, model）这一环。
// bare 用于兜底判断（model 为空时按裸名查目录）。
func (h *Handler) providerReady(provider, bare, model string) bool {
	switch provider {
	case auth.ProviderQoder:
		if !h.qoderEnabled() || h.cfg.QoderPool == nil || !h.cfg.QoderPool.ServableNow() {
			return false
		}
		return h.qoderCatalogHas(model)
	case auth.ProviderLoomy:
		if !h.loomyEnabled() || h.cfg.LoomyPool == nil || !h.cfg.LoomyPool.ServableNow() {
			return false
		}
		return h.loomyCatalogHas(model)
	case auth.ProviderWorkBuddy:
		return h.cfg.Pool != nil && h.cfg.Pool.ServableNow()
	}
	return false
}

// qoderCatalogHas / loomyCatalogHas 按具体模型名查目录（不做别名二次解析）。
func (h *Handler) qoderCatalogHas(model string) bool {
	if h.cfg.Qoder == nil || h.cfg.Qoder.Catalog == nil {
		return false
	}
	return len(h.cfg.Qoder.Catalog.LookupAny(model)) > 0
}

func (h *Handler) loomyCatalogHas(model string) bool {
	if h.cfg.Loomy == nil || h.cfg.Loomy.Catalog == nil {
		return false
	}
	_, ok := h.cfg.Loomy.Catalog.Lookup(model)
	return ok
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
	out := h.orderedCandidates(bare, ranks)
	if len(out) == 0 {
		// 全部不可用：保留 WorkBuddy 兜底，让轮转循环给出统一的 503 文案
		// （而不是空列表被当成配置错误）。
		return []string{auth.ProviderWorkBuddy}
	}
	return out
}

// ModelRoutes 返回所有混合模型（aliases 裸名）的调度预览，供面板「混合路由」页。
//
// 只读：不发起任何上游请求，只做目录查询与池状态判断。输出顺序即真实尝试顺序，
// 让用户能直接看出「先走谁、某上游不可用时兜底是谁」。
func (h *Handler) ModelRoutes() []panel.ModelRouteView {
	names := make([]string, 0, len(h.cfg.QoderAliases))
	for name := range h.cfg.QoderAliases {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]panel.ModelRouteView, 0, len(names))
	for _, name := range names {
		alias := h.cfg.QoderAliases[name]
		view := panel.ModelRouteView{
			Name:    name,
			Targets: map[string]string{},
			Order:   alias.Order,
			Steps:   alias.Steps,
		}
		for _, p := range []string{auth.ProviderWorkBuddy, auth.ProviderQoder, auth.ProviderLoomy} {
			if t := alias.Target(p); t != "" {
				view.Targets[p] = t
			}
		}

		if len(alias.Steps) > 0 {
			// 显式候选链：按配置顺序逐环展示，被过滤掉的环也列出并说明原因。
			for i, s := range alias.Steps {
				p := strings.ToLower(strings.TrimSpace(s.Provider))
				if p == "" {
					continue
				}
				m := strings.TrimSpace(s.Model)
				if m == "" {
					m = h.providerModel(p, name)
				}
				avail := h.providerReady(p, name, m)
				oc := panel.ModelRouteCandidate{
					Provider:        p,
					ConfiguredOrder: i,
					Available:       avail,
					Model:           m,
				}
				if !avail {
					oc.Reason = h.unavailableReasonFor(p, name, m)
				}
				view.Candidates = append(view.Candidates, oc)
			}
			out = append(out, view)
			continue
		}

		// 无 Steps：按 Order / 全局 priority 合成顺序。
		ranks := h.rawRanks(name)
		ordered := h.orderedCandidates(name, ranks)
		available := map[string]bool{}
		for _, r := range ranks {
			available[r.provider] = true
		}
		ordPos := map[string]int{}
		for i, p := range alias.Order {
			ordPos[strings.ToLower(strings.TrimSpace(p))] = i
		}
		for _, p := range ordered {
			oc := panel.ModelRouteCandidate{
				Provider:        p,
				ConfiguredOrder: -1,
				Available:       available[p],
				Model:           h.providerModel(p, name),
			}
			if i, ok := ordPos[p]; ok {
				oc.ConfiguredOrder = i
			}
			if !oc.Available {
				oc.Reason = h.unavailableReason(p, name)
			}
			view.Candidates = append(view.Candidates, oc)
		}
		// 不可用但配置过的上游也列出来（否则用户看不到「为什么它没生效」）。
		for _, p := range alias.Order {
			key := strings.ToLower(strings.TrimSpace(p))
			if key == "" || available[key] {
				continue
			}
			view.Candidates = append(view.Candidates, panel.ModelRouteCandidate{
				Provider:        key,
				ConfiguredOrder: ordPos[key],
				Available:       false,
				Reason:          h.unavailableReason(key, name),
				Model:           h.providerModel(key, name),
			})
		}
		out = append(out, view)
	}
	return out
}

// unavailableReasonFor 说明「上游 + 具体模型」这一环为何不可用（Steps 展示用）。
func (h *Handler) unavailableReasonFor(provider, bare, model string) string {
	switch provider {
	case auth.ProviderQoder:
		if !h.qoderEnabled() {
			return "Qoder 未启用"
		}
		if h.cfg.QoderPool == nil || !h.cfg.QoderPool.ServableNow() {
			return "Qoder 账号池当前无可服务账号"
		}
		if !h.qoderCatalogHas(model) {
			return "Qoder 模型目录不认识 " + model + "（模型可能已下架）"
		}
	case auth.ProviderLoomy:
		if !h.loomyEnabled() {
			return "Loomy 未启用"
		}
		if h.cfg.LoomyPool == nil || !h.cfg.LoomyPool.ServableNow() {
			return "Loomy 账号池当前无可服务账号"
		}
		if !h.loomyCatalogHas(model) {
			return "Loomy 模型目录不认识 " + model + "（模型可能已下架）"
		}
	case auth.ProviderWorkBuddy:
		if h.cfg.Pool == nil || !h.cfg.Pool.ServableNow() {
			return "WorkBuddy 账号池当前无可服务账号"
		}
	}
	return "当前不可用"
}

// rawRanks 构造「不考虑顺序覆盖」的候选集（含可用性过滤），与 providerCandidates
// 的过滤逻辑保持一致，但不排序——排序交给调用方。
func (h *Handler) rawRanks(bare string) []providerRank {
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
	if h.cfg.LoomyPriority >= 0 && h.loomyEnabled() && h.cfg.LoomyPool.ServableNow() && h.loomySupports(bare) {
		w := h.cfg.LoomyWeight
		if w <= 0 {
			w = 1.0
		}
		ranks = append(ranks, providerRank{auth.ProviderLoomy, w, h.cfg.LoomyPriority})
	}
	return ranks
}

// unavailableReason 说明某上游为何无法承接该裸名（面板展示用）。
func (h *Handler) unavailableReason(provider, bare string) string {
	switch provider {
	case auth.ProviderQoder:
		if !h.qoderEnabled() {
			return "Qoder 未启用"
		}
		if h.cfg.QoderPool == nil || !h.cfg.QoderPool.ServableNow() {
			return "Qoder 账号池当前无可服务账号（全部冷却/禁用/占满）"
		}
		if !h.qoderSupports(bare) {
			return "Qoder 模型目录不认识 " + h.providerModel(provider, bare) + "（模型可能已下架）"
		}
	case auth.ProviderLoomy:
		if !h.loomyEnabled() {
			return "Loomy 未启用"
		}
		if h.cfg.LoomyPriority < 0 {
			return "loomy.priority < 0：该上游不参与裸名混合调度"
		}
		if h.cfg.LoomyPool == nil || !h.cfg.LoomyPool.ServableNow() {
			return "Loomy 账号池当前无可服务账号（全部冷却/禁用/占满）"
		}
		if !h.loomySupports(bare) {
			return "Loomy 模型目录不认识 " + h.providerModel(provider, bare) + "（模型可能已下架）"
		}
	case auth.ProviderWorkBuddy:
		if h.cfg.Pool == nil || !h.cfg.Pool.ServableNow() {
			return "WorkBuddy 账号池当前无可服务账号"
		}
	}
	return "当前不可用"
}

// orderedCandidates 决定候选 Provider 的尝试顺序。
//
// 默认走全局 priority（orderByPriority）；若该裸名在 aliases 里显式配了 order，
// 则按 order 的先后严格排序——这是「按模型覆盖全局调度顺序」的入口，
// 用于某个上游的模型随时可能下架/变贵的场景：
//   - order 里列出的 Provider 按列表顺序优先尝试；
//   - 未列在 order 里的候选排在其后，仍按全局 priority 排；
//   - order 里出现但本轮不可用的 Provider 自然缺席（不进 ranks 就无从排序）。
func (h *Handler) orderedCandidates(bare string, ranks []providerRank) []string {
	if len(ranks) == 0 {
		return nil
	}
	alias, ok := qoder.LookupAlias(h.cfg.QoderAliases, bare)
	if !ok || len(alias.Order) == 0 {
		return orderByPriority(ranks)
	}
	// order 中的位置 → 序号；值越小越先试。
	pos := make(map[string]int, len(alias.Order))
	for i, p := range alias.Order {
		name := strings.ToLower(strings.TrimSpace(p))
		if name == "" {
			continue
		}
		if _, exists := pos[name]; !exists {
			pos[name] = i
		}
	}
	if len(pos) == 0 {
		return orderByPriority(ranks)
	}
	explicit := make([]providerRank, 0, len(ranks))
	rest := make([]providerRank, 0, len(ranks))
	for _, r := range ranks {
		if _, ok := pos[r.provider]; ok {
			explicit = append(explicit, r)
		} else {
			rest = append(rest, r)
		}
	}
	sort.SliceStable(explicit, func(i, j int) bool {
		return pos[explicit[i].provider] < pos[explicit[j].provider]
	})
	out := make([]string, 0, len(ranks))
	for _, r := range explicit {
		out = append(out, r.provider)
	}
	return append(out, orderByPriority(rest)...)
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
