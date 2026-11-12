// Package qoderops Qoder 账号批次运营：签到 / 活动领取 / 余额刷新 / token 保活。
//
// 与 internal/qoder 的分工：qoder 只负责单账号的上游协议；本包负责遍历账号池、
// 串行节奏、刷新 token、把结果写回池内额度并落活动流水。调度器与面板共用同一份
// 实现，避免「手动签到」和「定时签到」两套语义漂移。
package qoderops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
)

// ErrBusy 已有一次批次任务在执行。
var ErrBusy = errors.New("qoder checkin already running")

// Config 服务依赖。
type Config struct {
	Pool     *pool.Pool
	Client   *qoder.Client
	Activity *qoder.ActivityLog // 可为 nil
	// Gap 账号之间的间隔（防风控）；<=0 用默认 1s。
	Gap time.Duration
	// RefreshSkew 批次开始前判定 token 将过期的窗口；0 用默认 10min。
	RefreshSkew time.Duration
}

// Service Qoder 批次运营服务。
type Service struct {
	cfg Config

	mu          sync.Mutex
	lastAt      time.Time
	lastSummary string
}

// New 构造服务。
func New(cfg Config) *Service {
	if cfg.Gap <= 0 {
		cfg.Gap = time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	return &Service{cfg: cfg}
}

// LastCheckin 最近一次批次签到的时刻与汇总（零值 = 本进程内尚未执行过）。
func (s *Service) LastCheckin() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAt, s.lastSummary
}

// CheckinOutcome 单账号签到回执。
type CheckinOutcome struct {
	UID               string       `json:"uid"`
	Nickname          string       `json:"nickname,omitempty"`
	Provider          string       `json:"provider"`
	Status            string       `json:"status"` // ok / already / blocked / fail / skipped
	Detail            string       `json:"detail,omitempty"`
	Credits           *int64       `json:"credits,omitempty"`
	Earned            int64        `json:"earned_credit,omitempty"`
	Claimed           []string     `json:"claimed,omitempty"`
	Pending           []string     `json:"pending,omitempty"`
	Locked            []string     `json:"locked,omitempty"`
	BlockedCodes      []string     `json:"blocked_codes,omitempty"`
	Codes             []qoder.Code `json:"codes,omitempty"`
	NextAvailableAt   int64        `json:"next_available_at,omitempty"`
	NextAvailableNote string       `json:"next_available_note,omitempty"`
}

