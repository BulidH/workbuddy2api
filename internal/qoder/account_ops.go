// account_ops.go Qoder 账号运营：积分余额、套餐、每日签到、活动平台领取。
//
// 对齐 qoder2api-hub 的 qoder_accounts.py：全部走 openapi 基址 + 双区候选，
// 账号文件字段（credits / lastCheckin / campaignCodes ...）与 Python 侧互通。
package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	pathQuota          = "/api/v2/quota/usage"
	pathPlan           = "/api/v2/user/plan"
	pathCheckinStatus  = "/sash/api/v1/me/daily-check-in/status"
	pathCheckinClaim   = "/sash/api/v1/me/daily-check-in/claim"
	pathCampaigns      = "/sash/api/v1/me/campaigns"
	pathCampaignReward = "/sash/api/v1/me/campaigns/%s/reward"
	pathCampaignClaim  = "/sash/api/v1/me/campaigns/%s/claim"

	desktopClientType    = "10"
	defaultDesktopVer    = "0.4.8"
	checkinWindowHourCST = 10
)

var utc8 = time.FixedZone("UTC+8", 8*3600)

// ── 余额快照 ──────────────────────────────────────────────────────────────

// QuotaBucket 额度构成中的一项（套餐/资源包/SOTA/共享池）。
type QuotaBucket struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Remain    int64  `json:"remain"`
	Used      int64  `json:"used"`
	Size      int64  `json:"size"`
	Available *bool  `json:"available,omitempty"`
	Unlimited bool   `json:"unlimited,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// Credits 账号余额快照（字段名与 qoder2api-hub 落盘格式一致，供面板直读）。
type Credits struct {
	Remain          int64         `json:"remain"`
	RemainEffective int64         `json:"remain_effective"`
	Used            int64         `json:"used"`
	Size            int64         `json:"size"`
	Exceeded        bool          `json:"exceeded"`
	UsagePct        float64       `json:"usage_pct,omitempty"`
	ExpiresAt       int64         `json:"expires_at,omitempty"`
	Shared          *QuotaBucket  `json:"shared,omitempty"`
	Packages        []QuotaBucket `json:"packages"`
	Composition     []QuotaBucket `json:"composition,omitempty"`
	Source          string        `json:"source"`
	UpdatedAt       int64         `json:"updated_at"`
	UpdatedISO      string        `json:"updated_iso"`
}

// Map 转成可写入账号文件的通用形态。
func (c *Credits) Map() map[string]any {
	raw, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// ── 签到 / 活动结果 ───────────────────────────────────────────────────────

// CheckinStatus 旧 sash 签到接口的状态。
type CheckinStatus struct {
	Available         bool
	Unavailable       bool
	Status            string
	Active            bool
	TodayCheckedIn    bool
	StreakDays        int64
	TotalClaimDays    int64
	RewardCredits     int64
	TotalRewardCredit int64
	NextClaimAt       int64
	LastClaimedAt     int64
	RewardExpiresAt   int64
	Reason            string
	HTTP              int
}

// CheckinResult 单次签到结论（活动平台 + sash 兜底）。
type CheckinResult struct {
	OK             bool     `json:"ok"`
	Already        bool     `json:"already,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
	Unavailable    bool     `json:"unavailable,omitempty"`
	Blocked        bool     `json:"blocked,omitempty"`
	Earned         int64    `json:"earned_credit,omitempty"`
	RewardCredits  int64    `json:"reward_credits,omitempty"`
	StreakDays     int64    `json:"streak_days,omitempty"`
	Message        string   `json:"message,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Claimed        []string `json:"claimed,omitempty"`
	BlockedCodes   []string `json:"blocked_codes,omitempty"`
	Pending        []string `json:"pending,omitempty"`
	Locked         []string `json:"locked,omitempty"`
	Codes          []Code   `json:"codes,omitempty"`
	NextAvailable  int64    `json:"next_available_at,omitempty"`
	NextAvailNote  string   `json:"next_available_note,omitempty"`
	Credits        *Credits `json:"-"`
	CreditRemain   int64    `json:"credits,omitempty"`
	CampaignFailed bool     `json:"-"`
	Error          string   `json:"error,omitempty"`
}

// Code 兑换码类奖励。
type Code struct {
	Campaign string `json:"campaign"`
	Code     string `json:"code"`
}

// Campaign 活动平台活动。
type Campaign struct {
	ID                   string `json:"id"`
	Key                  string `json:"key"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	DetailURL            string `json:"detail_url"`
	ButtonText           string `json:"button_text"`
	ActionType           string `json:"action_type"`
	ClaimStatus          string `json:"claim_status"`
	BenefitKind          string `json:"benefit_kind"`
	BenefitAmount        int64  `json:"benefit_amount"`
	StartAt              int64  `json:"start_at,omitempty"`
	EndAt                int64  `json:"end_at,omitempty"`
	RequiredAchievement  string `json:"required_achievement_key,omitempty"`
	AchievementCompleted *bool  `json:"achievement_completed,omitempty"`
	UnavailableReason    string `json:"unavailable_reason,omitempty"`
}

// CampaignsResult 活动列表查询结果。
type CampaignsResult struct {
	OK            bool       `json:"ok"`
	Available     bool       `json:"available"`
	ShowCampaign  bool       `json:"show_campaign"`
	Claimable     bool       `json:"claimable"`
	CampaignURL   string     `json:"campaign_url,omitempty"`
	Campaigns     []Campaign `json:"campaigns"`
	Identity      string     `json:"identity,omitempty"`
	MachineHeader string     `json:"machine_headers,omitempty"`
	Hint          string     `json:"hint,omitempty"`
	Error         string     `json:"error,omitempty"`
	HTTP          int        `json:"-"`
}

