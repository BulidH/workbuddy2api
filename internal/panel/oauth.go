// oauth.go 面板内的账号添加流程（设备授权 + global 域注册激活）。
//
// 与 cmd/login + login.sh 等价，但全程在浏览器里完成，并复用进程内的账号池对齐，
// 因此**新增账号不需要重启**（pool.SyncToDir 语义）。
//
// 流程：
//  1. POST /panel/api/login/start {realm}          → 取授权 URL（state 暂存服务端）
//  2. 用户在浏览器完成登录
//  3. GET  /panel/api/login/poll?state=&realm=     → 换 token → 落盘凭证 → 对齐账号池
//     - global 域若返回「region required」→ 回 needs_region + 可选地区列表
//  4. POST /panel/api/login/region {uid, ios2, code, en_name} → 提交地区 → 重新激活 → 领 trial
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	baseCN     = "https://copilot.tencent.com"
	baseGlobal = "https://www.workbuddy.ai"

	originCN     = "https://www.codebuddy.cn"
	originGlobal = "https://www.workbuddy.ai"

	// 与 cmd/login 保持一致的客户端标识
	loginUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// stateTTL 授权 state 的服务端暂存时长（超时后 poll 拒绝，需重新发起）。
const stateTTL = 30 * time.Minute

func realmEndpoints(realm string) (base, origin string) {
	if realm == "global" {
		return baseGlobal, originGlobal
	}
	return baseCN, originCN
}

// envelope 上游统一响应信封 {code,msg,data}。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// jsonStrOrObj 兼容 data 为内联对象或 JSON 字符串两种形态（部分端点返回字符串）。
func jsonStrOrObj(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return json.RawMessage(s)
	}
	return raw
}

// upstreamCall 发一个带官方头族的请求并解开信封。
func (p *Panel) upstreamCall(method, url, origin, token string, body any, extraHeaders map[string]string) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", loginUA)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	// 每次调用用独立 jar：多账号登录互不串会话（与 cmd/login 语义一致）
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("上游返回非 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	// 业务码非 0 一律当失败（含 "login ing" 这类 pending 态）
	if env.Code != 0 {
		return nil, fmt.Errorf("%s", nonEmpty(env.Msg, fmt.Sprintf("code=%d", env.Code)))
	}
	return env.Data, nil
}

// handleLoginStart 发起设备授权，返回授权 URL。
func (p *Panel) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	realm := "cn"
	if strings.ToLower(strings.TrimSpace(body.Realm)) == "global" {
		realm = "global"
	}
	base, origin := realmEndpoints(realm)

	data, err := p.upstreamCall(http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", origin, "", map[string]any{}, nil)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "获取授权链接失败: "+err.Error())
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		errJSON(w, http.StatusBadGateway, "上游未返回 state/authUrl")
		return
	}

	// 暂存 state（服务端权威），并顺手清理过期项
	p.mu.Lock()
	for k, v := range p.states {
		if time.Since(v.At) > stateTTL {
			delete(p.states, k)
		}
	}
	p.states[st.State] = loginState{Realm: realm, At: time.Now()}
	p.mu.Unlock()

	okJSON(w, map[string]any{"ok": true, "state": st.State, "url": st.AuthURL, "realm": realm})
}