// BalanceOutcome 单账号额度查询回执。
type BalanceOutcome struct {
	UID      string         `json:"uid"`
	Nickname string         `json:"nickname,omitempty"`
	Provider string         `json:"provider"`
	Realm    string         `json:"realm"`
	Credits  *int64         `json:"credits,omitempty"`
	Plan     string         `json:"plan,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// targets 遍历池内账号；uids 非空时只取交集（保持池内顺序）。
func (s *Service) targets(uids []string) []pool.Status {
	want := map[string]bool{}
	for _, u := range uids {
		if u != "" {
			want[u] = true
		}
	}
	out := []pool.Status{}
	for _, st := range s.cfg.Pool.List() {
		if len(want) > 0 && !want[st.UID] {
			continue
		}
		out = append(out, st)
	}
	return out
}

// ensureToken 在 token 临近过期时先刷新。
// 只有存在可刷新凭证（refresh token / PAT）才尝试；导入的账号常无 expiresAt，
// 刷新失败但 access token 仍在时继续（过期与否交给上游 401 判定）。
func (s *Service) ensureToken(ctx context.Context, a *auth.Auth) error {
	if a.RefreshToken == "" && a.PersonalToken == "" {
		return nil
	}
	if !a.NeedsRefresh(s.cfg.RefreshSkew) {
		return nil
	}
	if err := s.cfg.Client.RefreshToken(ctx, a); err != nil {
		if a.AccessToken == "" {
			return err
		}
		log.Printf("qoder refresh %s: %v（继续使用现有 access token）", shortUID(a.UID), err)
	}
	return nil
}

// CheckinAll 批量签到。onlyDaily=true 只领 Credits 类活动（面板「每日签到」）。
// 禁用的账号默认跳过；账号文件显式开启 checkinWhenDisabled 时仍参与。
func (s *Service) CheckinAll(ctx context.Context, uids []string, onlyDaily bool) ([]CheckinOutcome, error) {
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()

	statuses := s.targets(uids)
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, blockedN, failN, skipN int
	first := true
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname, Provider: auth.ProviderQoder}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			oc.Status, oc.Detail = "skipped", "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		if st.Disabled && !a.QoderStateSnapshot().CheckinWhenDisabled {
			oc.Status, oc.Detail = "skipped", "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		if a.AccessToken == "" && a.RefreshToken == "" && a.PersonalToken == "" {
			oc.Status, oc.Detail = "skipped", "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		if !first && s.cfg.Gap > 0 {
			if err := sleepCtx(ctx, s.cfg.Gap); err != nil {
				return out, err
			}
		}
		first = false

		if err := s.ensureToken(ctx, a); err != nil {
			oc.Status, oc.Detail = "fail", "refresh: "+err.Error()
			failN++
			log.Printf("qoder checkin %s refresh: %v", shortUID(st.UID), err)
			out = append(out, oc)
			continue
		}
		res := s.cfg.Client.DailyCheckin(ctx, a, s.cfg.Gap, onlyDaily)
		oc.Status = checkinStatus(res)
		oc.Detail = res.Error
		oc.Earned = res.Earned
		oc.Claimed = res.Claimed
		oc.Pending = res.Pending
		oc.Locked = res.Locked
		oc.BlockedCodes = res.BlockedCodes
		oc.Codes = res.Codes
		oc.NextAvailableAt = res.NextAvailable
		oc.NextAvailableNote = res.NextAvailNote
		remain := int64(-1)
		if res.Credits != nil {
			remain = res.CreditRemain
		} else if v := a.CreditsRemain(); v > 0 {
			remain = v
		} else if v := s.poolCredits(st.UID); v > 0 {
			remain = v
		}
		if remain >= 0 {
			s.cfg.Pool.SetCreditsDetailed(st.UID, remain, 0)
			s.cfg.Pool.ReenableIfCredits(st.UID, remain)
			v := remain
			oc.Credits = &v
		}
		switch oc.Status {
		case "ok":
			okN++
		case "already":
			alreadyN++
		case "blocked":
			blockedN++
		default:
			failN++
			if oc.Detail == "" {
				oc.Detail = res.Message
			}
			log.Printf("qoder checkin %s: %s %s", shortUID(st.UID), oc.Status, oc.Detail)
		}
		s.logActivity(st, "checkin", oc)
		out = append(out, oc)
	}
	summary := fmt.Sprintf("total=%d ok=%d already=%d blocked=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, blockedN, failN, skipN)
	s.lastAt = time.Now()
	s.lastSummary = summary
	log.Printf("qoder checkin done: %s", summary)
	return out, nil
}

// RefreshBalances 批量刷新额度与套餐（只读）。禁用的账号也查（面板要能看到积分）。
func (s *Service) RefreshBalances(ctx context.Context, uids []string) ([]BalanceOutcome, error) {
	statuses := s.targets(uids)
	out := make([]BalanceOutcome, 0, len(statuses))
	first := true
	for _, st := range statuses {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			out = append(out, BalanceOutcome{UID: st.UID, Nickname: st.Nickname,
				Provider: auth.ProviderQoder, Realm: "cn", Error: "no credentials"})
			continue
		}
		if !first && s.cfg.Gap > 0 {
			if err := sleepCtx(ctx, s.cfg.Gap); err != nil {
				return out, err
			}
		}
		first = false
		oc := BalanceOutcome{UID: st.UID, Nickname: st.Nickname,
			Provider: auth.ProviderQoder, Realm: a.Realm()}
		if err := s.ensureToken(ctx, a); err != nil {
			oc.Error = "refresh: " + err.Error()
			out = append(out, oc)
			continue
		}
		cr, err := s.cfg.Client.FetchCredits(ctx, a)
		if err != nil {
			oc.Error = err.Error()
			out = append(out, oc)
			continue
		}
		oc.Plan = s.cfg.Client.FetchPlan(ctx, a)
		remain := cr.RemainEffective
		oc.Credits = &remain
		oc.Detail = creditsDetail(cr)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, 0)
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		out = append(out, oc)
	}
	return out, nil
}

// Keepalive 刷新所有 Qoder 账号 token（定时保活）。
func (s *Service) Keepalive(ctx context.Context) {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || (a.RefreshToken == "" && a.PersonalToken == "") {
			continue
		}
		if err := s.cfg.Client.RefreshToken(ctx, a); err != nil {
			log.Printf("qoder keepalive %s: %v", shortUID(st.UID), err)
			continue
		}
		log.Printf("qoder keepalive %s: ok", shortUID(st.UID))
	}
}

// Campaigns 查询某账号的活动列表（force 绕过 20s 缓存）。
func (s *Service) Campaigns(ctx context.Context, uid string, force bool) (*qoder.CampaignsResult, error) {
	a, err := s.account(uid)
	if err != nil {
		return nil, err
	}
	if err := s.ensureToken(ctx, a); err != nil {
		return nil, err
	}
	return s.cfg.Client.Campaigns(ctx, a, force), nil
}

// Claim 领取单个活动；成功后刷新额度并落流水。
func (s *Service) Claim(ctx context.Context, uid, campaignID string) (qoder.ClaimResult, error) {
	a, err := s.account(uid)
	if err != nil {
		return qoder.ClaimResult{}, err
	}
	if err := s.ensureToken(ctx, a); err != nil {
		return qoder.ClaimResult{}, err
	}
	res := s.cfg.Client.ClaimCampaign(ctx, a, campaignID)
	if res.OK || res.Replayed {
		title := campaignID
		if camps := s.cfg.Client.Campaigns(ctx, a, true); camps != nil {
			for _, c := range camps.Campaigns {
				if c.ID == campaignID || c.Key == campaignID {
					title = c.Title
					break
				}
			}
		}
		if cr, err := s.cfg.Client.FetchCredits(ctx, a); err == nil && cr != nil {
			s.cfg.Pool.SetCreditsDetailed(uid, cr.RemainEffective, 0)
			s.cfg.Pool.ReenableIfCredits(uid, cr.RemainEffective)
		}
		status := "ok"
		if res.Replayed {
			status = "already"
		}
		if s.cfg.Activity != nil {
			s.cfg.Activity.Log(qoder.ActivityEntry{
				UID: uid, Nickname: a.Nickname, Kind: "claim",
				Campaign: title, Credits: res.Amount, Status: status,
				Message: res.Message,
			})
		}
	}
	return res, nil
}

// SetCheckinWhenDisabled 修改「禁用账号也签到」开关并落盘。
func (s *Service) SetCheckinWhenDisabled(uid string, v bool) error {
	a, err := s.account(uid)
	if err != nil {
		return err
	}
	return a.UpdateQoderState(func(st *auth.QoderState) { st.CheckinWhenDisabled = v })
}

func (s *Service) account(uid string) (*auth.Auth, error) {
	if a := s.cfg.Pool.AuthByUID(uid); a != nil {
		return a, nil
	}
	return nil, fmt.Errorf("账号不在 Qoder 池内: %s", uid)
}

func (s *Service) poolCredits(uid string) int64 {
	if st, ok := s.cfg.Pool.Status(uid); ok {
		return st.Credits
	}
	return 0
}

func (s *Service) logActivity(st pool.Status, kind string, oc CheckinOutcome) {
	if s.cfg.Activity == nil {
		return
	}
	s.cfg.Activity.Log(qoder.ActivityEntry{
		UID: st.UID, Nickname: st.Nickname, Kind: kind,
		Credits: oc.Earned, Status: oc.Status, Message: oc.Detail,
	})
}

func checkinStatus(res qoder.CheckinResult) string {
	switch {
	case len(res.Claimed) > 0:
		return "ok"
	case len(res.BlockedCodes) > 0 || res.Blocked:
		return "blocked"
	case res.Already:
		return "already"
	case res.OK:
		if res.Earned > 0 {
			return "ok"
		}
		return "already"
	default:
		return "fail"
	}
}

func creditsDetail(cr *qoder.Credits) map[string]any {
	if cr == nil {
		return nil
	}
	out := map[string]any{
		"remain":           cr.Remain,
		"remain_effective": cr.RemainEffective,
		"used":             cr.Used,
		"size":             cr.Size,
	}
	if cr.ExpiresAt > 0 {
		out["expires_at"] = cr.ExpiresAt
	}
	if cr.Shared != nil {
		out["shared"] = bucketView(*cr.Shared)
	}
	if len(cr.Packages) > 0 {
		pkgs := make([]map[string]any, 0, len(cr.Packages))
		for _, p := range cr.Packages {
			pkgs = append(pkgs, bucketView(p))
		}
		out["packages"] = pkgs
	}
	return out
}

func bucketView(b qoder.QuotaBucket) map[string]any {
	m := map[string]any{
		"label": b.Name, "kind": b.Kind, "remain": b.Remain,
		"size": b.Size, "used": b.Used, "unlimited": b.Unlimited,
	}
	if b.ExpiresAt > 0 {
		m["expires_at"] = b.ExpiresAt
	}
	return m
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
