// client.go Loomy 账号服务 + 模型网关客户端。
package loomy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// Config 客户端配置。
type Config struct {
	ModelBase       string
	AccountBase     string
	AccountAppID    string
	AccessKeyID     string
	AccessKeySecret string
	Version         string
	RequestPurpose  string
	Timeout         time.Duration
}

// Identity 账号的设备身份（账号服务请求信封字段 + 推广设备号）。
type Identity struct {
	DevID          string `json:"devid"`
	UA             string `json:"ua"`
	ModelID        string `json:"modelid"`
	Version        string `json:"version"`
	CampusDeviceID string `json:"campus_device_id"`
}

// LoginResult 一次登录的结果。
type LoginResult struct {
	Session  string
	UserID   string
	Phone    string
	ExpireAt int64
}

// ChatMeta 单次聊天请求的归因信息（可选）。
type ChatMeta struct {
	SessionKey string
	RequestID  string
	ChatID     string
	MsgID      string
	TurnID     string
	Purpose    string
}

// Quota 积分余额。
type Quota struct {
	Balance   *int64 `json:"balance"`
	Daily     *int64 `json:"dailyBalance"`
	Available *int64 `json:"availableBalance"`
}

// ModelInfo 模型目录条目（/models 的 data 元素）。
type ModelInfo struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	ContextLength int64          `json:"context_length"`
	MaxOutput     int64          `json:"max_output_tokens"`
	Capabilities  map[string]any `json:"capabilities"`
	Efforts       []string       `json:"reasoning_efforts"`
}

// Error 上游业务/HTTP 错误。
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("loomy upstream %d/%s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("loomy upstream %d: %s", e.Status, e.Msg)
}

// Client Loomy 上游客户端（账号服务 + 模型网关）。
type Client struct {
	cfg      Config
	apiHTTP  *http.Client
	chatHTTP *http.Client
	// Catalog 模型目录（内嵌兜底 + 动态刷新），供 /v1/models 与合并调度判断。
	Catalog *Catalog
}

// New 构建客户端。
func New(cfg Config) *Client {
	if cfg.ModelBase == "" {
		cfg.ModelBase = DefaultModelBase
	}
	if cfg.AccountBase == "" {
		cfg.AccountBase = DefaultAccountBase
	}
	if cfg.AccountAppID == "" {
		cfg.AccountAppID = DefaultAccountAppID
	}
	if cfg.AccessKeyID == "" {
		cfg.AccessKeyID = DefaultAccessKeyID
	}
	if cfg.AccessKeySecret == "" {
		cfg.AccessKeySecret = DefaultAccessKeySecret
	}
	if cfg.Version == "" {
		cfg.Version = DefaultLoomyVersion
	}
	if cfg.RequestPurpose == "" {
		cfg.RequestPurpose = DefaultRequestPurpose
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 600 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		cfg:      cfg,
		apiHTTP:  &http.Client{Timeout: 60 * time.Second, Transport: tr},
		chatHTTP: &http.Client{Transport: tr},
		Catalog:  NewCatalog(),
	}
}

// EnsureCatalog 目录过期时用池内首个可用账号刷新（失败静默，保留兜底清单）。
func (c *Client) EnsureCatalog(ctx context.Context, a *auth.Auth) {
	if c.Catalog == nil || a == nil || a.AccessToken == "" {
		return
	}
	if c.Catalog.Fresh() {
		return
	}
	if err := c.ensureSession(ctx, a); err != nil {
		return
	}
	rows, err := c.ListModels(ctx, a)
	if err != nil {
		return
	}
	c.Catalog.Replace(rows)
}

// NewIdentity 生成账号的设备身份：每个账号一个独立的 devid / 推广设备号。
func NewIdentity() Identity {
	return Identity{
		DevID:          fmt.Sprintf("web-%s", hexN(16)),
		UA:             DefaultClientUA,
		ModelID:        DefaultWebModelID,
		Version:        DefaultClientVersion,
		CampusDeviceID: CampusDeviceIDPrefix + newUUID(),
	}
}

// envelope base 段。
func (c *Client) base(ident Identity) map[string]string {
	return map[string]string{
		"appid":   c.cfg.AccountAppID,
		"modelid": firstNonEmpty(ident.ModelID, DefaultWebModelID),
		"version": firstNonEmpty(ident.Version, DefaultClientVersion),
		"devid":   firstNonEmpty(ident.DevID, DefaultDeviceID),
		"ua":      firstNonEmpty(ident.UA, DefaultClientUA),
		"traceid": hexN(32), // 客户端每个请求都换新 traceid
	}
}

