package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	// chatPath 必须带 Encode=1：请求体是 Qoder 自定义 Base64 变体（EncodeBody），
	// 少了这个参数服务端会按明文 JSON 解析，直接抛 Jackson
	// "Unrecognized token ..." 并以 500 结束（表现为空 SSE 流）。
	chatPath   = "/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1"
	modelsPath = "/algo/api/v2/model/list?Encode=1"

	pathDevicePoll    = "/api/v1/deviceToken/poll"
	pathDeviceRefresh = "/api/v1/deviceToken/refresh"
	pathJobExchange   = "/api/v1/jobToken/exchange"
	pathJobRefresh    = "/api/v1/jobToken/refresh"
	pathUserinfo      = "/api/v1/userinfo"
)

// Endpoint 单个区域（cn/intl）的服务端点。
type Endpoint struct {
	OpenAPI          string   `json:"openapi"`
	Gateway          string   `json:"gateway"`
	GatewayFallbacks []string `json:"gateway_fallbacks"`
	Website          string   `json:"website"`
	Domain           string   `json:"domain"`
	UA               string   `json:"ua"`
	// 设备授权（PKCE）参数：双区差异由配置表达，避免代码分叉。
	ClientID        string `json:"client_id"`
	RedirectURI     string `json:"redirect_uri"`
	NonceDashed     bool   `json:"nonce_dashed"`
	SendClientID    bool   `json:"send_client_id"`
	SendRedirectURI bool   `json:"send_redirect_uri"`
}

// Config Qoder 客户端配置。
type Config struct {
	Endpoints   map[string]Endpoint
	CosyVersion string
	// DesktopVersion 活动平台使用的桌面端版本（cosy-version），缺省 0.4.8。
	DesktopVersion string
	Timeout        time.Duration
}

// DefaultEndpoints 返回官方双区域端点（可被配置覆盖）。
func DefaultEndpoints() map[string]Endpoint {
	return map[string]Endpoint{
		"cn": {
			OpenAPI:         "https://openapi.qoder.com.cn",
			Gateway:         "https://gateway.qoder.com.cn",
			Website:         "https://qoder.com.cn",
			Domain:          "qoder.com.cn",
			UA:              "QoderWork/1.1.75",
			ClientID:        "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
			RedirectURI:     "qoder-work-cn://",
			NonceDashed:     true,
			SendClientID:    true,
			SendRedirectURI: true,
		},
		"intl": {
			OpenAPI:          "https://openapi.qoder.sh",
			Gateway:          "https://api3.qoder.sh",
			GatewayFallbacks: []string{"https://api2.qoder.sh", "https://api1.qoder.sh"},
			Website:          "https://qoder.com",
			Domain:           "qoder.com",
			UA:               "Qoder/1.1.75",
			ClientID:         "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
			RedirectURI:      "qoder://aicoding.aicoding-agent/login-success",
			NonceDashed:      false,
			SendClientID:     true,
			SendRedirectURI:  false,
		},
	}
}

// Client Qoder 上游客户端。
type Client struct {
	cfg      Config
	apiHTTP  *http.Client
	chatHTTP *http.Client
	Catalog  *Catalog
	sessions *SessionCache
	logins   *LoginManager

	modelMu   sync.Mutex
	modelFail map[string]time.Time

	// campaignMu/campaignCache 活动列表短缓存（上游一次 1–4 秒，面板轮询会重复打）。
	campaignMu    sync.Mutex
	campaignCache map[string]campaignCacheEntry
}

// New 构建客户端。
func New(cfg Config) *Client {
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = DefaultEndpoints()
	}
	if cfg.CosyVersion == "" {
		cfg.CosyVersion = defaultCosyVersion
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 600 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		cfg:       cfg,
		apiHTTP:   &http.Client{Timeout: 30 * time.Second, Transport: tr},
		chatHTTP:  &http.Client{Transport: tr},
		Catalog:   NewCatalog(),
		sessions:  NewSessionCache(),
		modelFail: map[string]time.Time{},
	}
}

// SetLoginManager 注入设备授权登录管理器（面板使用）。
func (c *Client) SetLoginManager(m *LoginManager) { c.logins = m }

// LoginManager 返回设备授权登录管理器。
func (c *Client) LoginManager() *LoginManager { return c.logins }

// Endpoint 返回区域端点。
func (c *Client) Endpoint(realm string) Endpoint {
	if ep, ok := c.cfg.Endpoints[NormalizeRealm(realm)]; ok {
		return ep
	}
	return DefaultEndpoints()[NormalizeRealm(realm)]
}

// Realm 返回账号所属 Qoder 区域（cn/intl）。
func (c *Client) Realm(a *auth.Auth) string {
	if a == nil {
		return "cn"
	}
	return NormalizeRealm(a.Realm())
}

