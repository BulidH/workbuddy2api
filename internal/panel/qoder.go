// qoder.go 面板的 Qoder 接入：设备授权登录、PAT 导入、账号文件导入。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/qoderops"
)

// isQoderProvider 判定面板请求里的 provider 字段；缺省/空串 = WorkBuddy。
func isQoderProvider(provider string) bool {
	return provider == auth.ProviderQoder
}

// handleQoderLoginStart 生成 Qoder 设备授权 URL（浏览器打开完成授权）。
func (p *Panel) handleQoderLoginStart(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderLogin == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	started, err := p.deps.QoderLogin.Start(body.Realm)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "启动 Qoder 登录失败: "+err.Error())
		return
	}
	started["ok"] = true
	okJSON(w, started)
}

// handleQoderLoginPoll 轮询设备授权状态；成功时账号已落盘并对齐账号池。
func (p *Panel) handleQoderLoginPoll(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderLogin == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	state := r.URL.Query().Get("state")
	res := p.deps.QoderLogin.Poll(r.Context(), state)
	out := map[string]any{"ok": res.Status == "ok", "status": res.Status, "message": res.Message}
	if res.Account != nil {
		out["account"] = qoderAccountView(res.Account)
		if n, err := p.reloadQoder(); err == nil {
			out["accounts"] = n
		}
	}
	okJSON(w, out)
}