// accountCall 调账号服务（自动签名）。
func (c *Client) accountCall(ctx context.Context, path string, body map[string]any, ident Identity) (map[string]any, error) {
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.AccountBase, "/")+path, strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range AccountHeaders(c.cfg.AccessKeyID, c.cfg.AccessKeySecret, http.MethodPost, path, raw, time.Now()) {
		req.Header.Set(k, v)
	}
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy account service unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &Error{Status: resp.StatusCode, Msg: truncate(string(data), 200)}
	}
	code := strField(payload, "code")
	if code == "" {
		code = strField(payload, "errorCode")
	}
	if code != "" && code != SuccessCode {
		msg := strField(payload, "desc")
		if msg == "" {
			msg = strField(payload, "message")
		}
		return nil, &Error{Status: resp.StatusCode, Code: code, Msg: msg}
	}
	if resp.StatusCode >= 400 {
		msg := strField(payload, "message")
		if msg == "" {
			msg = truncate(string(data), 200)
		}
		return nil, &Error{Status: resp.StatusCode, Code: code, Msg: msg}
	}
	return payload, nil
}

// LoginByPassword 手机号 + 密码登录，返回 14 天 session。
// 流程：getPuKey 取 RSA 公钥与 rcode（服务端 nonce，非验证码）→ byPwd 提交加密口令。
func (c *Client) LoginByPassword(ctx context.Context, ident Identity, loginID, password string) (LoginResult, error) {
	if loginID == "" || password == "" {
		return LoginResult{}, errors.New("loomy: loginid/password required")
	}
	pk, err := c.accountCall(ctx, PathGetPuKey, map[string]any{"base": c.base(ident)}, ident)
	if err != nil {
		return LoginResult{}, err
	}
	data, _ := pk["data"].(map[string]any)
	pukey := strField(data, "pukey")
	rcode := strField(data, "rcode")
	if pukey == "" {
		return LoginResult{}, errors.New("loomy: getPuKey returned no public key")
	}
	enc, err := RSAEncrypt(pukey, password)
	if err != nil {
		return LoginResult{}, err
	}
	res, err := c.accountCall(ctx, PathLoginByPwd, map[string]any{
		"base": c.base(ident),
		"param": map[string]any{
			"loginid":  loginID,
			"password": enc,
			"rcode":    rcode,
			"type":     1,
			"expire":   SessionExpireSeconds,
		},
	}, ident)
	if err != nil {
		return LoginResult{}, err
	}
	return extractSession(res, loginID)
}

// SendSMSCode 发送登录短信验证码。
func (c *Client) SendSMSCode(ctx context.Context, ident Identity, phone string) (msgID string, err error) {
	res, err := c.accountCall(ctx, PathSendSMS, map[string]any{
		"base":  c.base(ident),
		"param": map[string]any{"ccode": "86", "phone": phone, "expire": 300},
	}, ident)
	if err != nil {
		return "", err
	}
	data, _ := res["data"].(map[string]any)
	return strField(data, "msgid"), nil
}

// LoginBySMS 用短信验证码登录。
func (c *Client) LoginBySMS(ctx context.Context, ident Identity, phone, code, msgID string) (LoginResult, error) {
	res, err := c.accountCall(ctx, PathCheckSMS, map[string]any{
		"base": c.base(ident),
		"param": map[string]any{
			"ccode": "86", "phone": phone, "mcode": code, "msgid": msgID,
			"expire": SessionExpireSeconds,
		},
	}, ident)
	if err != nil {
		return LoginResult{}, err
	}
	return extractSession(res, phone)
}

func extractSession(payload map[string]any, fallbackLoginID string) (LoginResult, error) {
	node, _ := payload["data"].(map[string]any)
	if node == nil {
		node = payload
	}
	session := strField(node, "session")
	if session == "" {
		return LoginResult{}, &Error{Status: 200, Msg: "login returned no session"}
	}
	userID := strField(node, "userid")
	if userID == "" {
		userID = strField(node, "userId")
	}
	phone := strField(node, "phone")
	if phone == "" {
		phone = fallbackLoginID
	}
	now := time.Now().Unix()
	return LoginResult{
		Session:  session,
		UserID:   userID,
		Phone:    phone,
		ExpireAt: now + SessionExpireSeconds,
	}, nil
}

