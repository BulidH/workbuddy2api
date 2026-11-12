// agents.go 面板的「智能体配置」页：本机客户端探测、一键接入/预览/还原。
//
// 安全边界：这是本机运维功能，服务只改 state 目录与各客户端自己的配置目录；
// API Key 由用户在面板输入，只写进客户端本地文件，state 里仅存 sha256。
package panel

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/agentcfg"
)

type agentBody struct {
	Client  string                `json:"client"`
	BaseURL string                `json:"base_url"`
	APIKey  string                `json:"api_key"`
	Model   string                `json:"model"`
	Models  []agentcfg.ModelEntry `json:"models"`
}

func (p *Panel) agentService(w http.ResponseWriter) (*agentcfg.Service, bool) {
	if p.deps.Agents == nil {
		errJSON(w, http.StatusNotImplemented, "智能体配置未启用")
		return nil, false
	}
	return p.deps.Agents, true
}

func decodeAgentBody(w http.ResponseWriter, r *http.Request) (agentBody, bool) {
	var body agentBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return body, false
	}
	if body.BaseURL == "" {
		errJSON(w, http.StatusBadRequest, "base_url 不能为空")
		return body, false
	}
	return body, true
}

func (p *Panel) handleAgentsOverview(w http.ResponseWriter, r *http.Request) {
	svc, ok := p.agentService(w)
	if !ok {
		return
	}
	okJSON(w, svc.Overview())
}

func (p *Panel) handleAgentsPlan(w http.ResponseWriter, r *http.Request) {
	svc, ok := p.agentService(w)
	if !ok {
		return
	}
	body, ok := decodeAgentBody(w, r)
	if !ok {
		return
	}
	if body.APIKey == "" {
		errJSON(w, http.StatusBadRequest, "api_key 不能为空")
		return
	}
	plan, err := svc.Plan(body.Client, body.BaseURL, body.APIKey, body.Model, body.Models)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "plan": plan})
}

func (p *Panel) handleAgentsApply(w http.ResponseWriter, r *http.Request) {
	svc, ok := p.agentService(w)
	if !ok {
		return
	}
	body, ok := decodeAgentBody(w, r)
	if !ok {
		return
	}
	if body.APIKey == "" {
		errJSON(w, http.StatusBadRequest, "api_key 不能为空")
		return
	}
	res, err := svc.Integrate(body.Client, body.BaseURL, body.APIKey, body.Model, body.Models)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "result": res})
}

func (p *Panel) handleAgentsRestore(w http.ResponseWriter, r *http.Request) {
	svc, ok := p.agentService(w)
	if !ok {
		return
	}
	var body struct {
		Client string `json:"client"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	res, err := svc.Restore(body.Client)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "result": res})
}

func (p *Panel) handleAgentsTemplate(w http.ResponseWriter, r *http.Request) {
	svc, ok := p.agentService(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	base := q.Get("base_url")
	if base == "" {
		errJSON(w, http.StatusBadRequest, "base_url 不能为空")
		return
	}
	plan, err := svc.Template(q.Get("client"), base, q.Get("model"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "plan": plan})
}