func (c *Client) gateways(realm string) []string {
	ep := c.Endpoint(realm)
	out := []string{ep.Gateway}
	out = append(out, ep.GatewayFallbacks...)
	return out
}

func (c *Client) session(a *auth.Auth) (*Session, error) {
	return c.sessions.Get(a.UID, a.Nickname, a.AccessToken, a.RefreshToken, a.UserType, a.OrganizationID, a.OrganizationName)
}

// Supports 报告账号所属区域是否支持该模型名。
func (c *Client) Supports(realm, model string) bool {
	_, ok := c.Catalog.Lookup(realm, model)
	return ok
}

// ChatMeta 单次聊天请求的会话信息（用于粘性/日志，可选）。
type ChatMeta struct {
	SessionKey string
	RequestID  string
}

// result of findModel: 目录条目 + 所属区域。
type modelMatch struct {
	Info  ModelInfo
	Realm string
}

func (c *Client) findModel(realm, model string) (modelMatch, bool) {
	if m, ok := c.Catalog.Lookup(realm, model); ok {
		return modelMatch{Info: m, Realm: realm}, true
	}
	// 账号区域未命中时，跨区找一次（同一模型在另一区可用时允许借用）。
	other := "intl"
	if NormalizeRealm(realm) == "intl" {
		other = "cn"
	}
	if m, ok := c.Catalog.Lookup(other, model); ok {
		return modelMatch{Info: m, Realm: other}, true
	}
	return modelMatch{}, false
}

// ChatStreamContext 发起 Qoder 推理请求，返回已归一化为 OpenAI SSE 的响应流。
// stream=false 由调用方在本地聚合（/v1/chat/completions 非流式分支）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, 400, []byte(`{"error":"invalid request body"}`), nil
	}
	realm := c.Realm(a)
	model := strField(req, "model")
	match, ok := c.findModel(realm, model)
	if !ok {
		return nil, http.StatusNotFound, []byte(fmt.Sprintf(`{"error":"model %q is not available on qoder"}`, model)), nil
	}
	acct := QoderAccount{
		UID:       a.UID,
		Nickname:  a.Nickname,
		UserType:  a.UserType,
		OrgID:     a.OrganizationID,
		OrgName:   a.OrganizationName,
		AccessTok: a.AccessToken,
		Refresh:   a.RefreshToken,
	}
	if transcribed, total := PrepareRequestImages(req); transcribed > 0 {
		log.Printf("qoder images: transcoded %d/%d inline png(s)", transcribed, total)
	}
	qbody, err := BuildRequest(req, acct, match.Info, realm)
	if err != nil {
		return nil, http.StatusBadRequest, []byte(`{"error":"build qoder request: ` + escapeJSON(err.Error()) + `"}`), nil
	}
	raw, err := json.Marshal(qbody)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, err
	}
	encoded := EncodeBody(raw)
	sess, err := c.session(a)
	if err != nil {
		return nil, http.StatusUnauthorized, []byte(`{"error":"qoder session: ` + escapeJSON(err.Error()) + `"}`), nil
	}

	var lastErr error
	var lastStatus int
	var lastBody []byte
	for _, gw := range c.gateways(match.Realm) {
		rawURL := strings.TrimRight(gw, "/") + chatPath
		headers := sess.Headers(encoded, rawURL, match.Info.Key, true, c.cfg.CosyVersion)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(encoded))
		if err != nil {
			lastErr = err
			continue
		}
		for k, v := range headers {
			httpReq.Header.Set(k, v)
		}
		resp, err := c.chatHTTP.Do(httpReq)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			lastStatus, lastBody = resp.StatusCode, detail
			if resp.StatusCode < 500 {
				return nil, resp.StatusCode, detail, nil
			}
			continue
		}
		rc, status, detail := c.unwrapSSE(resp.Body, model)
		if rc == nil {
			if status < 500 {
				return nil, status, detail, nil
			}
			lastStatus, lastBody = status, detail
			continue
		}
		return rc, http.StatusOK, nil, nil
	}
	if lastStatus != 0 {
		return nil, lastStatus, lastBody, nil
	}
	return nil, http.StatusServiceUnavailable, []byte(fmt.Sprintf(`{"error":"qoder upstream unreachable: %s"}`, escapeJSON(errText(lastErr)))), nil
}