// RefreshSession 用已保存的手机号 + 密码重新登录并写回账号文件。
// Loomy 没有 refresh token：续期等价于重新登录（14 天一次）。
func (c *Client) RefreshSession(ctx context.Context, a *auth.Auth) error {
	if a == nil {
		return errors.New("loomy: nil account")
	}
	if a.LoginID == "" || a.Password == "" {
		return &Error{Status: 401, Code: "NO_CREDENTIALS", Msg: "session expired and no password stored"}
	}
	a.Lock()
	ident := Identity{
		DevID:          a.LoomyDevID,
		UA:             DefaultClientUA,
		ModelID:        DefaultWebModelID,
		Version:        DefaultClientVersion,
		CampusDeviceID: a.LoomyCampusDeviceID,
	}
	a.Unlock()
	res, err := c.LoginByPassword(ctx, ident, a.LoginID, a.Password)
	if err != nil {
		return err
	}
	a.Lock()
	a.AccessToken = res.Session
	a.ExpiresAt = res.ExpireAt
	if a.UID == "" {
		a.UID = res.UserID
	}
	a.Unlock()
	if a.FilePath != "" {
		return a.SaveAtomic()
	}
	return nil
}

// ensureSession 在会话即将过期时先续期；没有密码时保持现状，交给上游 401 判定。
func (c *Client) ensureSession(ctx context.Context, a *auth.Auth) error {
	if a == nil || a.LoginID == "" || a.Password == "" {
		return nil
	}
	if a.ExpiresAt > 0 && time.Now().Add(30*time.Minute).Unix() < a.ExpiresAt {
		return nil
	}
	return c.RefreshSession(ctx, a)
}

// modelHeaders 模型网关必须的请求头。
func (c *Client) modelHeaders(session string) map[string]string {
	return map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + session,
		"token":         session,
		"traceparent":   "00-" + hexN(32) + "-" + hexN(16) + "-01",
		"loomy-version": c.cfg.Version,
	}
}

// ChatStreamContext 发起聊天请求，返回 OpenAI SSE 流。
// 上游本身就是 OpenAI 兼容 SSE，只做两件事：强制 stream:true（便于本地聚合）与
// 把 usage.points_consumed 同步为 usage.credit（网关成本账本只认 credit）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if err := c.ensureSession(ctx, a); err != nil {
		return nil, http.StatusUnauthorized, []byte(`{"error":{"message":"loomy session refresh: ` + escapeJSON(err.Error()) + `"}}`), nil
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, []byte(`{"error":"invalid request body"}`), nil
	}
	req["stream"] = true
	req["stream_options"] = map[string]any{"include_usage": true}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathChat, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, nil, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		httpReq.Header.Set(k, v)
	}
	if meta.ChatID != "" {
		httpReq.Header.Set("ChatId", meta.ChatID)
	}
	if meta.MsgID != "" {
		httpReq.Header.Set("MsgId", meta.MsgID)
	}
	if meta.TurnID != "" {
		httpReq.Header.Set("TurnId", meta.TurnID)
	}
	purpose := meta.Purpose
	if purpose == "" {
		purpose = c.cfg.RequestPurpose
	}
	httpReq.Header.Set("X-Loomy-Request-Purpose", purpose)

	resp, err := c.chatHTTP.Do(httpReq)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 会话失效（401/403 或业务码 020002/100002）且有密码：重新登录后重试一次。
		if isAuthFailure(resp.StatusCode, string(detail)) && a.LoginID != "" && a.Password != "" {
			if rerr := c.RefreshSession(ctx, a); rerr == nil {
				return c.chatOnce(ctx, a, raw, meta)
			}
		}
		return nil, resp.StatusCode, detail, nil
	}
	return rewriteUsageFrames(resp.Body), http.StatusOK, nil, nil
}

// chatOnce 用（刚刷新过的）session 再发一次相同请求。
func (c *Client) chatOnce(ctx context.Context, a *auth.Auth, raw []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathChat, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, nil, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("X-Loomy-Request-Purpose", firstNonEmpty(meta.Purpose, c.cfg.RequestPurpose))
	resp, err := c.chatHTTP.Do(httpReq)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, detail, nil
	}
	return rewriteUsageFrames(resp.Body), http.StatusOK, nil, nil
}

// rewriteUsageFrames 透传 SSE，同时把 usage.points_consumed 映射为 usage.credit。
func rewriteUsageFrames(body io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer body.Close()
		br := bufio.NewReaderSize(body, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				out := line
				if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "data: ") {
					payload := strings.TrimPrefix(trimmed, "data: ")
					if payload != "[DONE]" {
						out = "data: " + rewriteUsageJSON(payload) + "\n"
						if strings.HasSuffix(line, "\r\n") {
							out = "data: " + rewriteUsageJSON(payload) + "\r\n"
						}
					}
				}
				if _, werr := pw.Write([]byte(out)); werr != nil {
					return
				}
			}
			if err != nil {
				_ = pw.Close()
				return
			}
		}
	}()
	return pr
}