// ClaimResult 单个活动领取结果。
type ClaimResult struct {
	OK             bool   `json:"ok"`
	Blocked        bool   `json:"blocked,omitempty"`
	Replayed       bool   `json:"replayed,omitempty"`
	Status         string `json:"status,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	GrantID        string `json:"grant_id,omitempty"`
	RedemptionCode string `json:"redemption_code,omitempty"`
	Confirming     bool   `json:"confirming,omitempty"`
	Amount         int64  `json:"amount,omitempty"`
	Message        string `json:"message,omitempty"`
	Error          string `json:"error,omitempty"`
}

// CampaignOutcome 活动批量签到结论。
type CampaignOutcome struct {
	OK             bool
	Claimed        []Campaign
	Already        []Campaign
	Blocked        []map[string]any
	Pending        []Campaign
	Locked         []Campaign
	Views          []Campaign
	Errors         []string
	Codes          []Code
	Earned         int64
	Message        string
	NextAvailable  int64
	NextAvailNote  string
	CampaignsState *CampaignsResult
}

// ── HTTP 头 ───────────────────────────────────────────────────────────────

// EndpointFor 返回账号实际使用的端点（企业 VPC 覆盖 > 区域缺省）。
func (c *Client) EndpointFor(a *auth.Auth) Endpoint {
	ep := c.Endpoint(c.Realm(a))
	if v := strings.TrimSpace(a.VpcEndpoint); v != "" {
		if parsed, err := ParseVPCEndpoint(v); err == nil {
			ep.OpenAPI = parsed.OpenAPI
			ep.Gateway = parsed.Gateway
			ep.GatewayFallbacks = nil
			ep.Website = parsed.Website
			ep.Domain = parsed.Domain
		}
	}
	return ep
}

// apiBases openapi 基址候选：VPC 只走专属域；公网依次为 openapi、
// intl 活动中心、openapi.<domain>、website，仅在连接失败/404 时切换。
func (c *Client) apiBases(a *auth.Auth) []string {
	ep := c.EndpointFor(a)
	if strings.TrimSpace(a.VpcEndpoint) != "" && ep.Domain != "" && strings.HasSuffix(ep.Domain, ".vpc.qoder.com.cn") {
		return []string{strings.TrimRight(ep.OpenAPI, "/")}
	}
	out := []string{}
	add := func(v string) {
		v = strings.TrimRight(strings.TrimSpace(v), "/")
		if v == "" {
			return
		}
		for _, x := range out {
			if x == v {
				return
			}
		}
		out = append(out, v)
	}
	add(ep.OpenAPI)
	if c.Realm(a) == "intl" {
		add("https://center.qoder.sh")
	}
	if ep.Domain != "" {
		add("https://openapi." + ep.Domain)
	}
	add(ep.Website)
	return out
}

func (c *Client) openAPIHeaders(a *auth.Auth, ep Endpoint) map[string]string {
	return map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json, text/plain, */*",
		"User-Agent":    defaultStr(ep.UA, "QoderWork/1.1.75"),
		"Authorization": "Bearer " + a.AccessToken,
		"X-Request-ID":  newUUID4Hex(),
		"X-Machine-ID":  deriveID(a.UID, "machine"),
		"X-Session-ID":  deriveID(a.UID, "session"),
		"Origin":        ep.Website,
		"Referer":       strings.TrimRight(ep.Website, "/") + "/",
	}
}

// desktopHeaders 桌面端同款头（活动平台必需）。派生身份账号不发 cosy-machine*
// 六头（上游会把派生六头判为非官方客户端并过滤可领取活动，issue #10）；
// minimal=true 时只补 os+hostname，用于 showCampaign=false 的自愈重试。
func (c *Client) desktopHeaders(a *auth.Auth, minimal bool) map[string]string {
	ver := c.cfg.DesktopVersion
	if ver == "" {
		ver = defaultDesktopVer
	}
	h := map[string]string{
		"Accept":          "application/json",
		"User-Agent":      "Qoder",
		"Authorization":   "Bearer " + a.AccessToken,
		"cosy-clienttype": desktopClientType,
		"cosy-version":    ver,
	}
	if minimal {
		h["cosy-machineos"] = defaultMachineOS()
		if host := machineHostname(a.UID); host != "" {
			h["cosy-machinehostname"] = host
		}
	}
	return h
}

// ── 通用 openapi 调用 ─────────────────────────────────────────────────────

// apiCall 在基址候选上依次请求；连接失败或 404/405/410 才换下一个。
func (c *Client) apiCall(ctx context.Context, a *auth.Auth, method, path string, headers map[string]string, body []byte) (any, int, string) {
	var lastStatus int
	var lastBody string
	for _, base := range c.apiBases(a) {
		rawURL := base + path
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
		if err != nil {
			lastBody = err.Error()
			continue
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.apiHTTP.Do(req)
		if err != nil {
			lastBody = err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		lastStatus, lastBody = resp.StatusCode, string(raw)
		if resp.StatusCode == 404 || resp.StatusCode == 405 || resp.StatusCode == 410 {
			continue
		}
		var payload any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, resp.StatusCode, string(raw)
			}
		}
		return payload, resp.StatusCode, string(raw)
	}
	return nil, lastStatus, lastBody
}

func (c *Client) apiGet(ctx context.Context, a *auth.Auth, path string) (any, int, string) {
	return c.apiCall(ctx, a, http.MethodGet, path, c.openAPIHeaders(a, c.EndpointFor(a)), nil)
}

func (c *Client) apiPost(ctx context.Context, a *auth.Auth, path string) (any, int, string) {
	return c.apiCall(ctx, a, http.MethodPost, path, c.openAPIHeaders(a, c.EndpointFor(a)), []byte("{}"))
}

// ── 余额 / 套餐 ───────────────────────────────────────────────────────────

// FetchCredits 查询余额并写回账号文件；返回快照与相对上次的有效余额变化。
func (c *Client) FetchCredits(ctx context.Context, a *auth.Auth) (*Credits, error) {
	before := a.QoderStateSnapshot().Credits
	payload, status, raw := c.apiGet(ctx, a, pathQuota)
	if status == 401 || status == 403 {
		// 桌面端头再试一次（WAF/出口拦截时官方客户端头常能通过）。
		payload, status, raw = c.apiCall(ctx, a, http.MethodGet, pathQuota,
			c.desktopHeaders(a, false), nil)
	}
	q, _ := payload.(map[string]any)
	if q == nil {
		return nil, fmt.Errorf("qoder quota http %d: %s", status, truncateBytes([]byte(raw), 160))
	}
	credits, err := normalizeQuota(q, raw)
	if err != nil {
		// 退化体：保留旧快照，错因透出面板。
		return nil, err
	}
	credits.Source = c.EndpointFor(a).OpenAPI + pathQuota
	now := time.Now()
	credits.UpdatedAt = now.Unix()
	credits.UpdatedISO = now.Format("2006-01-02 15:04:05")

	prevRemain := creditsRemainMap(before)
	delta := credits.RemainEffective - prevRemain
	_ = a.UpdateQoderState(func(s *auth.QoderState) {
		s.Credits = credits.Map()
		if before != nil && delta != 0 {
			s.LastCreditsDelta = delta
			s.LastCreditsDeltaAt = now.Unix()
		}
	})
	return credits, nil
}

// FetchPlan 查询套餐名（Pro Trial 等），失败返回旧值。
func (c *Client) FetchPlan(ctx context.Context, a *auth.Auth) string {
	payload, _, _ := c.apiGet(ctx, a, pathPlan)
	m, _ := payload.(map[string]any)
	if m == nil {
		return a.QoderStateSnapshot().Plan
	}
	plan := strAny(m["planName"])
	if plan == "" {
		plan = strAny(m["plan"])
	}
	if plan == "" {
		plan = strAny(m["name"])
	}
	if plan != "" {
		_ = a.UpdateQoderState(func(s *auth.QoderState) { s.Plan = plan })
	}
	return plan
}

func normalizeQuota(q map[string]any, raw string) (*Credits, error) {
	uq, _ := q["userQuota"].(map[string]any)
	aq, _ := q["addOnQuota"].(map[string]any)
	comp := quotaComposition(q)
	remain := int64(numOr(uq, "remaining") + numOr(aq, "remaining"))
	used := int64(numOr(uq, "used") + numOr(aq, "used"))
	size := int64(numOr(uq, "total") + numOr(aq, "total"))
	if len(comp) > 0 {
		var cr, cu, cs int64
		for _, b := range comp {
			if b.Kind == "shared" {
				continue
			}
			if b.Available != nil && !*b.Available {
				continue
			}
			cr += b.Remain
			cu += b.Used
			cs += b.Size
		}
		if cr != 0 || cs != 0 {
			remain, used, size = cr, cu, cs
		}
	}
	bothMissing := uq == nil && aq == nil
	if bothMissing && len(comp) == 0 {
		return nil, fmt.Errorf("degenerate quota body (both quota objects missing): %s", truncateBytes([]byte(raw), 120))
	}
	if boolAny(q["isQuotaExceeded"]) && remain == 0 && size == 0 && !hasShared(comp) {
		return nil, fmt.Errorf("degenerate quota body (exceeded=true with zero size/remain)")
	}

	out := &Credits{
		Remain:      remain,
		Used:        used,
		Size:        size,
		Exceeded:    boolAny(q["isQuotaExceeded"]),
		UsagePct:    numOr(q, "totalUsagePercentage"),
		ExpiresAt:   epochAny(q["expiresAt"]),
		Packages:    []QuotaBucket{},
		Composition: comp,
	}
	base := QuotaBucket{Name: "基础额度", Kind: "plan", Remain: int64(numOr(uq, "remaining")), Used: int64(numOr(uq, "used")), Size: int64(numOr(uq, "total"))}
	bonus := QuotaBucket{Name: "赠送/签到额度", Kind: "pack", Remain: int64(numOr(aq, "remaining")), Used: int64(numOr(aq, "used")), Size: int64(numOr(aq, "total"))}
	out.Packages = append(out.Packages, base, bonus)
	for _, key := range []string{"orgResourcePackage", "sharedQuota", "teamQuota", "poolQuota", "sharedCredits", "sharedCredit"} {
		v, ok := q[key]
		if !ok {
			continue
		}
		if b, ok := bucketFrom(v, "shared", "共享额度池"); ok {
			b.Name = "共享额度"
			out.Shared = &b
			out.Packages = append(out.Packages, b)
			break
		}
	}
	sharedAvail := int64(0)
	if out.Shared != nil && !out.Shared.Unlimited {
		if out.Shared.Available == nil || *out.Shared.Available {
			sharedAvail = out.Shared.Remain
		}
	}
	out.RemainEffective = remain + sharedAvail
	return out, nil
}

func hasShared(bs []QuotaBucket) bool {
	for _, b := range bs {
		if b.Kind == "shared" {
			return true
		}
	}
	return false
}

// quotaComposition 广度优先扫描已知额度容器（套餐 / SOTA / 资源包 / 共享池）。
func quotaComposition(payload map[string]any) []QuotaBucket {
	bucketKeys := []struct {
		keys  []string
		kind  string
		label string
	}{
		{[]string{"userquota", "planquota", "subscriptionquota", "basequota", "plan"}, "plan", "套餐内 Credits"},
		{[]string{"sota", "sotacredits", "sotaquota", "sotaspecial"}, "sota", "SOTA 专属积分"},
		{[]string{"addonquota", "resourcepackage", "resourcepackages", "addon", "package", "packages", "bonus"}, "pack", "资源包"},
		{[]string{"dedicatedresourcepackages", "dedicatedresourcepackage", "exclusivepackages", "sotapackages"}, "sota", "专属积分包"},
		{[]string{"orgresourcepackage", "sharedquota", "teamquota", "poolquota", "sharedcredits"}, "shared", "共享额度池"},
	}
	var found []QuotaBucket
	seen := map[string]bool{}
	queue := []any{payload}
	for depth := 0; len(queue) > 0 && depth < 8; depth++ {
		next := []any{}
		for _, node := range queue {
			m, ok := node.(map[string]any)
			if !ok {
				if list, ok := node.([]any); ok {
					next = append(next, list...)
				}
				continue
			}
			for k, v := range m {
				low := strings.ToLower(k)
				for _, bk := range bucketKeys {
					if !containsStr(bk.keys, low) {
						continue
					}
					items := []any{v}
					if list, ok := v.([]any); ok {
						items = list
					}
					for i, item := range items {
						b, ok := bucketFrom(item, bk.kind, bk.label)
						if !ok {
							continue
						}
						if len(items) > 1 {
							b.Name = fmt.Sprintf("%s #%d", bk.label, i+1)
						}
						sig := fmt.Sprintf("%s|%d|%d|%d", b.Kind, b.Size, b.Remain, b.ExpiresAt)
						if seen[sig] {
							continue
						}
						seen[sig] = true
						found = append(found, b)
					}
					break
				}
				if v != nil {
					switch v.(type) {
					case map[string]any, []any:
						next = append(next, v)
					}
				}
			}
		}
		queue = next
	}
	return found
}

func bucketFrom(src any, kind, name string) (QuotaBucket, bool) {
	if n, ok := toFloat(src); ok {
		return QuotaBucket{Name: name, Kind: kind, Remain: int64(n), Size: int64(n)}, true
	}
	m, ok := src.(map[string]any)
	if !ok {
		return QuotaBucket{}, false
	}
	remain, hasRemain := firstNum(m, "remaining", "remain", "balance")
	used, _ := firstNum(m, "used")
	size, hasSize := firstNum(m, "cap")
	if kind != "shared" || !hasSize {
		if v, ok := firstNum(m, "total"); ok {
			size, hasSize = v, true
		}
	}
	if !hasSize {
		if v, ok := firstNum(m, "cap"); ok {
			size, hasSize = v, true
		}
	}
	if !hasSize {
		if v, ok := firstNum(m, "size"); ok {
			size, hasSize = v, true
		}
	}
	if !hasSize && hasRemain {
		size, hasSize = remain+used, true
	}
	if !hasRemain && hasSize {
		remain, hasRemain = maxf(0, size-used), true
	}
	if !hasRemain && !hasSize {
		return QuotaBucket{}, false
	}
	b := QuotaBucket{Name: name, Kind: kind, Remain: int64(remain), Used: int64(used), Size: int64(size)}
	if v, ok := m["available"]; ok {
		av := truthyAny(v, true)
		b.Available = &av
	}
	if kind == "shared" && b.Size < 0 {
		b.Unlimited = true
	}
	for _, k := range []string{"expiresAt", "expireAt", "expires_at", "expiredAt", "validUntil", "dueAt"} {
		if v, ok := m[k]; ok {
			b.ExpiresAt = epochAny(v)
			break
		}
	}
	if label := displayLabel(m); label != "" {
		b.Name = label
	} else if v := strAny(m["name"]); v != "" {
		b.Name = v
	}
	return b, true
}

func displayLabel(m map[string]any) string {
	labels, _ := m["displayLabels"].([]any)
	for _, it := range labels {
		lm, _ := it.(map[string]any)
		if lm == nil || strAny(lm["dimension"]) != "title" {
			continue
		}
		if i18n, ok := lm["valueI18n"].(map[string]any); ok {
			if v := strAny(i18n["zh-CN"]); v != "" {
				return v
			}
			if v := strAny(i18n["en-US"]); v != "" {
				return v
			}
		}
		if v := strAny(lm["value"]); v != "" {
			return v
		}
	}
	return ""
}

// ── 旧 sash 签到 ──────────────────────────────────────────────────────────

// CheckinStatus 查询旧每日签到接口状态。
func (c *Client) CheckinStatus(ctx context.Context, a *auth.Auth) CheckinStatus {
	payload, status, raw := c.apiCall(ctx, a, http.MethodGet, pathCheckinStatus,
		c.openAPIHeaders(a, c.EndpointFor(a)), nil)
	q, _ := payload.(map[string]any)
	if q == nil {
		st := CheckinStatus{HTTP: status}
		switch status {
		case 404, 405, 410:
			st.Unavailable = true
			st.Reason = "checkin_endpoint_not_found"
		case 403:
			st.Unavailable = true
			st.Reason = "上游拒绝访问（HTTP 403）：可能是出口 IP/代理被拦截，或本区域未开放该接口"
		default:
			st.Reason = fmt.Sprintf("HTTP %d %s", status, truncateBytes([]byte(raw), 160))
		}
		return st
	}
	now := time.Now().In(utc8)
	var last string
	if lc := epochAny(q["lastClaimedAt"]); lc > 0 {
		last = time.Unix(lc, 0).In(utc8).Format("2006-01-02")
	}
	statusStr := strAny(q["status"])
	return CheckinStatus{
		Available:         true,
		Status:            statusStr,
		Active:            statusStr == "CLAIMABLE" || statusStr == "CLAIMED",
		TodayCheckedIn:    statusStr == "CLAIMED" && last == now.Format("2006-01-02"),
		StreakDays:        int64(numOr(q, "currentStreakDays")),
		TotalClaimDays:    int64(numOr(q, "totalClaimDays")),
		RewardCredits:     int64(numOr(q, "rewardCredits")),
		TotalRewardCredit: int64(numOr(q, "totalRewardCredits")),
		NextClaimAt:       epochAny(q["nextClaimAt"]),
		LastClaimedAt:     epochAny(q["lastClaimedAt"]),
		RewardExpiresAt:   epochAny(q["rewardExpiresAt"]),
	}
}

// Checkin 旧 sash 签到：先查状态，未签则领取（幂等）。
func (c *Client) Checkin(ctx context.Context, a *auth.Auth) CheckinResult {
	st := c.CheckinStatus(ctx, a)
	if st.Unavailable {
		return CheckinResult{OK: true, Unavailable: true, Reason: st.Reason,
			Message: "本区域未开放 daily-check-in 接口，每日领取活动改由活动平台承接"}
	}
	if !st.Available {
		return CheckinResult{OK: false, Error: st.Reason}
	}
	if st.TodayCheckedIn {
		_ = a.UpdateQoderState(func(s *auth.QoderState) {
			if !strings.HasPrefix(s.LastCheckin, time.Now().In(utc8).Format("2006-01-02")) {
				s.LastCheckin = time.Now().In(utc8).Format("2006-01-02 15:04:05")
			}
		})
		return CheckinResult{OK: true, Already: true, Message: "今日已签到",
			StreakDays: st.StreakDays, RewardCredits: st.RewardCredits}
	}
	if !st.Active {
		return CheckinResult{OK: true, Disabled: true, Message: "官方签到活动未开放 (status=" + st.Status + ")"}
	}
	payload, status, raw := c.apiPost(ctx, a, pathCheckinClaim)
	m, _ := payload.(map[string]any)
	body := raw
	if m == nil {
		if strings.Contains(body, "ALREADY_CLAIMED") {
			_ = a.UpdateQoderState(func(s *auth.QoderState) { s.LastCheckin = time.Now().In(utc8).Format("2006-01-02 15:04:05") })
			_, _ = c.FetchCredits(ctx, a)
			return CheckinResult{OK: true, Already: true, Message: "今日已签到"}
		}
		return CheckinResult{OK: false, Error: fmt.Sprintf("HTTP %d %s", status, truncateBytes([]byte(body), 160))}
	}
	if strAny(m["result"]) == "ALREADY_CLAIMED" || (m["success"] == false && strings.Contains(strAny(m["error"]), "ALREADY")) {
		_ = a.UpdateQoderState(func(s *auth.QoderState) { s.LastCheckin = time.Now().In(utc8).Format("2006-01-02 15:04:05") })
		_, _ = c.FetchCredits(ctx, a)
		return CheckinResult{OK: true, Already: true, Message: "今日已签到"}
	}
	if success, ok := m["success"].(bool); ok && !success {
		if st2 := c.CheckinStatus(ctx, a); st2.TodayCheckedIn {
			_ = a.UpdateQoderState(func(s *auth.QoderState) { s.LastCheckin = time.Now().In(utc8).Format("2006-01-02 15:04:05") })
			return CheckinResult{OK: true, Already: true, Message: "今日已签到（复查确认）"}
		}
		return CheckinResult{OK: false, Error: truncateBytes([]byte(strAny(m["error"])), 160)}
	}
	reward := int64(numOr(m, "rewardCredits"))
	_ = a.UpdateQoderState(func(s *auth.QoderState) { s.LastCheckin = time.Now().In(utc8).Format("2006-01-02 15:04:05") })
	_, _ = c.FetchCredits(ctx, a)
	return CheckinResult{OK: true, RewardCredits: reward, Message: fmt.Sprintf("签到成功 +%d 积分", reward)}
}

// ── 活动平台 ──────────────────────────────────────────────────────────────

type campaignCacheEntry struct {
	at  time.Time
	res *CampaignsResult
}

// Campaigns 查询活动列表（短缓存，force 绕过）。
func (c *Client) Campaigns(ctx context.Context, a *auth.Auth, force bool) *CampaignsResult {
	if !force {
		c.campaignMu.Lock()
		if e, ok := c.campaignCache[a.UID]; ok && time.Since(e.at) < 20*time.Second {
			res := *e.res
			c.campaignMu.Unlock()
			return &res
		}
		c.campaignMu.Unlock()
	}
	res := c.fetchCampaigns(ctx, a, false)
	if res.OK && !res.ShowCampaign {
		// 派生身份下活动列表被隐藏：用 os+hostname 最小机器头再试一次。
		if retry := c.fetchCampaigns(ctx, a, true); retry.OK && retry.ShowCampaign {
			res = retry
		}
	}
	if res.OK {
		c.campaignMu.Lock()
		if c.campaignCache == nil {
			c.campaignCache = map[string]campaignCacheEntry{}
		}
		c.campaignCache[a.UID] = campaignCacheEntry{at: time.Now(), res: res}
		c.campaignMu.Unlock()
	}
	return res
}

func (c *Client) fetchCampaigns(ctx context.Context, a *auth.Auth, minimal bool) *CampaignsResult {
	payload, status, raw := c.apiCall(ctx, a, http.MethodGet, pathCampaigns,
		c.desktopHeaders(a, minimal), nil)
	if payload == nil && (status == 401 || status == 403) {
		payload, status, raw = c.apiCall(ctx, a, http.MethodGet, pathCampaigns,
			c.openAPIHeaders(a, c.EndpointFor(a)), nil)
	}
	q, _ := payload.(map[string]any)
	if q == nil {
		return &CampaignsResult{OK: false, Available: status != 404 && status != 405 && status != 410,
			Error:         fmt.Sprintf("HTTP %d %s", status, truncateBytes([]byte(raw), 160)),
			HTTP:          status,
			MachineHeader: "omitted"}
	}
	out := &CampaignsResult{
		OK:            true,
		Available:     true,
		ShowCampaign:  boolAny(q["showCampaign"]),
		Claimable:     boolAny(q["claimable"]),
		CampaignURL:   strAny(q["campaignUrl"]),
		Campaigns:     []Campaign{},
		Identity:      "derived",
		MachineHeader: "omitted",
	}
	rawList, _ := q["campaigns"].([]any)
	for _, it := range rawList {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		camp := Campaign{
			ID:                  firstStr(m, "campaignId", "campaign_id"),
			Key:                 firstStr(m, "campaignKey", "campaign_key"),
			ActionType:          firstStr(m, "actionType", "action_type"),
			ClaimStatus:         firstStr(m, "claimStatus", "claim_status"),
			RequiredAchievement: strAny(m["requiredAchievementKey"]),
			UnavailableReason:   strAny(m["unavailableReason"]),
			StartAt:             epochAny(firstAny(m, "startAt", "start_at")),
			EndAt:               epochAny(firstAny(m, "endAt", "end_at")),
		}
		if v, ok := m["achievementCompleted"].(bool); ok {
			camp.AchievementCompleted = &v
		}
		if benefit, ok := m["benefit"].(map[string]any); ok {
			camp.BenefitKind = strAny(benefit["kind"])
			camp.BenefitAmount = int64(numOr(benefit, "amount"))
		}
		if placements, ok := m["placements"].([]any); ok {
			for _, pl := range placements {
				pm, _ := pl.(map[string]any)
				if pm == nil {
					continue
				}
				cont, _ := pm["content"].(map[string]any)
				if cont == nil {
					continue
				}
				zh, _ := cont["zh"].(map[string]any)
				en, _ := cont["en"].(map[string]any)
				if camp.Title == "" {
					camp.Title = firstStr(zh, "title")
				}
				if camp.Title == "" {
					camp.Title = firstStr(en, "title")
				}
				if camp.Description == "" {
					camp.Description = firstStr(zh, "description")
				}
				if camp.DetailURL == "" {
					camp.DetailURL = firstStr(zh, "detailUrl")
					if camp.DetailURL == "" {
						camp.DetailURL = firstStr(en, "detailUrl")
					}
				}
				if camp.ButtonText == "" {
					camp.ButtonText = firstStr(zh, "buttonText")
					if camp.ButtonText == "" {
						camp.ButtonText = firstStr(en, "buttonText")
					}
				}
				if camp.Title != "" && camp.Description != "" && camp.DetailURL != "" {
					break
				}
			}
		}
		out.Campaigns = append(out.Campaigns, camp)
	}
	return out
}

// CampaignReward 查询某活动发放状态（只读）。
func (c *Client) CampaignReward(ctx context.Context, a *auth.Auth, campaignID string) (map[string]any, error) {
	path := fmt.Sprintf(pathCampaignReward, campaignID)
	payload, status, raw := c.apiCall(ctx, a, http.MethodGet, path, c.desktopHeaders(a, false), nil)
	m, _ := payload.(map[string]any)
	if m == nil {
		return nil, fmt.Errorf("campaign reward http %d: %s", status, truncateBytes([]byte(raw), 160))
	}
	return m, nil
}

// ClaimCampaign 领取活动奖励（上游幂等）。
func (c *Client) ClaimCampaign(ctx context.Context, a *auth.Auth, campaignID string) ClaimResult {
	path := fmt.Sprintf(pathCampaignClaim, campaignID)
	h := c.desktopHeaders(a, false)
	h["Content-Type"] = "application/json"
	payload, status, raw := c.apiCall(ctx, a, http.MethodPost, path, h, []byte("{}"))
	m, _ := payload.(map[string]any)
	if m == nil {
		code := jsonErrorCode(raw)
		if status == 409 || strings.EqualFold(code, "ALREADY_CLAIMED") || strings.EqualFold(code, "REPLAYED") {
			return ClaimResult{OK: true, Replayed: true, Status: "CLAIMED", Message: "今日已领取（上游幂等确认）"}
		}
		return ClaimResult{OK: false, Error: fmt.Sprintf("HTTP %d %s", status, truncateBytes([]byte(raw), 160))}
	}
	st := strings.ToUpper(strAny(m["status"]))
	failure := strings.ToUpper(strAny(m["failureCode"]))
	if failure == "SAME_PERSON_ALREADY_CLAIMED" || st == "BLOCKED" {
		return ClaimResult{Blocked: true, Status: firstNonEmpty(st, "BLOCKED"), FailureCode: firstNonEmpty(failure, "BLOCKED"),
			Amount:  int64(numOr(asMap(m["benefit"]), "amount")),
			Message: "同人已领取（同一设备/身份下其他账号本轮已领，服务端按人去重）"}
	}
	if st == "" {
		if success, ok := m["success"].(bool); ok && !success {
			return ClaimResult{Error: truncateBytes([]byte(strAny(m["error"])), 160)}
		}
	}
	redeem := strAny(m["redemptionCode"])
	if redeem != "" {
		_ = a.UpdateQoderState(func(s *auth.QoderState) {
			if s.CampaignCodes == nil {
				s.CampaignCodes = map[string]string{}
			}
			s.CampaignCodes[campaignID] = redeem
		})
	}
	if cn, ok := campaignFailureCN[failure]; ok {
		return ClaimResult{Status: firstNonEmpty(st, "NOT_ELIGIBLE"), FailureCode: failure,
			RedemptionCode: redeem, Message: cn}
	}
	amount := int64(numOr(asMap(m["benefit"]), "amount"))
	if amount == 0 {
		amount = int64(numOr(m, "amount"))
	}
	replayed := boolAny(m["replayed"])
	msg := "领取成功"
	if replayed {
		msg = "已领取"
	}
	if redeem != "" {
		msg += "，兑换码：" + redeem
	} else if st == "CLAIMED" {
		msg += "，兑换码发放确认中"
	}
	return ClaimResult{
		OK:             st == "CLAIMED" || st == "GRANTED" || st == "SUCCESS",
		Status:         st,
		Replayed:       replayed,
		FailureCode:    failure,
		GrantID:        strAny(m["grantId"]),
		RedemptionCode: redeem,
		Confirming:     st == "CLAIMED" && redeem == "",
		Amount:         amount,
		Message:        msg,
	}
}

var campaignFailureCN = map[string]string{
	"REDEMPTION_CODE_OUT_OF_STOCK": "今日名额已发完，次日 10:00 后再试",
	"ACHIEVEMENT_NOT_COMPLETED":    "需先在官方桌面端完成对应新人任务",
	"NOT_ELIGIBLE":                 "当前账号不在该活动定向内",
}

// CampaignCheckin 领取所有 CLAIMABLE 的 Credits 活动。
// onlyKinds 非 nil 时只处理这些 benefit.kind（如 "", "CREDITS"）。
func (c *Client) CampaignCheckin(ctx context.Context, a *auth.Auth, gap time.Duration, onlyKinds []string) CampaignOutcome {
	out := CampaignOutcome{OK: true}
	st := c.Campaigns(ctx, a, true)
	for _, wait := range []time.Duration{800 * time.Millisecond, 2 * time.Second} {
		if !campaignListNeedsRetry(st, onlyKinds) {
			break
		}
		if !sleepCtx(ctx, wait) {
			return out
		}
		if retry := c.Campaigns(ctx, a, true); retry.OK {
			st = retry
		}
	}
	out.CampaignsState = st
	nextAt, nextNote := nextCheckinWindow()
	out.NextAvailable, out.NextAvailNote = nextAt, nextNote
	if !st.OK {
		out.OK = false
		out.Message = firstNonEmpty(st.Error, "campaigns 查询失败")
		return out
	}
	state := a.QoderStateSnapshot()
	blockedUntil := state.CampaignBlockedUntil
	if blockedUntil == nil {
		blockedUntil = map[string]int64{}
	}
	now := time.Now()
	for _, cpn := range st.Campaigns {
		if onlyKinds != nil {
			kind := strings.ToUpper(cpn.BenefitKind)
			if !containsStr(onlyKinds, kind) {
				continue
			}
		}
		status := strings.ToUpper(cpn.ClaimStatus)
		action := cpn.ActionType
		amount := cpn.BenefitAmount
		if status == "CLAIMED" {
			if amount > 0 || action == "" || action == "CLAIM_BENEFIT" {
				out.Already = append(out.Already, cpn)
			} else {
				out.Views = append(out.Views, cpn)
			}
			if code := state.CampaignCodes[cpn.ID]; code != "" {
				out.Codes = append(out.Codes, Code{Campaign: campaignLabel(cpn), Code: code})
			}
			continue
		}
		if action != "" && action != "CLAIM_BENEFIT" {
			continue
		}
		if status != "CLAIMABLE" {
			if strings.EqualFold(cpn.UnavailableReason, "REDEMPTION_CODE_OUT_OF_STOCK") {
				out.Pending = append(out.Pending, cpn)
			} else if strings.EqualFold(cpn.UnavailableReason, "ACHIEVEMENT_NOT_COMPLETED") ||
				(cpn.AchievementCompleted != nil && !*cpn.AchievementCompleted) {
				out.Locked = append(out.Locked, cpn)
			}
			continue
		}
		if blockedUntil[cpn.ID] > now.Unix() {
			out.Blocked = append(out.Blocked, map[string]any{"campaign": campaignLabel(cpn), "failure_code": "SAME_PERSON_ALREADY_CLAIMED", "cooldown": true})
			continue
		}
		res := c.ClaimCampaign(ctx, a, cpn.ID)
		switch {
		case res.OK:
			if res.Replayed {
				out.Already = append(out.Already, cpn)
			} else {
				out.Claimed = append(out.Claimed, cpn)
				out.Earned += firstNonZero(res.Amount, amount)
			}
			if res.RedemptionCode != "" {
				out.Codes = append(out.Codes, Code{Campaign: campaignLabel(cpn), Code: res.RedemptionCode})
			}
		case res.Blocked:
			blockedUntil[cpn.ID] = now.Add(6 * time.Hour).Unix()
			out.Blocked = append(out.Blocked, map[string]any{"campaign": campaignLabel(cpn), "failure_code": res.FailureCode})
			_ = a.UpdateQoderState(func(s *auth.QoderState) { s.CampaignBlockedUntil = blockedUntil })
		case res.FailureCode == "REDEMPTION_CODE_OUT_OF_STOCK":
			out.Pending = append(out.Pending, cpn)
		case res.FailureCode == "ACHIEVEMENT_NOT_COMPLETED":
			out.Locked = append(out.Locked, cpn)
		default:
			out.Errors = append(out.Errors, cpn.Key+": "+res.Error)
		}
		if gap > 0 {
			if !sleepCtx(ctx, gap) {
				return out
			}
		}
	}
	today := time.Now().In(utc8).Format("2006-01-02")
	if (len(out.Already) > 0 || len(out.Claimed) > 0 || len(out.Blocked) > 0) && !strings.HasPrefix(state.LastCheckin, today) {
		_ = a.UpdateQoderState(func(s *auth.QoderState) { s.LastCheckin = time.Now().In(utc8).Format("2006-01-02 15:04:05") })
	}
	c.invalidateCampaignCache(a.UID)
	out.OK = len(out.Errors) == 0
	out.Message = campaignMessage(out)
	return out
}

func campaignMessage(o CampaignOutcome) string {
	switch {
	case len(o.Claimed) > 0:
		return fmt.Sprintf("活动领取成功 +%d Credits（%s）", o.Earned, joinLabels(o.Claimed))
	case len(o.Blocked) > 0:
		return "同人已领取：同一设备/身份下的其他账号本轮已领过（服务端按人去重）"
	case len(o.Already) > 0:
		return "今日活动奖励已领取（" + joinLabels(o.Already) + "）"
	case len(o.Pending) > 0:
		return "名额已发完，次日 10:00 后可再领：" + joinLabels(o.Pending)
	case len(o.Locked) > 0:
		return "需先在官方桌面端完成新人任务后可领：" + joinLabels(o.Locked)
	case len(o.Errors) > 0:
		return "活动领取失败：" + truncateBytes([]byte(strings.Join(o.Errors, "; ")), 200)
	}
	return "当前账号暂无可领取的官方活动"
}

func (c *Client) invalidateCampaignCache(uid string) {
	c.campaignMu.Lock()
	delete(c.campaignCache, uid)
	c.campaignMu.Unlock()
}

// campaignListNeedsRetry 仅在「查询成功但没有可领项」时重试（issue #130：
// 上游偶发返回空列表/claimable=false）。
func campaignListNeedsRetry(st *CampaignsResult, onlyKinds []string) bool {
	if st == nil || !st.OK {
		return false
	}
	if len(st.Campaigns) == 0 {
		return true
	}
	if !st.ShowCampaign && !st.Claimable {
		return true
	}
	for _, c := range st.Campaigns {
		if !strings.EqualFold(c.ClaimStatus, "CLAIMABLE") {
			continue
		}
		if onlyKinds != nil && !containsStr(onlyKinds, strings.ToUpper(c.BenefitKind)) {
			continue
		}
		return false
	}
	return true
}

// ── 完整签到（活动平台 + sash 兜底 + 余额/套餐刷新）──────────────────────

// DailyCheckin 单账号签到闭环。onlyDaily=true 时只领 Credits 类活动。
func (c *Client) DailyCheckin(ctx context.Context, a *auth.Auth, gap time.Duration, onlyDaily bool) CheckinResult {
	if gap <= 0 {
		gap = time.Second
	}
	var onlyKinds []string
	if onlyDaily {
		onlyKinds = []string{"", "CREDITS"}
	}
	camp := c.CampaignCheckin(ctx, a, gap, onlyKinds)
	out := CheckinResult{
		OK:            camp.OK || len(camp.Claimed) > 0 || len(camp.Already) > 0 || len(camp.Blocked) > 0,
		Earned:        camp.Earned,
		Message:       camp.Message,
		Claimed:       campaignLabels(camp.Claimed),
		Pending:       campaignLabels(camp.Pending),
		Locked:        campaignLabels(camp.Locked),
		Codes:         camp.Codes,
		NextAvailable: camp.NextAvailable,
		NextAvailNote: camp.NextAvailNote,
	}
	for _, b := range camp.Blocked {
		out.BlockedCodes = append(out.BlockedCodes, strAny(b["failure_code"]))
	}
	if camp.OK || len(camp.Blocked) > 0 || len(camp.Claimed) > 0 || len(camp.Already) > 0 ||
		len(camp.Pending) > 0 || len(camp.Locked) > 0 {
		credits, _ := c.FetchCredits(ctx, a)
		c.FetchPlan(ctx, a)
		out.Credits = credits
		if credits != nil {
			out.CreditRemain = credits.RemainEffective
		}
		return out
	}
	// 活动平台没拿到东西：旧 sash 接口兜底。
	sash := c.Checkin(ctx, a)
	if !sash.OK {
		out.OK = false
		out.Error = sash.Error
		return out
	}
	out.OK = true
	out.Already = sash.Already
	out.Disabled = sash.Disabled
	out.Unavailable = sash.Unavailable
	if sash.Message != "" {
		out.Message = sash.Message
	}
	if sash.RewardCredits > 0 {
		out.Earned = sash.RewardCredits
		out.RewardCredits = sash.RewardCredits
	}
	if credits, err := c.FetchCredits(ctx, a); err == nil {
		out.Credits = credits
		out.CreditRemain = credits.RemainEffective
	}
	c.FetchPlan(ctx, a)
	return out
}

// NextCheckinWindow 下一个每日 10:00（UTC+8）窗口：epoch 秒与可读文本。
func nextCheckinWindow() (int64, string) {
	now := time.Now().In(utc8)
	next := time.Date(now.Year(), now.Month(), now.Day(), checkinWindowHourCST, 0, 0, 0, utc8)
	if !now.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return next.Unix(), next.Format("2006-01-02 15:04") + "（UTC+8）"
}

func campaignLabel(c Campaign) string {
	if c.Title != "" {
		return c.Title
	}
	if c.Key != "" {
		return c.Key
	}
	return c.ID
}

func campaignLabels(cs []Campaign) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, campaignLabel(c))
	}
	return out
}

func joinLabels(cs []Campaign) string {
	labels := campaignLabels(cs)
	return strings.Join(labels, "、")
}

// ── 通用取值工具 ──────────────────────────────────────────────────────────

func numOr(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, ok := toFloat(m[key])
	if !ok {
		return 0
	}
	return v
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func firstNum(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := toFloat(m[k]); ok {
			return v, true
		}
	}
	return 0, false
}

func strAny(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(s, 10)
	case int:
		return strconv.Itoa(s)
	}
	return ""
}

func boolAny(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return truthyAny(b, false)
	case float64:
		return b != 0
	}
	return false
}

func truthyAny(v any, def bool) bool {
	if v == nil {
		return def
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		s := strings.ToLower(strings.TrimSpace(b))
		return s != "" && s != "0" && s != "false" && s != "no" && s != "off" && s != "none" && s != "null"
	}
	return boolAny(v)
}

func epochAny(v any) int64 {
	n, ok := toFloat(v)
	if !ok || n <= 0 {
		return 0
	}
	if n > 1e12 { // 毫秒
		n /= 1000
	}
	return int64(n)
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := strAny(m[k]); v != "" {
			return v
		}
	}
	return ""
}

func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func jsonErrorCode(body string) string {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return ""
	}
	return strAny(m["errorCode"])
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func newUUID4Hex() string {
	return strings.ReplaceAll(newUUID4(), "-", "")
}

func creditsRemainMap(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	for _, k := range []string{"remain_effective", "remain"} {
		if v, ok := toFloat(m[k]); ok {
			return int64(v)
		}
	}
	return 0
}
