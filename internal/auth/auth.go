// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// Provider 标识账号所属上游。
const (
	ProviderWorkBuddy = "workbuddy"
	ProviderQoder     = "qoder"
	ProviderLoomy     = "loomy"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// provider 账号所属上游（空 = workbuddy）。由加载路径决定：
	// workbuddy*.json 恒为 workbuddy，qoder*.json 恒为 qoder。
	provider string
	// PersonalToken 仅 Qoder：pt- 个人访问令牌（refresh 的最终兜底）。
	PersonalToken string
	// UserType / OrganizationID / OrganizationName 仅 Qoder：COSY 身份体字段。
	UserType         string
	OrganizationID   string
	OrganizationName string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// 以下字段仅 Qoder：原样保留以写回与 qoder2api-hub 兼容的账号文件。
	Platform string
	Group    string
	Source   string
	AddedAt  float64
	// VpcEndpoint 企业 VPC 专属实例（qoder*.json 的 vpcEndpoint 键）。
	// 空 = 跟随区域公网端点；非空时该账号的所有 openapi 请求走实例域名。
	VpcEndpoint string

	// ── Loomy 账号（loomy*.json）：手机号 + 密码换 14 天 session ──
	// LoginID 登录手机号（Loomy 账号服务 loginid），Password 明文口令（仅本地 600 文件）。
	// Loomy 无 refresh token，续期等价于用这两个字段重新登录，因此必须落盘。
	LoginID             string `json:"loginid,omitempty"`
	Password            string `json:"password,omitempty"`
	LoomyDevID          string `json:"loomyDevID,omitempty"`
	LoomyCampusDeviceID string `json:"loomyCampusDeviceID,omitempty"`
	// LoomyOnboardingDone 8 项新手任务已全部确认领取（落盘 onboardingDone）。
	// 用途：签到排程每天跑两趟，已领完后不必每趟再打两次「读状态 + 上报」的往返。
	LoomyOnboardingDone bool `json:"loomyOnboardingDone,omitempty"`

	// ── Qoder 运营状态（与 qoder2api-hub 账号文件字段一一对应）──
	LastCheckin          string            `json:"lastCheckin,omitempty"`
	CheckinWhenDisabled  bool              `json:"checkinWhenDisabled,omitempty"`
	Plan                 string            `json:"plan,omitempty"`
	Credits              map[string]any    `json:"credits,omitempty"`
	LastCreditsDelta     int64             `json:"lastCreditsDelta,omitempty"`
	LastCreditsDeltaAt   int64             `json:"lastCreditsDeltaAt,omitempty"`
	CampaignCodes        map[string]string `json:"campaignCodes,omitempty"`
	CampaignBlockedUntil map[string]int64  `json:"campaignBlockedUntil,omitempty"`
}

// QoderState 账号运营状态的快照副本（积分 / 签到 / 活动），供面板与调度器读取。
type QoderState struct {
	LastCheckin          string
	CheckinWhenDisabled  bool
	Plan                 string
	Credits              map[string]any
	LastCreditsDelta     int64
	LastCreditsDeltaAt   int64
	CampaignCodes        map[string]string
	CampaignBlockedUntil map[string]int64
}

// QoderStateSnapshot 返回运营状态的深拷贝（调用方无需再持锁）。
func (a *Auth) QoderStateSnapshot() QoderState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.qoderStateLocked()
}

func (a *Auth) qoderStateLocked() QoderState {
	s := QoderState{
		LastCheckin:         a.LastCheckin,
		CheckinWhenDisabled: a.CheckinWhenDisabled,
		Plan:                a.Plan,
		LastCreditsDelta:    a.LastCreditsDelta,
		LastCreditsDeltaAt:  a.LastCreditsDeltaAt,
	}
	if a.Credits != nil {
		s.Credits = make(map[string]any, len(a.Credits))
		for k, v := range a.Credits {
			s.Credits[k] = v
		}
	}
	if a.CampaignCodes != nil {
		s.CampaignCodes = make(map[string]string, len(a.CampaignCodes))
		for k, v := range a.CampaignCodes {
			s.CampaignCodes[k] = v
		}
	}
	if a.CampaignBlockedUntil != nil {
		s.CampaignBlockedUntil = make(map[string]int64, len(a.CampaignBlockedUntil))
		for k, v := range a.CampaignBlockedUntil {
			s.CampaignBlockedUntil[k] = v
		}
	}
	return s
}

