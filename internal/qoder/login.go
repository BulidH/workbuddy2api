package qoder

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

const loginTTL = 10 * time.Minute

// LoginManager 管理 Qoder 设备授权登录（PKCE）与 PAT 导入。
type LoginManager struct {
	client *Client
	dir    string

	mu     sync.Mutex
	states map[string]*loginState
}

type loginState struct {
	created  time.Time
	verifier string
	nonce    string
	realm    string
}

// LoginResult 轮询结果（status: pending / ok / expired / unknown / error）。
type LoginResult struct {
	Status  string
	Message string
	Account *auth.Auth
}

// NewLoginManager 构建登录管理器；dir 为 Qoder 账号文件目录。
func NewLoginManager(c *Client, dir string) *LoginManager {
	return &LoginManager{client: c, dir: dir, states: map[string]*loginState{}}
}

// Start 构造设备授权 URL（浏览器打开完成授权）。
func (m *LoginManager) Start(realm string) (map[string]any, error) {
	realm = NormalizeRealm(realm)
	ep := m.client.Endpoint(realm)
	verifier, challenge := makePKCE()
	nonce := randomNonce(ep.NonceDashed)
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	if ep.SendRedirectURI && ep.RedirectURI != "" {
		q.Set("redirect_uri", ep.RedirectURI)
	}
	if ep.SendClientID {
		q.Set("client_id", ep.ClientID)
		q.Set("machine_id", newUUID4())
	}
	authURL := strings.TrimRight(ep.Website, "/") + "/device/selectAccounts?" + q.Encode()
	state := "qd-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	m.mu.Lock()
	m.states[state] = &loginState{created: time.Now(), verifier: verifier, nonce: nonce, realm: realm}
	m.mu.Unlock()
	return map[string]any{"state": state, "authUrl": authURL, "realm": realm}, nil
}

// Cancel 取消一个登录会话。
func (m *LoginManager) Cancel(state string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.states[state]
	delete(m.states, state)
	return ok
}