// handleLoginPoll 轮询登录结果；成功后落盘凭证并对齐账号池。
func (p *Panel) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		errJSON(w, http.StatusBadRequest, "缺少 state")
		return
	}
	p.mu.Lock()
	st, known := p.states[state]
	p.mu.Unlock()
	if !known {
		errJSON(w, http.StatusBadRequest, "state 不存在或已过期，请重新获取授权链接")
		return
	}
	realm := st.Realm
	base, origin := realmEndpoints(realm)

	// 1) 换 token
	data, err := p.upstreamCall(http.MethodGet, base+"/v2/plugin/auth/token?state="+state, origin, "", nil, nil)
	if err != nil {
		// pending 是常态（用户还没点完），用 409 与真实错误区分
		okJSON(w, map[string]any{"ok": false, "pending": true, "msg": "尚未完成登录：" + err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		okJSON(w, map[string]any{"ok": false, "pending": true, "msg": "尚未完成登录"})
		return
	}

	// 2) 取账号信息（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if raw, err := p.upstreamCall(http.MethodGet, base+"/v2/plugin/login/account?state="+state, origin, tok.AccessToken, nil, nil); err == nil {
		_ = json.Unmarshal(raw, &acct)
	}
	if acct.UID == "" {
		okJSON(w, map[string]any{"ok": false, "pending": true, "msg": "已拿到 token 但未取到 uid，请稍后重试"})
		return
	}

	// 3) 落盘凭证
	if err := p.writeAuthFile(realm, tok.AccessToken, tok.RefreshToken, tok.ExpiresIn, tok.Domain,
		acct.UID, acct.EnterpriseID, acct.Nickname); err != nil {
		errJSON(w, http.StatusInternalServerError, "写入凭证失败: "+err.Error())
		return
	}
	// state 一次性，用掉即删
	p.mu.Lock()
	delete(p.states, state)
	p.mu.Unlock()

	n, _ := p.reload()

	resp := map[string]any{
		"ok":       true,
		"uid":      acct.UID,
		"nickname": acct.Nickname,
		"realm":    realm,
		"accounts": n,
		"msg":      "账号已添加并加入账号池",
	}

	// 4) global 域：注册激活（新号需先完善地区，否则聊天报 14017）
	if realm == "global" {
		okReg, needsRegion, regMsg := p.activateRegion(tok.AccessToken, acct.UID)
		switch {
		case okReg:
			resp["register"] = "已激活"
			resp["trial"] = p.claimTrial(tok.AccessToken)
		case needsRegion:
			resp["needs_region"] = true
			resp["register"] = "需完善注册地区（否则聊天报 14017）"
			if countries, err := p.fetchCountries(); err == nil {
				resp["countries"] = countries
			}
			if ios2, en, err := p.detectRegion(tok.AccessToken); err == nil && ios2 != "" {
				resp["detected_region"] = map[string]string{"ios2": ios2, "en_name": en}
			}
		default:
			resp["register"] = "激活失败：" + regMsg
		}
	}
	okJSON(w, resp)
}

