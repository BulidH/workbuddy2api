package qoder

import "strings"

// ModelAlias 把一个对外模型名映射到两个上游各自认的名字。
//
// 典型场景：Qoder 叫 DeepSeek-Flash，WorkBuddy CN 叫 deepseek-v4.1-flash。
// 配置 `"aliases": {"deepseek-flash": {"workbuddy": "deepseek-v4.1-flash", "qoder": "DeepSeek-Flash"}}`
// 后，客户端只需请求裸名 `deepseek-flash`，网关按选中的 Provider 改写 outbound
// model，并在两侧都可用时做加权混合调度。
type ModelAlias struct {
	WorkBuddy string `json:"workbuddy,omitempty"`
	Qoder     string `json:"qoder,omitempty"`
	// Loomy Loomy 侧模型名（如 DeepSeek-V4.1-Flash）。
	Loomy string `json:"loomy,omitempty"`
	// Order 该裸名的 Provider 尝试顺序（如 ["loomy","workbuddy"]）。
	//
	// 为什么需要：默认顺序由各 Provider 的 priority 全局决定，对所有模型一视同仁；
	// 但不同模型在各上游的可用性/成本差异很大（某个上游的模型可能随时下架）。
	// Order 让调度顺序**按模型**覆盖全局 priority，未列出的 Provider 排在列出的之后。
	// 空 = 完全沿用全局 priority 语义（向后兼容）。
	//
	// 注意：Order 只能表达「每个上游一次」。需要同一上游多个模型、或跨上游任意
	// 混排时用 Steps（Steps 非空时 Order 被忽略）。
	Order []string `json:"order,omitempty"`
	// Steps 显式候选链：按顺序逐个尝试，每项是「哪个上游 + 哪个模型名」。
	//
	// 这是 Order 的完整版，解决「同一个上游要在不同位置出现多次」的场景，例如：
	//   [{"provider":"qoder","model":"Qwen3.8-Flash"},
	//    {"provider":"loomy","model":"spark-x2.6"},
	//    {"provider":"loomy","model":"qwen3.8-flash"}]   ← 兜底仍是 Loomy 的另一个模型
	// 空 = 回落 Order / 全局 priority。
	Steps []ModelStep `json:"steps,omitempty"`
}

// ModelStep 候选链的一环：上游 + 该上游的模型名。
// Model 为空时按 provider 字段/裸名解析（等价于旧的分上游别名行为）。
type ModelStep struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// LookupAlias 按归一化模型名查别名（大小写、连字符差异都视为同一个名字）。
// 未配置或归一化后为空时返回 false。
func LookupAlias(aliases map[string]ModelAlias, name string) (ModelAlias, bool) {
	key := NormalizeModelName(name)
	if key == "" || len(aliases) == 0 {
		return ModelAlias{}, false
	}
	for alias, target := range aliases {
		if NormalizeModelName(alias) == key {
			return target, true
		}
	}
	return ModelAlias{}, false
}

// Target 返回该 Provider 对应的上游模型名；未配置返回空串（调用方回落原名）。
func (a ModelAlias) Target(provider string) string {
	switch provider {
	case "qoder":
		return strings.TrimSpace(a.Qoder)
	case "workbuddy", "cn", "global":
		return strings.TrimSpace(a.WorkBuddy)
	case "loomy":
		return strings.TrimSpace(a.Loomy)
	}
	return ""
}