// Poll 轮询一次设备授权状态；成功时落盘并返回账号。
func (m *LoginManager) Poll(ctx context.Context, state string) LoginResult {
	m.mu.Lock()
	info := m.states[state]
	m.mu.Unlock()
	if info == nil {
		return LoginResult{Status: "unknown", Message: "unknown login state, start again"}
	}
	if time.Since(info.created) > loginTTL {
		m.Cancel(state)
		return LoginResult{Status: "expired", Message: "login window expired, start again"}
	}
	ep := m.client.Endpoint(info.realm)
	q := url.Values{}
	q.Set("nonce", info.nonce)
	q.Set("verifier", info.verifier)
	q.Set("challenge_method", "S256")
	rawURL := strings.TrimRight(ep.OpenAPI, "/") + pathDevicePoll + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return LoginResult{Status: "error", Message: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QoderWork")
	resp, err := m.client.apiHTTP.Do(req)
	if err != nil {
		return LoginResult{Status: "pending", Message: "poll error: " + err.Error()}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return LoginResult{Status: "pending", Message: "等待浏览器完成 Qoder 设备授权"}
	}
	if resp.StatusCode != http.StatusOK {
		return LoginResult{Status: "error", Message: fmt.Sprintf("poll http %d %s", resp.StatusCode, truncateBytes(body, 160))}
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return LoginResult{Status: "pending", Message: "waiting for grant"}
	}
	token := strField(data, "token")
	if token == "" {
		token = strField(data, "device_token")
	}
	if token == "" {
		return LoginResult{Status: "pending", Message: "waiting for token"}
	}
	uid := strField(data, "user_id")
	nickname, userType := "", ""
	orgID, orgName := "", ""
	if ui, err := m.userinfo(ctx, ep, token); err == nil {
		if v := strField(ui, "id"); v != "" {
			uid = v
		}
		nickname = strField(ui, "name")
		userType = strField(ui, "user_type")
		orgID = firstNonEmpty(strField(ui, "organization_id"), strField(ui, "organizationId"))
		orgName = firstNonEmpty(strField(ui, "organization_name"), strField(ui, "organizationName"))
	}
	acct := auth.NewQoderAuth(uid, nickname, token, strField(data, "refresh_token"), "", info.realm, "oauth")
	acct.UserType = userType
	acct.OrganizationID = orgID
	acct.OrganizationName = orgName
	acct.ExpiresAt = deviceExpiry(data)
	if err := m.save(acct); err != nil {
		return LoginResult{Status: "error", Message: "save account: " + err.Error()}
	}
	m.Cancel(state)
	return LoginResult{Status: "ok", Account: acct}
}

// ImportPAT 导入 pt- 个人访问令牌：交换 jobToken 并拉取身份后入库。
func (m *LoginManager) ImportPAT(ctx context.Context, pat, realm string) (*auth.Auth, error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, "pt-") {
		return nil, fmt.Errorf("PAT must start with pt-")
	}
	realm = NormalizeRealm(realm)
	ep := m.client.Endpoint(realm)
	payload, _ := json.Marshal(map[string]string{"personal_token": pat})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(ep.OpenAPI, "/")+pathJobExchange, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultStr(ep.UA, "QoderWork/1.1.75"))
	resp, err := m.client.apiHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jobToken exchange http %d: %s", resp.StatusCode, truncateBytes(body, 200))
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("jobToken exchange decode: %w", err)
	}
	token := strField(data, "token")
	if token == "" {
		return nil, fmt.Errorf("jobToken exchange returned no token")
	}
	uid, nickname, userType := "", "", ""
	orgID, orgName := "", ""
	if ui, err := m.userinfo(ctx, ep, token); err == nil {
		uid = strField(ui, "id")
		nickname = strField(ui, "name")
		userType = strField(ui, "user_type")
		orgID = firstNonEmpty(strField(ui, "organization_id"), strField(ui, "organizationId"))
		orgName = firstNonEmpty(strField(ui, "organization_name"), strField(ui, "organizationName"))
	}
	acct := auth.NewQoderAuth(uid, nickname, token, strField(data, "refresh_token"), pat, realm, "pat")
	acct.UserType = userType
	acct.OrganizationID = orgID
	acct.OrganizationName = orgName
	if secs, ok := intField(data, "expires_in"); ok {
		acct.ExpiresAt = time.Now().Unix() + int64(secs)/1000
	} else {
		acct.ExpiresAt = time.Now().Unix() + 24*3600
	}
	if err := m.save(acct); err != nil {
		return nil, err
	}
	return acct, nil
}

// ImportAccounts 解析并落盘一批 Qoder 账号 JSON（面板粘贴/上传导入）。
func (m *LoginManager) ImportAccounts(payloads [][]byte) ([]*auth.Auth, []error) {
	var out []*auth.Auth
	var errs []error
	for i, raw := range payloads {
		a, err := auth.ParseQoder(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("item %d: %w", i+1, err))
			continue
		}
		if err := m.save(a); err != nil {
			errs = append(errs, fmt.Errorf("item %d: %w", i+1, err))
			continue
		}
		out = append(out, a)
	}
	return out, errs
}

func (m *LoginManager) save(a *auth.Auth) error {
	a.FilePath = auth.QoderFilePath(m.dir, a.UID)
	return a.SaveAtomic()
}

func (m *LoginManager) userinfo(ctx context.Context, ep Endpoint, token string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.OpenAPI, "/")+pathUserinfo, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultStr(ep.UA, "QoderWork/1.1.75"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.client.apiHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo http %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// makePKCE 生成 RFC 7636 S256 (verifier, challenge)。
func makePKCE() (string, string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	raw := make([]byte, 64)
	if _, err := cryptorand.Read(raw); err != nil {
		copy(raw, []byte(randomHex(32)))
	}
	var b strings.Builder
	for _, c := range raw {
		b.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	verifier := b.String()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomNonce(dashed bool) string {
	if dashed {
		return newUUID4()
	}
	return strings.ReplaceAll(newUUID4(), "-", "")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
