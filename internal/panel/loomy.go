// loomy.go 面板的 Loomy 运营入口：批量签到（含新手任务领取）与余额刷新。
//
// 与 Qoder 保持同一套交互形态（body.uids 可选、返回 outcomes + summary），
// 前端可以复用同一段渲染逻辑。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
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

// handleLoomyLogin 手机号 + 密码登录并加入账号池（面板「添加账号 · Loomy」）。
//
// Loomy 没有 refresh token：账号文件必须同时存手机号与密码（0600），14 天后的
// 自动续期就是靠这对凭证重新登录，所以这里不接受只给 session 的「无密码接入」。
func (p *Panel) handleLoomyLogin(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomyAddAccount == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	var body struct {
		LoginID  string `json:"loginid"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	body.LoginID = strings.TrimSpace(body.LoginID)
	body.Password = strings.TrimSpace(body.Password)
	if body.LoginID == "" || body.Password == "" {
		errJSON(w, http.StatusBadRequest, "手机号与密码都不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	a, err := p.deps.LoomyAddAccount(ctx, body.LoginID, body.Password, body.Nickname)
	if err != nil {
		// 登录失败最常见的是密码错/风控，原样带回上游文案便于自查。
		errJSON(w, http.StatusBadRequest, "登录失败: "+err.Error())
		return
	}
	n, _ := p.reloadLoomy()
	okJSON(w, map[string]any{"ok": true, "account": loomyAccountView(a), "accounts": n})
}

// handleLoomyImport 粘贴/上传 loomy-*.json（单个对象、数组或 {"accounts":[...]}）。
func (p *Panel) handleLoomyImport(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomyImportAccounts == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	items, err := splitAccountPayloads(raw)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(items) == 0 {
		errJSON(w, http.StatusBadRequest, "未提供账号数据")
		return
	}
	imported, errs := p.deps.LoomyImportAccounts(items)
	n, _ := p.reloadLoomy()
	views := make([]map[string]any, 0, len(imported))
	for _, a := range imported {
		views = append(views, loomyAccountView(a))
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	okJSON(w, map[string]any{
		"ok":       len(imported) > 0,
		"imported": views,
		"errors":   msgs,
		"accounts": n,
	})
}

// loomyAccountView 面板展示用的账号摘要（不含密码等敏感字段）。
func loomyAccountView(a *auth.Auth) map[string]any {
	if a == nil {
		return map[string]any{}
	}
	// 先在本包内取快照：Realm()/CreditsRemain() 自己会加同一把锁，不能持有 a.mu 再调。
	a.Lock()
	uid, nickname, loginID, expiresAt := a.UID, a.Nickname, a.LoginID, a.ExpiresAt
	a.Unlock()
	return map[string]any{
		"uid":        uid,
		"nickname":   nickname,
		"loginid":    maskLoginID(loginID),
		"provider":   auth.ProviderLoomy,
		"realm":      a.Realm(),
		"expires_at": expiresAt,
		"credits":    a.CreditsRemain(),
	}
}

// maskLoginID 脱敏手机号（138****8000）；非 11 位数字原样保留前 2 后 2。
func maskLoginID(p string) string {
	if len(p) == 11 {
		return p[:3] + "****" + p[7:]
	}
	if len(p) > 4 {
		return p[:2] + "***" + p[len(p)-2:]
	}
	return p
}

// reloadLoomy 重扫 Loomy 账号目录并对齐池。
func (p *Panel) reloadLoomy() (int, error) {
	if p.deps.ReloadLoomyAuths == nil {
		return 0, nil
	}
	return p.deps.ReloadLoomyAuths()
}