func (a *Auth) applyQoderStateLocked(s QoderState) {
	a.LastCheckin = s.LastCheckin
	a.CheckinWhenDisabled = s.CheckinWhenDisabled
	a.Plan = s.Plan
	a.Credits = s.Credits
	a.LastCreditsDelta = s.LastCreditsDelta
	a.LastCreditsDeltaAt = s.LastCreditsDeltaAt
	a.CampaignCodes = s.CampaignCodes
	a.CampaignBlockedUntil = s.CampaignBlockedUntil
}

// UpdateQoderState 在锁内修改运营状态并原子写回账号文件。
// mut 拿到的是当前状态的副本，修改后整体应用；返回落盘错误（状态本身已生效）。
func (a *Auth) UpdateQoderState(mut func(*QoderState)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.qoderStateLocked()
	mut(&s)
	a.applyQoderStateLocked(s)
	if a.FilePath == "" {
		return nil
	}
	return a.saveByProviderLocked()
}

// UpdateCheckinState 把签到盖章时间写回账号文件（provider 仅用于日志与语义标注，落盘
// 形状由账号自身的 provider 决定）。Loomy 与 Qoder 共用 LastCheckin 字段与
// CheckedInToday 判定，因此「当天是否已签到」在两类账号上是同一口径。
func (a *Auth) UpdateCheckinState(provider, ts string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if provider != "" && provider != a.Provider() {
		// 上游归类不一致不阻断（配置换代时的历史账号），仍按账号自身形状落盘。
		log.Printf("WARN: auth %s checkin provider mismatch: want=%s have=%s", logfmt.UID8(a.UID), provider, a.Provider())
	}
	a.LastCheckin = ts
	if a.FilePath == "" {
		return nil
	}
	return a.saveByProviderLocked()
}

// UpdateLoomyCredits 更新 Loomy 账号的积分快照并落盘（面板展示 + 冷却恢复依据）。
func (a *Auth) UpdateLoomyCredits(m map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Credits = m
	if a.FilePath == "" {
		return nil
	}
	return a.saveByProviderLocked()
}

// OnboardingComplete 报告 Loomy 新手任务是否已全部确认领取。
func (a *Auth) OnboardingComplete() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.LoomyOnboardingDone
}

// SetOnboardingComplete 记录新手任务领取进度并落盘。
func (a *Auth) SetOnboardingComplete(v bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LoomyOnboardingDone = v
	if a.FilePath == "" {
		return nil
	}
	return a.saveByProviderLocked()
}

// saveByProviderLocked 按账号上游选择落盘形状（调用方已持 a.mu）。
func (a *Auth) saveByProviderLocked() error {
	switch a.Provider() {
	case ProviderQoder:
		return a.saveQoderLocked()
	case ProviderLoomy:
		return a.saveLoomyLocked()
	}
	return a.saveWorkBuddyLocked()
}

// CheckedInToday 报告本地是否已为 now 所在自然日盖章签到。
func (a *Auth) CheckedInToday(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.HasPrefix(a.LastCheckin, now.Format("2006-01-02"))
}

// CreditsRemain 返回最近一次余额快照的有效剩余积分（无快照返回 0）。
func (a *Auth) CreditsRemain() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return creditsRemainOf(a.Credits)
}

func creditsRemainOf(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	for _, k := range []string{"remain_effective", "remain"} {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return int64(n)
			case int64:
				return n
			case int:
				return int64(n)
			}
		}
	}
	return 0
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Provider 返回账号所属上游（空 = workbuddy）。
func (a *Auth) Provider() string {
	if a.provider == "" {
		return ProviderWorkBuddy
	}
	return a.provider
}

