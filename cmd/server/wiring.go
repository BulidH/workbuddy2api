package main

import (
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的闭包（WorkBuddy + Qoder）。
func realmAwareAvailableForModel(p *pool.Pool, qp *pool.Pool) func(model string) []string {
	return realmAwareAvailableForModel3(p, qp, nil)
}

// realmAwareAvailableForModel3 是带 Loomy 池的版本（lp 可为 nil = 未启用）。
//
// 粘性分配的模型名可能带 provider/realm 前缀（"global:gpt-5.4" / "cn:glm-5.2" /
// "qoder:DeepSeek-V4-Pro" / "loomy:deepseek-v4-flash-0731"）：必须按前缀剥出
// provider + realm + bareModel，再交给对应池的选号域过滤——否则裸名取池子全集，
// global 号会被粘性分配给 CN 前缀请求，Qoder/Loomy 号会被分配给 WorkBuddy 前缀请求
// （跨 Provider 泄漏）。
//
// 裸名 = 合并调度：返回各池可用 UID 的并集（Qoder/Loomy 侧按模型目录匹配，不做 realm 过滤）。
func realmAwareAvailableForModel3(p *pool.Pool, qp *pool.Pool, lp *pool.Pool) func(model string) []string {
	return func(model string) []string {
		provider, realm, bare := server.ResolveRoute(model)
		switch provider {
		case auth.ProviderQoder:
			if qp == nil {
				return nil
			}
			return qp.AvailableUIDsForModel(bare)
		case auth.ProviderLoomy:
			if lp == nil {
				return nil
			}
			return lp.AvailableUIDsForModel(bare)
		case auth.ProviderWorkBuddy:
			return p.AvailableUIDsForModelRealm(bare, realm)
		}
		out := p.AvailableUIDsForModelRealm(bare, realm)
		if qp != nil {
			out = append(out, qp.AvailableUIDsForModel(bare)...)
		}
		if lp != nil {
			out = append(out, lp.AvailableUIDsForModel(bare)...)
		}
		return out
	}
}
