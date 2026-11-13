// loomy.go 面板的 Loomy 运营入口：批量签到（含新手任务领取）与余额刷新。
//
// 与 Qoder 保持同一套交互形态（body.uids 可选、返回 outcomes + summary），
// 前端可以复用同一段渲染逻辑。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"workbuddy2api/internal/loomyops"
)

// loomyOpBody 签到/余额请求体（uid 与 uids 二选一）。
type loomyOpBody struct {
	UID  string   `json:"uid"`
	UIDs []string `json:"uids"`
}

func (b loomyOpBody) targets() []string {
	if len(b.UIDs) > 0 {
		return b.UIDs
	}
	if b.UID != "" {
		return []string{b.UID}
	}
	return nil
}

// handleLoomyCheckin 批量签到：每日积分刷新 + 新手任务领取 + 余额写回池。
func (p *Panel) handleLoomyCheckin(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomyCheckin == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	var body loomyOpBody
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	outcomes, err := p.deps.LoomyCheckin(ctx, body.targets())
	if err != nil {
		if errors.Is(err, loomyops.ErrBusy) {
			errJSON(w, http.StatusConflict, "Loomy 签到正在执行，请稍后重试")
			return
		}
		errJSON(w, http.StatusBadGateway, "Loomy 签到失败: "+err.Error())
		return
	}
	sum := map[string]int{"total": len(outcomes)}
	earned := 0
	for _, o := range outcomes {
		sum[o.Status]++
		earned += o.Earned
	}
	sum["earned_points"] = earned
	okJSON(w, map[string]any{"ok": true, "outcomes": outcomes, "summary": sum})
}

// handleLoomyBalance 批量刷新积分余额（只读）。
func (p *Panel) handleLoomyBalance(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomyBalance == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	var body loomyOpBody
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	outcomes, err := p.deps.LoomyBalance(ctx, body.targets())
	if err != nil {
		errJSON(w, http.StatusBadGateway, "Loomy 余额查询失败: "+err.Error())
		return
	}
	okN, failN := 0, 0
	for _, o := range outcomes {
		if o.Error != "" {
			failN++
		} else {
			okN++
		}
	}
	payload := map[string]any{
		"ok": true, "outcomes": outcomes,
		"summary": map[string]any{"total": len(outcomes), "ok": okN, "fail": failN},
	}
	if p.deps.LoomyLastCheckin != nil {
		if at, msg := p.deps.LoomyLastCheckin(); !at.IsZero() {
			payload["last_checkin_at"] = at.Format(time.RFC3339)
			payload["last_checkin_summary"] = msg
		}
	}
	okJSON(w, payload)
}

// reloadLoomy 重扫 Loomy 账号目录并对齐池。
func (p *Panel) reloadLoomy() (int, error) {
	if p.deps.ReloadLoomyAuths == nil {
		return 0, nil
	}
	return p.deps.ReloadLoomyAuths()
}