// SetProvider 设置账号上游（反序列化/测试用）。
func (a *Auth) SetProvider(p string) { a.provider = p }

// IsQoder 报告账号是否为 Qoder 上游。
func (a *Auth) IsQoder() bool { return a.Provider() == ProviderQoder }

// IsLoomy 报告账号是否为 Loomy 上游。
func (a *Auth) IsLoomy() bool { return a.Provider() == ProviderLoomy }

// Realm 返回账号的归一化域：
//   - Qoder：cn / intl（来自 qoder*.json 的 realm 字段，缺省 cn），不受 global 逃生门影响；
//   - WorkBuddy：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，否则 "cn"。
//
// 显式 global 优先于 domain 回落（D1）。WorkBuddy 全局开关 SetGlobalEnabled(false)
// 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。空 realm + 空 domain → "cn"。
func (a *Auth) Realm() string {
	// Loomy 只有国内站（loomyad.xunfei.cn），恒 cn，不受 global 逃生门影响。
	if a.Provider() == ProviderLoomy {
		return "cn"
	}
	if a.Provider() == ProviderQoder {
		if strings.EqualFold(strings.TrimSpace(a.realm), "intl") ||
			strings.EqualFold(strings.TrimSpace(a.realm), "global") {
			return "intl"
		}
		return "cn"
	}
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string { return a.realm }

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	if a.Provider() == ProviderQoder || a.Provider() == ProviderLoomy {
		return a.saveByProviderLocked()
	}
	return a.saveWorkBuddyLocked()
}