// rewriteUsageJSON 把 points_consumed 补成 credit（只在缺失时补，不改其它字段）。
func rewriteUsageJSON(payload string) string {
	if !strings.Contains(payload, `"points_consumed"`) {
		return payload
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return payload
	}
	usage, ok := obj["usage"].(map[string]any)
	if !ok {
		return payload
	}
	if _, has := usage["credit"]; !has {
		if v, hasPts := usage["points_consumed"]; hasPts {
			usage["credit"] = v
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}

// ListModels 拉取模型目录。
func (c *Client) ListModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathModels, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		req.Header.Set(k, v)
	}
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Status: resp.StatusCode, Msg: truncate(string(data), 200)}
	}
	var doc struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.Data, nil
}

// Quota 查询积分余额。
func (c *Client) Quota(ctx context.Context, a *auth.Auth) (Quota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathPoints+"?record_type=all&page_no=1&page_size=1", nil)
	if err != nil {
		return Quota{}, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		req.Header.Set(k, v)
	}
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return Quota{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Quota{}, &Error{Status: resp.StatusCode, Msg: truncate(string(data), 200)}
	}
	var doc struct {
		Data struct {
			Balance          *int64 `json:"balance"`
			DailyBalance     *int64 `json:"dailyBalance"`
			AvailableBalance *int64 `json:"availableBalance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Quota{}, err
	}
	return Quota{Balance: doc.Data.Balance, Daily: doc.Data.DailyBalance, Available: doc.Data.AvailableBalance}, nil
}

// FirstLogin 刷新每日赠送积分（按天幂等），返回最新日赠送余额（可能为 nil）。
func (c *Client) FirstLogin(ctx context.Context, a *auth.Auth) (*int64, error) {
	body := map[string]any{}
	if a.LoomyCampusDeviceID != "" {
		body["deviceId"] = a.LoomyCampusDeviceID
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathFirstLogin, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		req.Header.Set(k, v)
	}
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := checkBusiness(resp.StatusCode, data); err != nil {
		return nil, err
	}
	var doc struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(data, &doc)
	if doc.Data != nil {
		if v, ok := doc.Data["dailyBalance"].(float64); ok {
			n := int64(v)
			return &n, nil
		}
		if nested, ok := doc.Data["data"].(map[string]any); ok {
			if v, ok := nested["dailyBalance"].(float64); ok {
				n := int64(v)
				return &n, nil
			}
		}
	}
	return nil, nil
}

// Onboarding 读取新手任务完成状态（key → done）。
func (c *Client) Onboarding(ctx context.Context, a *auth.Auth) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathOnboardingTasks, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		req.Header.Set(k, v)
	}
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := checkBusiness(resp.StatusCode, data); err != nil {
		return nil, err
	}
	var doc struct {
		Data struct {
			Tasks map[string]any `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(doc.Data.Tasks))
	for _, t := range OnboardingTasks {
		done[t.Key] = doc.Data.Tasks[t.Key] == true
	}
	return done, nil
}

// ClaimTask 上报领取单个新手任务，返回是否此前已完成。
func (c *Client) ClaimTask(ctx context.Context, a *auth.Auth, key string) (already bool, err error) {
	raw, _ := json.Marshal(map[string]string{"key": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.ModelBase, "/")+PathOnboardingComplete, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	for k, v := range c.modelHeaders(a.AccessToken) {
		req.Header.Set(k, v)
	}
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := checkBusiness(resp.StatusCode, data); err != nil {
		return false, err
	}
	var doc struct {
		Data struct {
			AlreadyCompleted bool `json:"alreadyCompleted"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &doc)
	return doc.Data.AlreadyCompleted, nil
}

// checkBusiness 校验上游业务码（000000 才算成功）。
func checkBusiness(status int, data []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return &Error{Status: status, Msg: truncate(string(data), 200)}
	}
	code := firstNonEmpty(strField(payload, "code"), strField(payload, "errorCode"))
	if code != "" && code != SuccessCode {
		return &Error{Status: status, Code: code, Msg: firstNonEmpty(strField(payload, "desc"), strField(payload, "message"))}
	}
	if status >= 400 {
		return &Error{Status: status, Code: code, Msg: truncate(string(data), 200)}
	}
	return nil
}

func isAuthFailure(status int, body string) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	return strings.Contains(body, "020002") || strings.Contains(body, "100002")
}

func strField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}
