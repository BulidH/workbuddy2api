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
	}
	return ""
}