// unwrapSSE 读取首个信封帧：非 200 直接返回错误，否则返回持续输出 OpenAI SSE 的流。
// 返回 (nil, status, body) 表示首帧即错误。
func (c *Client) unwrapSSE(body io.ReadCloser, model string) (io.ReadCloser, int, []byte) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	// 首帧之前可能有心跳/空行，循环读到第一个可解析帧。
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data := stripDataPrefix(line)
		if data == "" {
			continue
		}
		var outer envelopeFrame
		if err := json.Unmarshal([]byte(data), &outer); err != nil {
			continue
		}
		if outer.StatusCode != 0 && outer.StatusCode != 200 {
			defer body.Close()
			detail := []byte(outer.Body)
			if len(detail) == 0 {
				detail, _ = json.Marshal(map[string]any{"error": "qoder upstream status", "status": outer.StatusCode})
			}
			status := outer.StatusCode
			if status < 400 {
				status = http.StatusBadGateway
			}
			return nil, status, detail
		}
		if outer.Body == "" {
			continue
		}
		pr, pw := io.Pipe()
		go func(first envelopeFrame) {
			defer body.Close()
			writeFrame := func(frame envelopeFrame) bool {
				if frame.StatusCode != 0 && frame.StatusCode != 200 {
					_, _ = pw.Write([]byte("data: " + `{"error":{"message":"qoder upstream status ` + strconv.Itoa(frame.StatusCode) + `","type":"upstream_error"}}` + "\n\n"))
					_, _ = pw.Write([]byte("data: [DONE]\n\n"))
					return false
				}
				if frame.Body == "" {
					return true
				}
				if frame.Body == "[DONE]" {
					_, _ = pw.Write([]byte("data: [DONE]\n\n"))
					return false
				}
				_, err := pw.Write([]byte("data: " + rewriteFrameModel(frame.Body, model) + "\n\n"))
				return err == nil
			}
			if !writeFrame(first) {
				_ = pw.Close()
				return
			}
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				data := stripDataPrefix(line)
				if data == "" {
					continue
				}
				var frame envelopeFrame
				if err := json.Unmarshal([]byte(data), &frame); err != nil {
					continue
				}
				if !writeFrame(frame) {
					_ = pw.Close()
					return
				}
			}
			if err := sc.Err(); err != nil {
				_, _ = pw.Write([]byte("data: " + `{"error":{"message":"qoder stream interrupted","type":"upstream_error"}}` + "\n\n"))
			}
			_, _ = pw.Write([]byte("data: [DONE]\n\n"))
			_ = pw.Close()
		}(outer)
		return pr, http.StatusOK, nil
	}
	defer body.Close()
	detail, _ := json.Marshal(map[string]any{"error": "qoder upstream closed without any SSE frame"})
	return nil, http.StatusBadGateway, detail
}

type envelopeFrame struct {
	Headers    map[string]any `json:"headers"`
	Body       string         `json:"body"`
	StatusCode int            `json:"statusCodeValue"`
}

// rewriteFrameModel 把上游帧里的 model 改回本次请求的模型名，并把 Qoder 的
// usage.credits 补成网关统一的 usage.credit。
//
// Qoder 上游不论请求哪个模型，SSE 帧里的 model 一律回 "auto"（实测
// DeepSeek-Flash / DeepSeek-V4-Pro / Qwen3.8-Max 都是），客户端拿到的
// 模型名跟请求对不上，会让按 model 字段做统计/路由的下游错乱。
// usage 里 Qoder 用 credits（复数），WorkBuddy 用 credit（单数），网关的
// 成本账本只认 credit，不补齐则 Qoder 的实测成本永远记不上。
// 帧不是合法 JSON 时原样返回。
func rewriteFrameModel(frameBody, model string) string {
	needCredit := strings.Contains(frameBody, `"credits"`)
	if model == "" && !needCredit {
		return frameBody
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(frameBody), &m); err != nil {
		return frameBody
	}
	changed := false
	if model != "" {
		if _, ok := m["model"]; ok {
			m["model"] = model
			changed = true
		}
	}
	if usage, ok := m["usage"].(map[string]any); ok {
		if _, has := usage["credit"]; !has {
			if c, hasCredits := usage["credits"]; hasCredits {
				usage["credit"] = c
				changed = true
			}
		}
	}
	if !changed {
		return frameBody
	}
	out, err := json.Marshal(m)
	if err != nil {
		return frameBody
	}
	return string(out)
}

func stripDataPrefix(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, ":") {
		return ""
	}
	for strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(line[len("data:"):])
	}
	if line == "" || strings.HasPrefix(line, ":") {
		return ""
	}
	return line
}

