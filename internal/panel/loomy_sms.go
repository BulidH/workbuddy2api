// loomy_sms.go 面板的 Loomy 短信免密登录入口。
//
// 交互与 WorkBuddy/Qoder 的「设备授权链接」对齐：面板生成一条一次性登录链接
// （/panel/loomy-login?t=<ticket>），用户在任意浏览器（手机也行）打开、填手机号
// 收验证码、完成登录，网关在那边拿到 14 天 session 后落盘入池；面板轮询
// /panel/api/loomy/sms/status 即可看到 done。
//
// 同一个弹窗也支持就地填手机号/验证码（两条 UI 路径共用这三个接口），
// ticket 只由服务端保存，URL 里不出现手机号与验证码。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/loomyops"
)

// handleLoomySMSStart 生成一次性登录会话，返回可分享的登录链接。
func (p *Panel) handleLoomySMSStart(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomySMSStart == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	ticket, expiresAt, err := p.deps.LoomySMSStart()
	if err != nil || ticket == "" {
		msg := "生成登录链接失败"
		if err != nil {
			msg += ": " + err.Error()
		}
		errJSON(w, http.StatusInternalServerError, msg)
		return
	}
	okJSON(w, map[string]any{
		"ok":         true,
		"ticket":     ticket,
		"url":        smsLoginURL(r, ticket),
		"expires_at": expiresAt,
		"expires_in": int(loomyops.SMSLoginTTL.Seconds()),
		"ttl_sec":    int(time.Until(time.Unix(expiresAt, 0)).Seconds()),
	})
}

// handleLoomySMSSend 绑定手机号并发送验证码。
func (p *Panel) handleLoomySMSSend(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomySMSSend == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	var body struct {
		Ticket string `json:"ticket"`
		Phone  string `json:"phone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := p.deps.LoomySMSSend(ctx, strings.TrimSpace(body.Ticket), strings.TrimSpace(body.Phone))
	if err != nil {
		smsErrJSON(w, r, err)
		return
	}
	okJSON(w, map[string]any{
		"ok":         true,
		"phone":      res.PhoneMasked,
		"resend_in":  res.ResendIn,
		"expires_at": res.ExpiresAt,
		"url":        smsLoginURL(r, strings.TrimSpace(body.Ticket)),
	})
}

// handleLoomySMSVerify 校验验证码 → 换取 session → 落盘入池。
func (p *Panel) handleLoomySMSVerify(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomySMSVerify == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	var body struct {
		Ticket   string `json:"ticket"`
		Code     string `json:"code"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	a, err := p.deps.LoomySMSVerify(ctx, strings.TrimSpace(body.Ticket),
		strings.TrimSpace(body.Code), strings.TrimSpace(body.Nickname))
	if err != nil {
		smsErrJSON(w, r, err)
		return
	}
	n, _ := p.reloadLoomy()
	okJSON(w, map[string]any{"ok": true, "account": loomyAccountView(a), "accounts": n})
}

// handleLoomySMSStatus 轮询会话状态；done 时带回账号摘要供面板直接渲染。
func (p *Panel) handleLoomySMSStatus(w http.ResponseWriter, r *http.Request) {
	if p.deps.LoomySMSStatus == nil {
		errJSON(w, http.StatusNotImplemented, "Loomy 未启用（config loomy.enabled=false）")
		return
	}
	st := p.deps.LoomySMSStatus(strings.TrimSpace(r.URL.Query().Get("ticket")))
	payload := map[string]any{
		"ok":           st.State != "unknown" && st.State != "expired",
		"state":        st.State,
		"phone_masked": st.PhoneMasked,
		"message":      st.Message,
		"resend_in":    st.ResendIn,
		"expires_at":   st.ExpiresAt,
	}
	if st.Account != nil {
		payload["account"] = loomyAccountView(st.Account)
	}
	okJSON(w, payload)
}

// handleLoomyLoginPage 服务一次性登录页（内嵌 HTML；链接形如 /panel/loomy-login?t=...）。
func (p *Panel) handleLoomyLoginPage(w http.ResponseWriter, r *http.Request) {
	raw, err := fs.ReadFile(p.staticFS(), "loomy-login.html")
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Loomy 登录页资源缺失")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(raw)
}

// smsErrJSON 把短信登录会话错误映射为合适的 HTTP 状态；上游错误统一 502。
func smsErrJSON(w http.ResponseWriter, r *http.Request, err error) {
	kind, ok := loomyops.IsSMSLoginError(err)
	if !ok {
		errJSON(w, http.StatusBadGateway, "登录上游失败: "+err.Error())
		return
	}
	status := http.StatusBadRequest
	var se *loomyops.SMSLoginError
	switch {
	case errors.As(err, &se) && se.Kind == loomyops.SMSKindCooldown:
		status = http.StatusTooManyRequests
	case kind == loomyops.SMSKindBusy:
		status = http.StatusConflict
	case kind == loomyops.SMSKindNotFound, kind == loomyops.SMSKindExpired:
		status = http.StatusGone
	}
	errJSON(w, status, err.Error())
}

// smsLoginURL 拼出可在浏览器直接打开的登录链接（使用请求实际的 host/scheme）。
//
// 反向代理下 X-Forwarded-Proto/Host 优先，否则用 Request.Host；链接只带 ticket。
func smsLoginURL(r *http.Request, ticket string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	if scheme != "http" && scheme != "https" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/panel/loomy-login?t=%s", scheme, host, url.QueryEscape(ticket))
}

func firstHeaderValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