// handleQoderPAT 导入 pt- 个人访问令牌。
func (p *Panel) handleQoderPAT(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderLogin == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	var body struct {
		PAT   string `json:"pat"`
		Realm string `json:"realm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	acct, err := p.deps.QoderLogin.ImportPAT(r.Context(), body.PAT, body.Realm)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "PAT 导入失败: "+err.Error())
		return
	}
	n, _ := p.reloadQoder()
	okJSON(w, map[string]any{"ok": true, "account": qoderAccountView(acct), "accounts": n})
}

// handleQoderImport 粘贴/上传 Qoder 账号 JSON（单个对象、数组或 {"accounts":[...]}）。
func (p *Panel) handleQoderImport(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderLogin == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
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
	imported, errs := p.deps.QoderLogin.ImportAccounts(items)
	n, _ := p.reloadQoder()
	views := make([]map[string]any, 0, len(imported))
	for _, a := range imported {
		views = append(views, qoderAccountView(a))
	}
	errMsgs := make([]string, 0, len(errs))
	for _, e := range errs {
		errMsgs = append(errMsgs, e.Error())
	}
	okJSON(w, map[string]any{
		"ok":       len(imported) > 0,
		"imported": views,
		"errors":   errMsgs,
		"accounts": n,
	})
}

func (p *Panel) reloadQoder() (int, error) {
	if p.deps.ReloadQoderAuths == nil {
		return 0, nil
	}
	return p.deps.ReloadQoderAuths()
}

// splitAccountPayloads 把三种输入形态统一成逐账号的 JSON 字节切片。
func splitAccountPayloads(raw []byte) ([][]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	if arr, ok := probe.([]any); ok {
		return encodeItems(arr), nil
	}
	if obj, ok := probe.(map[string]any); ok {
		if accs, ok := obj["accounts"].([]any); ok {
			return encodeItems(accs), nil
		}
		return [][]byte{raw}, nil
	}
	return nil, errors.New("账号数据必须是 JSON 对象或数组")
}

func encodeItems(items []any) [][]byte {
	out := make([][]byte, 0, len(items))
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out
}

func qoderAccountView(a *auth.Auth) map[string]any {
	state := a.QoderStateSnapshot()
	v := map[string]any{
		"uid":          a.UID,
		"nickname":     a.Nickname,
		"realm":        a.Realm(),
		"provider":     auth.ProviderQoder,
		"expires_at":   a.ExpiresAt,
		"domain":       a.Domain,
		"user_type":    a.UserType,
		"org_name":     a.OrganizationName,
		"has_pat":      a.PersonalToken != "",
		"has_refresh":  a.RefreshToken != "",
		"plan":         state.Plan,
		"last_checkin": state.LastCheckin,
		"credits":      a.CreditsRemain(),
	}
	if state.CheckinWhenDisabled {
		v["checkin_when_disabled"] = true
	}
	return v
}

// ── Qoder 运营：签到 / 额度 / 活动 ───────────────────────────────────────

// qoderOpBody 批量操作的公共请求体。
type qoderOpBody struct {
	UID       string   `json:"uid"`
	UIDs      []string `json:"uids"`
	OnlyDaily bool     `json:"only_daily"`
	Force     bool     `json:"force"`
}

func (b qoderOpBody) targets() []string {
	if len(b.UIDs) > 0 {
		return b.UIDs
	}
	if b.UID != "" {
		return []string{b.UID}
	}
	return nil
}

// handleQoderCheckin 批量签到（活动领取 + Credits 补领）。
func (p *Panel) handleQoderCheckin(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderCheckin == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	var body qoderOpBody
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	outcomes, err := p.deps.QoderCheckin(ctx, body.targets(), body.OnlyDaily)
	if err != nil {
		if errors.Is(err, qoderops.ErrBusy) {
			errJSON(w, http.StatusConflict, "Qoder 签到正在执行，请稍后重试")
			return
		}
		errJSON(w, http.StatusBadGateway, "Qoder 签到失败: "+err.Error())
		return
	}
	sum := map[string]int{"total": len(outcomes)}
	var earned int64
	for _, o := range outcomes {
		sum[o.Status]++
		earned += o.Earned
	}
	sum["earned_credit"] = int(earned)
	okJSON(w, map[string]any{"ok": true, "outcomes": outcomes, "summary": sum})
}

// handleQoderBalance 批量刷新额度与套餐（只读）。
func (p *Panel) handleQoderBalance(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderBalance == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	var body qoderOpBody
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	outcomes, err := p.deps.QoderBalance(ctx, body.targets())
	if err != nil {
		errJSON(w, http.StatusBadGateway, "Qoder 额度查询失败: "+err.Error())
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
	lastAt, lastMsg := time.Time{}, ""
	if p.deps.QoderLastCheckin != nil {
		lastAt, lastMsg = p.deps.QoderLastCheckin()
	}
	payload := map[string]any{
		"ok": true, "outcomes": outcomes,
		"summary": map[string]any{"total": len(outcomes), "ok": okN, "fail": failN},
	}
	if !lastAt.IsZero() {
		payload["last_checkin_at"] = lastAt.Format(time.RFC3339)
		payload["last_checkin_summary"] = lastMsg
	}
	okJSON(w, payload)
}

// handleQoderCampaigns 查询单账号活动列表（任务中心）。
func (p *Panel) handleQoderCampaigns(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderCampaigns == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		errJSON(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	res, err := p.deps.QoderCampaigns(r.Context(), uid, force)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "上游未返回可解析的活动列表"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": msg, "result": res})
		return
	}
	okJSON(w, map[string]any{"ok": true, "result": res})
}

// handleQoderClaim 手动领取单个活动。
func (p *Panel) handleQoderClaim(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderClaim == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	var body struct {
		UID        string `json:"uid"`
		CampaignID string `json:"campaign_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !qoderIDValid(body.CampaignID) {
		errJSON(w, http.StatusBadRequest, "非法 campaign_id")
		return
	}
	res, err := p.deps.QoderClaim(r.Context(), body.UID, body.CampaignID)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": res.OK || res.Replayed, "result": res})
}

// handleQoderPrefs 修改账号运营偏好（checkin_when_disabled）。
func (p *Panel) handleQoderPrefs(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderSetCheckinPref == nil {
		errJSON(w, http.StatusNotImplemented, "Qoder 未启用（config qoder.enabled=false）")
		return
	}
	var body struct {
		UID                 string `json:"uid"`
		CheckinWhenDisabled bool   `json:"checkin_when_disabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if body.UID == "" {
		errJSON(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	if err := p.deps.QoderSetCheckinPref(body.UID, body.CheckinWhenDisabled); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "uid": body.UID,
		"checkin_when_disabled": body.CheckinWhenDisabled})
}

// handleQoderActivity 最近活动流水（JSONL 尾部）。
func (p *Panel) handleQoderActivity(w http.ResponseWriter, r *http.Request) {
	if p.deps.QoderActivity == nil {
		okJSON(w, map[string]any{"ok": true, "entries": []any{}, "count": 0})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 200
	}
	if n > 2000 {
		n = 2000
	}
	entries := p.deps.QoderActivity.Tail(n)
	okJSON(w, map[string]any{"ok": true, "entries": entries, "count": len(entries)})
}

// qoderIDValid 限制活动 ID 字符集：它会拼进上游 URL 路径，必须防注入。
func qoderIDValid(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