// handleLoginRegion 为 global 新号提交注册地区，随后重新激活并领取 trial。
func (p *Panel) handleLoginRegion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID    string `json:"uid"`
		IOS2   string `json:"ios2"`
		EnName string `json:"en_name"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if body.UID == "" || body.IOS2 == "" {
		errJSON(w, http.StatusBadRequest, "缺少 uid 或 ios2")
		return
	}
	token, err := p.tokenOfUID(body.UID)
	if err != nil {
		errJSON(w, http.StatusNotFound, err.Error())
		return
	}
	if err := p.submitRegion(token, body.UID, body.Code, body.EnName, body.IOS2); err != nil {
		errJSON(w, http.StatusBadGateway, "提交地区失败: "+err.Error())
		return
	}
	okReg, _, msg := p.activateRegion(token, body.UID)
	resp := map[string]any{"ok": okReg, "msg": msg, "region": body.IOS2}
	if okReg {
		resp["msg"] = "地区已完善，账号可正常对话"
		resp["trial"] = p.claimTrial(token)
	}
	okJSON(w, resp)
}

// ─── 凭证与 global 域辅助 ─────────────────────────────────

// writeAuthFile 以与 login.sh 完全一致的嵌套格式落盘凭证（0600）。
func (p *Panel) writeAuthFile(realm, access, refresh string, expiresIn int64, domain, uid, entID, nick string) error {
	if err := os.MkdirAll(p.deps.AuthDir, 0o700); err != nil {
		return err
	}
	auth := map[string]any{
		"account": map[string]any{
			"uid":          uid,
			"enterpriseId": entID,
			"nickname":     nick,
		},
		"auth": map[string]any{
			"accessToken":  access,
			"refreshToken": refresh,
			"expiresAt":    time.Now().Unix() + expiresIn,
			"domain":       domain,
			"realm":        realm,
		},
	}
	out, err := json.MarshalIndent(auth, "", " ")
	if err != nil {
		return err
	}
	path, ok := p.authFilePath(uid)
	if !ok {
		return fmt.Errorf("非法 uid: %q", uid)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// tokenOfUID 从凭证文件读 accessToken（供地区补全用）。
func (p *Panel) tokenOfUID(uid string) (string, error) {
	path, ok := p.authFilePath(uid)
	if !ok {
		return "", fmt.Errorf("非法 uid")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("凭证不存在: %s", filepath.Base(path))
	}
	var a struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Auth.AccessToken == "" {
		return "", fmt.Errorf("凭证解析失败或缺少 accessToken")
	}
	return a.Auth.AccessToken, nil
}

// activateRegion 调 register 激活/查询。返回 (ok, needsRegion, msg)。
func (p *Panel) activateRegion(token, uid string) (bool, bool, string) {
	data, err := p.upstreamCall(http.MethodGet,
		baseGlobal+"/auth/realms/copilot/overseas/user/register?userId="+uid,
		originGlobal, token, nil, map[string]string{"X-User-Id": uid})
	if err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "region required") {
			return false, true, msg
		}
		return false, false, msg
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &r)
	}
	// 该端点成功时 data 可能为空或 {code:200}
	if r.Code == 200 || r.Code == 0 {
		return true, false, "register success"
	}
	if strings.Contains(strings.ToLower(r.Msg), "region required") || r.Code == 500 {
		return false, true, nonEmpty(r.Msg, fmt.Sprintf("code=%d", r.Code))
	}
	return false, false, nonEmpty(r.Msg, fmt.Sprintf("code=%d", r.Code))
}

// country 可选注册地区。
type country struct {
	IOS2   string `json:"ios2"`
	EnName string `json:"en_name"`
	Name   string `json:"name"`
	Code   string `json:"code"`
}

// fetchCountries 拉取国际版可选注册地区（web 端白名单）。
func (p *Panel) fetchCountries() ([]country, error) {
	data, err := p.upstreamCall(http.MethodPost, baseGlobal+"/billing/area/get-country-code",
		originGlobal, "", map[string]any{"filterForbidden": 1}, nil)
	if err != nil {
		return nil, err
	}
	var inner struct {
		Data struct {
			List []struct {
				IOS2   string `json:"IOS2"`
				EnName string `json:"EnName"`
				Name   string `json:"Name"`
				Code   any    `json:"Code"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(jsonStrOrObj(data), &inner); err != nil {
		return nil, err
	}
	allow := map[string]bool{"HK": true, "MO": true, "SG": true, "TH": true, "PH": true, "MY": true, "ID": true}
	out := []country{}
	for _, c := range inner.Data.List {
		if !allow[c.IOS2] {
			continue
		}
		out = append(out, country{
			IOS2:   c.IOS2,
			EnName: c.EnName,
			Name:   c.Name,
			Code:   fmt.Sprintf("%v", c.Code),
		})
	}
	return out, nil
}

// detectRegion 检测账号当前注册地区。返回 (ios2, enName, err)。
func (p *Panel) detectRegion(token string) (string, string, error) {
	data, err := p.upstreamCall(http.MethodPost, baseGlobal+"/billing/area/get-user-area-info",
		originGlobal, token, map[string]any{"action": "getUserAreaInfo"}, nil)
	if err != nil {
		return "", "", err
	}
	var inner struct {
		Data struct {
			IOS2   string `json:"IOS2"`
			EnName string `json:"enName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(jsonStrOrObj(data), &inner); err != nil {
		return "", "", err
	}
	return inner.Data.IOS2, inner.Data.EnName, nil
}

// submitRegion 提交注册地区。
func (p *Panel) submitRegion(token, uid, code, enName, ios2 string) error {
	body := map[string]any{
		"attributes": map[string]any{
			"countryCode":     []string{code},
			"countryFullName": []string{enName},
			"countryName":     []string{ios2},
		},
	}
	_, err := p.upstreamCall(http.MethodPost, baseGlobal+"/console/login/account",
		originGlobal, token, body, map[string]string{"X-User-Id": uid})
	return err
}

// claimTrial 领取一次性 trial 加油包（幂等，失败不阻断）。
func (p *Panel) claimTrial(token string) string {
	_, err := p.upstreamCall(http.MethodPost, baseGlobal+"/billing/ide/trial", originGlobal, token, map[string]any{}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "14051") {
			return "已领取过"
		}
		return "领取失败：" + err.Error()
	}
	return "已激活"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
