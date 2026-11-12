package main

import (
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的闭包。
//
// 粘性分配的模型名可能带 provider/realm 前缀（"global:gpt-5.4" / "cn:glm-5.2" /
// "qoder:DeepSeek-V4-Pro"）：必须按前缀剥出 provider + realm + bareModel，再交给
// 对应池的选号域过滤——否则裸名取池子全集，global 号会被粘性分配给 CN 前缀请求，
// Qoder 号会被分配给 WorkBuddy 前缀请求（跨 Provider 泄漏）。
//
// 裸名 = 合并调度：返回两池可用 UID 的并集（Qoder 侧按模型目录匹配，不做 realm 过滤）。
func realmAwareAvailableForModel(p *pool.Pool, qp *pool.Pool) func(model string) []string {
	return func(model string) []string {
		provider, realm, bare := server.ResolveRoute(model)
		switch provider {
		case auth.ProviderQoder:
			if qp == nil {
				return nil
			}
			return qp.AvailableUIDsForModel(bare)
		case auth.ProviderWorkBuddy:
			return p.AvailableUIDsForModelRealm(bare, realm)
		}
		out := p.AvailableUIDsForModelRealm(bare, realm)
		if qp != nil {
			out = append(out, qp.AvailableUIDsForModel(bare)...)
		}
		return out
	}
}