// saveWorkBuddyLocked 以嵌套形写回（调用方已持 a.mu）。
func (a *Auth) saveWorkBuddyLocked() error {
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// saveQoderLocked 以 qoder2api-hub 账号文件形状写回（调用方已持 a.mu）。
// 保持字段名与 Python 侧一致，使同一份账号文件可在两个网关间直接复用。
func (a *Auth) saveQoderLocked() error {
	// 先读回原文件，保留 credits / 签到等本网关不管理的键（qoder2api-hub 可能仍在用）。
	doc := map[string]any{}
	if old, err := os.ReadFile(a.FilePath); err == nil {
		_ = json.Unmarshal(old, &doc)
	}
	for k, v := range map[string]any{
		"uid":                 a.UID,
		"nickname":            a.Nickname,
		"group":               a.Group,
		"domain":              a.Domain,
		"realm":               a.Realm(),
		"platform":            a.Platform,
		"accessToken":         a.AccessToken,
		"refreshToken":        a.RefreshToken,
		"personalToken":       a.PersonalToken,
		"expiresAt":           a.ExpiresAt,
		"addedAt":             a.AddedAt,
		"source":              a.Source,
		"enabled":             true,
		"userType":            a.UserType,
		"organizationId":      a.OrganizationID,
		"organizationName":    a.OrganizationName,
		"plan":                a.Plan,
		"lastCheckin":         a.LastCheckin,
		"checkinWhenDisabled": bool(a.CheckinWhenDisabled),
		"lastCreditsDelta":    a.LastCreditsDelta,
		"lastCreditsDeltaAt":  a.LastCreditsDeltaAt,
	} {
		doc[k] = v
	}
	if a.VpcEndpoint != "" {
		doc["vpcEndpoint"] = a.VpcEndpoint
	}
	// 运营快照非空才写键：保留 Python 侧对「缺键 = 未知」的判定语义，
	// 也避免在纯凭证文件里塞空对象。
	if a.Credits != nil {
		doc["credits"] = a.Credits
	}
	if a.CampaignCodes != nil {
		doc["campaignCodes"] = a.CampaignCodes
	}
	if a.CampaignBlockedUntil != nil {
		doc["campaignBlockedUntil"] = a.CampaignBlockedUntil
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// qoderFile Qoder 账号文件形状（qoder2api-hub accounts/*.json）。
type qoderFile struct {
	UID                  string            `json:"uid"`
	Nickname             string            `json:"nickname"`
	Group                string            `json:"group"`
	Domain               string            `json:"domain"`
	Realm                string            `json:"realm"`
	VpcEndpoint          string            `json:"vpcEndpoint"`
	Platform             string            `json:"platform"`
	AccessToken          string            `json:"accessToken"`
	RefreshToken         string            `json:"refreshToken"`
	PersonalToken        string            `json:"personalToken"`
	ExpiresAt            int64             `json:"expiresAt"`
	AddedAt              float64           `json:"addedAt"`
	Source               string            `json:"source"`
	Enabled              *bool             `json:"enabled"`
	UserType             string            `json:"userType"`
	OrganizationID       string            `json:"organizationId"`
	OrganizationName     string            `json:"organizationName"`
	Plan                 string            `json:"plan"`
	LastCheckin          string            `json:"lastCheckin"`
	CheckinWhenDisabled  bool              `json:"checkinWhenDisabled"`
	Credits              map[string]any    `json:"credits"`
	LastCreditsDelta     int64             `json:"lastCreditsDelta"`
	LastCreditsDeltaAt   int64             `json:"lastCreditsDeltaAt"`
	CampaignCodes        map[string]string `json:"campaignCodes"`
	CampaignBlockedUntil map[string]int64  `json:"campaignBlockedUntil"`
}

// ParseQoder 解析 qoder2api-hub 形状的账号文件。
func ParseQoder(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty qoder auth file")
	}
	var f qoderFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if strings.TrimSpace(f.AccessToken) == "" && strings.TrimSpace(f.PersonalToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken/personalToken")
	}
	realm := strings.ToLower(strings.TrimSpace(f.Realm))
	if realm == "" {
		realm = detectQoderRealm(f.Domain)
	}
	if realm == "global" {
		realm = "intl"
	}
	if realm != "intl" {
		realm = "cn"
	}
	a := &Auth{
		AccessToken:          f.AccessToken,
		RefreshToken:         f.RefreshToken,
		ExpiresAt:            f.ExpiresAt,
		Domain:               f.Domain,
		realm:                realm,
		provider:             ProviderQoder,
		PersonalToken:        f.PersonalToken,
		UserType:             f.UserType,
		OrganizationID:       f.OrganizationID,
		OrganizationName:     f.OrganizationName,
		UID:                  f.UID,
		Nickname:             f.Nickname,
		Platform:             f.Platform,
		Group:                f.Group,
		Source:               f.Source,
		AddedAt:              f.AddedAt,
		VpcEndpoint:          f.VpcEndpoint,
		Plan:                 f.Plan,
		LastCheckin:          f.LastCheckin,
		CheckinWhenDisabled:  f.CheckinWhenDisabled,
		Credits:              f.Credits,
		LastCreditsDelta:     f.LastCreditsDelta,
		LastCreditsDeltaAt:   f.LastCreditsDeltaAt,
		CampaignCodes:        f.CampaignCodes,
		CampaignBlockedUntil: f.CampaignBlockedUntil,
	}
	if a.Platform == "" {
		a.Platform = "CLI"
	}
	if a.Source == "" {
		a.Source = "oauth"
	}
	if a.AddedAt == 0 {
		a.AddedAt = float64(time.Now().UnixNano()) / 1e9
	}
	if a.UID == "" {
		// 缺 uid 的旧文件：用 token 派生的短标识占位，保持账号唯一可寻址。
		a.UID = "q-" + shortHash(a.AccessToken+a.PersonalToken)
	}
	if a.Nickname == "" {
		a.Nickname = "u" + tail(a.UID, 8)
	}
	return a, nil
}

// NewQoderAuth 构造一个新 Qoder 账号（登录/导入路径使用）。
func NewQoderAuth(uid, nickname, accessToken, refreshToken, personalToken, realm, source string) *Auth {
	realm = strings.ToLower(strings.TrimSpace(realm))
	if realm != "intl" {
		realm = "cn"
	}
	domain := "qoder.com.cn"
	if realm == "intl" {
		domain = "qoder.com"
	}
	if uid == "" {
		uid = "q-" + shortHash(accessToken+personalToken+fmt.Sprint(time.Now().UnixNano()))
	}
	if nickname == "" {
		nickname = "u" + tail(uid, 8)
	}
	return &Auth{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		PersonalToken: personalToken,
		realm:         realm,
		provider:      ProviderQoder,
		Domain:        domain,
		UID:           uid,
		Nickname:      nickname,
		Platform:      "CLI",
		Source:        source,
		AddedAt:       float64(time.Now().UnixNano()) / 1e9,
	}
}

// LoadQoderDir 扫描 dir 下 qoder*.json；解析失败的文件静默跳过。
func LoadQoderDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qoder*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := ParseQoder(raw)
		if err != nil {
			log.Printf("WARN: qoder auth %s skipped: %v", f, err)
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// loomyFile Loomy 账号文件形状（loomy*.json）。
//
// 与 Qoder/WorkBuddy 不同，Loomy 没有 refresh token：14 天 session 到期只能重新登录，
// 所以 loginid + password 必须与 session 一起落盘（文件权限 600，与其它账号文件一致）。
type loomyFile struct {
	UID         string         `json:"uid"`
	Nickname    string         `json:"nickname"`
	LoginID     string         `json:"loginid"`
	Password    string         `json:"password"`
	Session     string         `json:"accessToken"`
	ExpiresAt   int64          `json:"expiresAt"`
	AddedAt     float64        `json:"addedAt"`
	Source      string         `json:"source"`
	Enabled     *bool          `json:"enabled"`
	LastCheckin string         `json:"lastCheckin"`
	Credits     map[string]any `json:"credits"`
	// OnboardingDone 记录 8 项新手任务是否已全部确认领取，避免已领完还反复打上游。
	OnboardingDone bool `json:"onboardingDone"`
	Identity       struct {
		DevID          string `json:"devid"`
		CampusDeviceID string `json:"campus_device_id"`
	} `json:"identity"`
	// 兼容 loomy2api 的扁平写法（devid / campus_device_id 直接在顶层）。
	DevID          string `json:"devid"`
	CampusDeviceID string `json:"campus_device_id"`
}

// ParseLoomy 解析 Loomy 账号文件：必须有 session 或有可重登的 loginid+password。
func ParseLoomy(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty loomy auth file")
	}
	var f loomyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	session := strings.TrimSpace(f.Session)
	loginID := strings.TrimSpace(f.LoginID)
	password := f.Password
	if session == "" && (loginID == "" || password == "") {
		return nil, fmt.Errorf("parse_error: missing accessToken or loginid/password")
	}
	a := &Auth{
		AccessToken:         session,
		ExpiresAt:           f.ExpiresAt,
		provider:            ProviderLoomy,
		realm:               "cn",
		UID:                 strings.TrimSpace(f.UID),
		Nickname:            f.Nickname,
		LoginID:             loginID,
		Password:            password,
		LoomyDevID:          firstNonEmptyStr(f.Identity.DevID, f.DevID),
		LoomyCampusDeviceID: firstNonEmptyStr(f.Identity.CampusDeviceID, f.CampusDeviceID),
		Source:              f.Source,
		AddedAt:             f.AddedAt,
		LastCheckin:         f.LastCheckin,
		Credits:             f.Credits,
		LoomyOnboardingDone: f.OnboardingDone,
	}
	if a.LoomyDevID == "" {
		a.LoomyDevID = "web-" + shortHash(session+loginID)
	}
	if a.Source == "" {
		a.Source = "login"
	}
	if a.AddedAt == 0 {
		a.AddedAt = float64(time.Now().UnixNano()) / 1e9
	}
	if a.UID == "" {
		a.UID = "l-" + shortHash(loginID+session)
	}
	if a.Nickname == "" {
		a.Nickname = shortUIDName(loginID, a.UID)
	}
	return a, nil
}

// shortUIDName 昵称缺省值：优先脱敏手机号（138****8000），否则 uid 尾 8 位。
func shortUIDName(loginID, uid string) string {
	if len(loginID) == 11 {
		for _, r := range loginID {
			if r < '0' || r > '9' {
				return "u" + tail(uid, 8)
			}
		}
		return loginID[:3] + "****" + loginID[7:]
	}
	return "u" + tail(uid, 8)
}

// NewLoomyAuth 构造一个新 Loomy 账号（登录 / 手工导入路径使用）。
func NewLoomyAuth(uid, nickname, loginID, password, session string, expireAt int64) *Auth {
	if uid == "" {
		uid = "l-" + shortHash(loginID+session)
	}
	if nickname == "" {
		nickname = shortUIDName(loginID, uid)
	}
	return &Auth{
		AccessToken: session,
		ExpiresAt:   expireAt,
		provider:    ProviderLoomy,
		realm:       "cn",
		UID:         uid,
		Nickname:    nickname,
		LoginID:     loginID,
		Password:    password,
		Source:      "login",
		AddedAt:     float64(time.Now().UnixNano()) / 1e9,
	}
}

// LoomyFilePath 返回账号在 dir 下的落盘路径（loomy-<safe uid>.json）。
func LoomyFilePath(dir, uid string) string {
	return filepath.Join(dir, "loomy-"+safeFilename(uid)+".json")
}

// LoadLoomyDir 扫描 dir 下 loomy*.json；解析失败的文件静默告警跳过。
func LoadLoomyDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "loomy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := ParseLoomy(raw)
		if err != nil {
			log.Printf("WARN: loomy auth %s skipped: %v", f, err)
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// saveLoomyLocked 以 Loomy 账号文件形状写回（调用方已持 a.mu）。
// 读回原文件再覆盖本网关管理的键，保留人工加的注释性字段（如 remark）。
func (a *Auth) saveLoomyLocked() error {
	doc := map[string]any{}
	if old, err := os.ReadFile(a.FilePath); err == nil {
		_ = json.Unmarshal(old, &doc)
	}
	for k, v := range map[string]any{
		"uid":         a.UID,
		"nickname":    a.Nickname,
		"loginid":     a.LoginID,
		"password":    a.Password,
		"accessToken": a.AccessToken,
		"expiresAt":   a.ExpiresAt,
		"addedAt":     a.AddedAt,
		"source":      a.Source,
		"enabled":     true,
		"lastCheckin": a.LastCheckin,
	} {
		doc[k] = v
	}
	if a.LoomyOnboardingDone {
		doc["onboardingDone"] = true
	}
	ident, _ := doc["identity"].(map[string]any)
	if ident == nil {
		ident = map[string]any{}
	}
	if a.LoomyDevID != "" {
		ident["devid"] = a.LoomyDevID
	}
	if a.LoomyCampusDeviceID != "" {
		ident["campus_device_id"] = a.LoomyCampusDeviceID
	}
	doc["identity"] = ident
	if a.Credits != nil {
		doc["credits"] = a.Credits
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// safeFilename 把 uid 归一化成可做文件名的短标识。
func safeFilename(uid string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, uid)
	safe = strings.Trim(safe, "_")
	if safe == "" {
		safe = shortHash(uid)
	}
	return safe
}

// QoderFilePath 返回账号在 dir 下的落盘路径（qoder-<safe uid>.json）。
func QoderFilePath(dir, uid string) string {
	return filepath.Join(dir, "qoder-"+safeFilename(uid)+".json")
}

// firstNonEmptyStr 返回第一个非空字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// detectQoderRealm 按 domain 推断 Qoder 区域（qoder.com.cn / 其它 CN 域 → cn）。
func detectQoderRealm(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if strings.Contains(d, "qoder.sh") || (strings.HasSuffix(d, "qoder.com") && !strings.Contains(d, "qoder.com.cn")) {
		return "intl"
	}
	return "cn"
}

func shortHash(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.UID8(a.UID), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.UID8(a.UID), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.UID8(a.UID), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