// ListModels 动态拉取某区域模型目录（需要该区域有可用账号做签名）。
func (c *Client) ListModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	realm := c.Realm(a)
	sess, err := c.session(a)
	if err != nil {
		return nil, err
	}
	signBody := EncodeBody([]byte("{}"))
	var payload []byte
	var lastErr error
	for _, gw := range c.gateways(realm) {
		rawURL := strings.TrimRight(gw, "/") + modelsPath
		headers := sess.Headers(signBody, rawURL, "", false, c.cfg.CosyVersion)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, strings.NewReader(signBody))
		if err != nil {
			lastErr = err
			continue
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.apiHTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("qoder models http %d: %s", resp.StatusCode, truncateBytes(body, 200))
			continue
		}
		payload = body
		break
	}
	if payload == nil {
		return nil, lastErr
	}
	var doc struct {
		Chat []map[string]any `json:"chat"`
		App  []map[string]any `json:"app"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(doc.Chat)+len(doc.App))
	seen := map[string]bool{}
	for _, rows := range [][]map[string]any{doc.Chat, doc.App} {
		for _, row := range rows {
			key := strField(row, "key")
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			mi := ModelInfo{
				Key:         key,
				DisplayName: strField(row, "display_name"),
				IsVL:        boolField(row, "is_vl"),
				IsReasoning: boolField(row, "is_reasoning"),
			}
			if n, ok := intField(row, "max_input_tokens"); ok {
				mi.MaxInputToken = n
			}
			if v, ok := row["enable"].(bool); ok {
				mi.Enable = &v
			}
			out = append(out, mi)
		}
	}
	if len(out) > 0 {
		c.Catalog.ReplaceDynamic(realm, out)
	}
	return out, nil
}

// EnsureCatalog 在目录过期时尝试刷新（失败静默，保留内嵌快照）。
func (c *Client) EnsureCatalog(ctx context.Context, a *auth.Auth) {
	realm := c.Realm(a)
	if c.Catalog.Fresh(realm) {
		return
	}
	c.modelMu.Lock()
	last := c.modelFail[realm]
	c.modelMu.Unlock()
	if time.Since(last) < 5*time.Minute {
		return
	}
	if _, err := c.ListModels(ctx, a); err != nil {
		c.modelMu.Lock()
		c.modelFail[realm] = time.Now()
		c.modelMu.Unlock()
	}
}

// RefreshToken 刷新账号凭证（device/job/PAT 三族）。
func (c *Client) RefreshToken(ctx context.Context, a *auth.Auth) error {
	realm := c.Realm(a)
	ep := c.Endpoint(realm)
	if a.AccessToken == "" && a.RefreshToken == "" && a.PersonalToken == "" {
		return fmt.Errorf("qoder: no credentials to refresh")
	}
	if strings.HasPrefix(a.RefreshToken, "drt-") {
		if err := c.postToken(ctx, a, ep, pathDeviceRefresh, map[string]string{"refresh_token": a.RefreshToken}, true); err == nil {
			return nil
		}
	}
	if a.RefreshToken != "" {
		if err := c.postToken(ctx, a, ep, pathJobRefresh, map[string]string{"refresh_token": a.RefreshToken}, false); err == nil {
			return nil
		}
	}
	if a.PersonalToken != "" {
		return c.postToken(ctx, a, ep, pathJobExchange, map[string]string{"personal_token": a.PersonalToken}, false)
	}
	return fmt.Errorf("qoder: refresh failed for uid=%s", a.UID)
}

func (c *Client) postToken(ctx context.Context, a *auth.Auth, ep Endpoint, path string, payload map[string]string, device bool) error {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep.OpenAPI, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultStr(ep.UA, "QoderWork/1.1.75"))
	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qoder refresh http %d: %s", resp.StatusCode, truncateBytes(body, 200))
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("qoder refresh decode: %w", err)
	}
	token := strField(data, "token")
	if token == "" {
		token = strField(data, "device_token")
	}
	if token == "" {
		return fmt.Errorf("qoder refresh returned no token")
	}
	a.AccessToken = token
	if rt := strField(data, "refresh_token"); rt != "" {
		a.RefreshToken = rt
	}
	if device {
		a.ExpiresAt = deviceExpiry(data)
	} else if ms, ok := intField(data, "expires_in"); ok {
		a.ExpiresAt = time.Now().Unix() + int64(ms)/1000
	}
	if err := a.SaveAtomic(); err != nil {
		// 刷新成功但落盘失败不阻断本次请求（下次启动可能用旧 token）。
		return nil
	}
	c.sessions.Invalidate(a.UID)
	return nil
}

func deviceExpiry(data map[string]any) int64 {
	if ms, ok := intField(data, "expires_in"); ok {
		return time.Now().Unix() + int64(ms)/1000
	}
	if s := strField(data, "expires_at"); s != "" {
		if t, err := time.Parse("2006-01-02T15:04:05", s[:min(len(s), 19)]); err == nil {
			return t.Unix()
		}
	}
	return time.Now().Unix() + 30*86400
}

func boolField(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}
