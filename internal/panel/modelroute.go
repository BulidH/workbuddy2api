// modelroute.go 面板「混合路由」页：展示并解释每个混合模型（aliases 裸名）
// 的实际调度顺序，以及各上游当前的可用性。
//
// 为什么需要这个页面：混合调度对用户是不透明的——`deepseek-flash` 到底先走哪个
// 上游、某上游模型下架后会不会自动兜底，只能翻代码或看日志。本页把
// 「别名目标 + 显式 order + 全局 priority」三者合成后的最终顺序摊开，
// 并标注每个上游当前是否真的可用（目录是否认识该模型 + 池内是否可服务）。
//
// 本页只读：顺序配置的写入复用已有的 PUT /panel/api/config（深合并 + 校验 + 原子写）。
package panel

import "net/http"

// handleModelRoutes 返回各混合模型的路由预览（只读，不触发上游请求）。
func (p *Panel) handleModelRoutes(w http.ResponseWriter, r *http.Request) {
	if p.deps.ModelRoutes == nil {
		errJSON(w, http.StatusNotImplemented, "混合路由未接线（ModelRoutes 为空）")
		return
	}
	routes, err := p.deps.ModelRoutes()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取混合路由失败："+err.Error())
		return
	}
	okJSON(w, map[string]any{
		"ok":     true,
		"routes": routes,
		// 供前端提示：改完后需要重启才生效。
		"restart_needed": true,
	})
}
