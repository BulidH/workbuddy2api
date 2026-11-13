// Package loomyops Loomy 账号批次运营：每日签到（/points/first-login）、
// 新手任务领取（/onboarding/tasks）、积分余额刷新与会话保活（重新登录）。
//
// 与 internal/loomy 的分工：loomy 只负责单账号的上游协议；本包负责遍历账号池、
// 串行节奏、续期会话、把结果写回池内额度并落盘。调度器、面板与 CLI 共用同一份
// 实现，避免「手动签到」和「定时签到」两套语义漂移。
package loomyops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
)

// ErrBusy 已有一次批次任务在执行。
var ErrBusy = errors.New("loomy checkin already running")

// Config 服务依赖。
type Config struct {
	Pool   *pool.Pool
	Client *loomy.Client
	// Gap 账号之间的间隔（防风控）；<=0 用默认 1s。
	Gap time.Duration
	// RefreshSkew 批次开始前判定会话将过期的窗口；0 用默认 1h。
	RefreshSkew time.Duration
	// ClaimOnboarding 是否领取新手任务（config loomy.onboarding_enabled，默认 true）。
	ClaimOnboarding bool
	// TaskGap 两项任务上报之间的间隔；<=0 用默认 1s。
	TaskGap time.Duration
}

// Service Loomy 批次运营服务。
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
		cfg.RefreshSkew = time.Hour
	}
	if cfg.TaskGap <= 0 {
		cfg.TaskGap = time.Second
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
	UID      string   `json:"uid"`
	Nickname string   `json:"nickname,omitempty"`
	Status   string   `json:"status"` // ok / already / fail / skipped
	Detail   string   `json:"detail,omitempty"`
	Credits  *int64   `json:"credits,omitempty"`
	Earned   int      `json:"earned_points,omitempty"`
	Claimed  []string `json:"claimed,omitempty"`
	Pending  []string `json:"pending,omitempty"`
}

// BalanceOutcome 单账号余额查询回执。
type BalanceOutcome struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits  *int64 `json:"credits,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
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

// CheckinAll 批量签到：刷新每日积分 + 领取新手任务 + 查余额解冻冷却账号。
// 禁用的账号跳过；当天已签到的账号只刷新余额（签到接口本身幂等，但新手任务
// 领取要回读两次状态，已签到就省掉这项上游往返）。
func (s *Service) CheckinAll(ctx context.Context, uids []string) ([]CheckinOutcome, error) {
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()

	statuses := s.targets(uids)
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	first := true
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			oc.Status, oc.Detail = "skipped", "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		if st.Disabled {
			oc.Status, oc.Detail = "skipped", "disabled"
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

		if err := s.ensureSession(ctx, a); err != nil {
			oc.Status, oc.Detail = "fail", "refresh: "+err.Error()
			failN++
			log.Printf("ERR: loomy checkin %s refresh: %v", shortUID(st.UID), err)
			out = append(out, oc)
			continue
		}

		alreadyChecked := a.CheckedInToday(time.Now())
		if !alreadyChecked {
			if _, err := s.cfg.Client.FirstLogin(ctx, a); err != nil {
				oc.Status, oc.Detail = "fail", "first-login: "+err.Error()
				failN++
				log.Printf("ERR: loomy checkin %s first-login: %v", shortUID(st.UID), err)
				out = append(out, oc)
				continue
			}
			oc.Status = "ok"
			if err := a.UpdateCheckinState("loomy", time.Now().Format(time.RFC3339)); err != nil {
				log.Printf("WARN: loomy checkin %s save: %v", shortUID(st.UID), err)
			}
		} else {
			oc.Status = "already"
		}

		// 新手任务的领取与签到解耦：当天补跑、签到后才发现未领、上次回读未确认的账号，
		// 只要还没全部确认领取就再试一次（claimOnboarding 只上报上游标记为未完成的项）。
		if s.cfg.ClaimOnboarding && (oc.Status == "ok" || !a.OnboardingComplete()) {
			claimed, pending, earned, detail := s.claimOnboarding(ctx, a)
			oc.Claimed, oc.Pending, oc.Earned = claimed, pending, earned
			if detail != "" {
				if oc.Detail != "" {
					oc.Detail += "；"
				}
				oc.Detail += detail
			}
			// 读到状态且无待处理项 = 8 项已全部确认领取，落盘后不再每趟空跑。
			done := len(pending) == 0 && !strings.HasPrefix(detail, "onboarding")
			if done != a.OnboardingComplete() {
				if err := a.SetOnboardingComplete(done); err != nil {
					log.Printf("WARN: loomy checkin %s onboarding state: %v", shortUID(st.UID), err)
				}
			}
		}

		// 余额查询：签到后立即刷新，积分恢复即解冻冷却账号（与 WorkBuddy 签到同口径）。
		if q, err := s.cfg.Client.Quota(ctx, a); err == nil {
			remain := pickRemain(q)
			if remain != nil {
				s.cfg.Pool.SetCreditsDetailed(st.UID, *remain, 0)
				s.cfg.Pool.ReenableIfCredits(st.UID, *remain)
				v := *remain
				oc.Credits = &v
			}
			if err := a.UpdateLoomyCredits(quotaMap(q)); err != nil {
				log.Printf("WARN: loomy checkin %s credits save: %v", shortUID(st.UID), err)
			}
		} else if oc.Detail == "" {
			oc.Detail = "quota: " + err.Error()
		}

		switch oc.Status {
		case "ok":
			okN++
		case "already":
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	summary := fmt.Sprintf("total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	s.lastAt = time.Now()
	s.lastSummary = summary
	log.Printf("loomy checkin done: %s", summary)
	return out, nil
}

// claimOnboarding 领取全部未完成新手任务，并在领取前后回读确认。
// 返回（已确认领取, 待处理, 获得积分, 说明）。
func (s *Service) claimOnboarding(ctx context.Context, a *auth.Auth) (claimed, pending []string, earned int, detail string) {
	before, err := s.cfg.Client.Onboarding(ctx, a)
	if err != nil {
		return nil, nil, 0, "onboarding: " + err.Error()
	}
	attempts := []loomy.OnboardingTask{}
	var failedTitles []string
	for _, t := range loomy.OnboardingTasks {
		if before[t.Key] {
			continue
		}
		first := len(attempts) == 0
		if !first {
			if err := sleepCtx(ctx, s.cfg.TaskGap); err != nil {
				break
			}
		}
		if _, err := s.cfg.Client.ClaimTask(ctx, a, t.Key); err != nil {
			log.Printf("WARN: loomy claim %s %s: %v", shortUID(a.UID), t.Key, err)
			failedTitles = append(failedTitles, t.Title)
			continue
		}
		attempts = append(attempts, t)
	}
	if len(attempts) == 0 && len(failedTitles) == 0 {
		return nil, nil, 0, ""
	}
	after, err := s.cfg.Client.Onboarding(ctx, a)
	if err != nil {
		for _, t := range attempts {
			pending = append(pending, t.Title)
		}
		pending = append(pending, failedTitles...)
		return nil, pending, 0, "onboarding readback failed: " + err.Error()
	}
	for _, t := range attempts {
		if after[t.Key] {
			claimed = append(claimed, fmt.Sprintf("%s+%d", t.Title, t.Points))
			earned += t.Points
		} else {
			pending = append(pending, t.Title)
		}
	}
	switch {
	case len(pending) > 0 && len(failedTitles) > 0:
		detail = fmt.Sprintf("新手任务回读未确认 %d 项，另有 %d 项上报失败", len(pending), len(failedTitles))
	case len(failedTitles) > 0:
		detail = fmt.Sprintf("新手任务 %d 项上报失败", len(failedTitles))
	case len(pending) > 0:
		detail = fmt.Sprintf("新手任务回读未确认 %d 项", len(pending))
	case len(claimed) > 0:
		detail = fmt.Sprintf("新手任务 +%d 项（+%d 分）", len(claimed), earned)
	}
	// 上报失败的项仍是未完成状态，计入 pending（否则会被误判成「已全部领取」而写死完成标记）。
	pending = append(pending, failedTitles...)
	return claimed, pending, earned, detail
}

// RefreshBalances 批量刷新积分余额（只读）。禁用的账号也查（面板要能看到积分）。
func (s *Service) RefreshBalances(ctx context.Context, uids []string) ([]BalanceOutcome, error) {
	statuses := s.targets(uids)
	out := make([]BalanceOutcome, 0, len(statuses))
	first := true
	for _, st := range statuses {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			out = append(out, BalanceOutcome{UID: st.UID, Nickname: st.Nickname, Error: "no credentials"})
			continue
		}
		if !first && s.cfg.Gap > 0 {
			if err := sleepCtx(ctx, s.cfg.Gap); err != nil {
				return out, err
			}
		}
		first = false
		oc := BalanceOutcome{UID: st.UID, Nickname: st.Nickname}
		if err := s.ensureSession(ctx, a); err != nil {
			oc.Error = "refresh: " + err.Error()
			out = append(out, oc)
			continue
		}
		q, err := s.cfg.Client.Quota(ctx, a)
		if err != nil {
			oc.Error = err.Error()
			out = append(out, oc)
			continue
		}
		if remain := pickRemain(q); remain != nil {
			oc.Credits = remain
			s.cfg.Pool.SetCreditsDetailed(st.UID, *remain, 0)
			s.cfg.Pool.ReenableIfCredits(st.UID, *remain)
		}
		oc.Detail = quotaText(q)
		if err := a.UpdateLoomyCredits(quotaMap(q)); err != nil {
			log.Printf("WARN: loomy balance %s save: %v", shortUID(st.UID), err)
		}
		out = append(out, oc)
	}
	return out, nil
}

// Keepalive 在会话临近过期前重新登录（Loomy 无 refresh token，续期 = 重登）。
// renewBefore <=0 时用默认 3 天。
func (s *Service) Keepalive(ctx context.Context, renewBefore time.Duration) {
	if renewBefore <= 0 {
		renewBefore = 3 * 24 * time.Hour
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.LoginID == "" || a.Password == "" {
			continue
		}
		if a.ExpiresAt > 0 && time.Now().Add(renewBefore).Unix() < a.ExpiresAt {
			continue
		}
		if err := s.cfg.Client.RefreshSession(ctx, a); err != nil {
			log.Printf("WARN: loomy keepalive %s: %v", shortUID(st.UID), err)
			continue
		}
		log.Printf("loomy keepalive %s: ok", shortUID(st.UID))
	}
}

// ensureSession 会话临近过期时先重新登录。
func (s *Service) ensureSession(ctx context.Context, a *auth.Auth) error {
	if a.LoginID == "" || a.Password == "" {
		return nil
	}
	if a.ExpiresAt > 0 && time.Now().Add(s.cfg.RefreshSkew).Unix() < a.ExpiresAt {
		return nil
	}
	return s.cfg.Client.RefreshSession(ctx, a)
}

func pickRemain(q loomy.Quota) *int64 {
	if q.Available != nil {
		return q.Available
	}
	if q.Balance != nil {
		return q.Balance
	}
	return nil
}

func quotaMap(q loomy.Quota) map[string]any {
	out := map[string]any{}
	if q.Available != nil {
		out["available"] = *q.Available
		out["remain"] = *q.Available
		out["remain_effective"] = *q.Available
	}
	if q.Balance != nil {
		out["balance"] = *q.Balance
	}
	if q.Daily != nil {
		out["daily_balance"] = *q.Daily
	}
	return out
}

func quotaText(q loomy.Quota) string {
	parts := ""
	if q.Available != nil {
		parts += fmt.Sprintf("available=%d ", *q.Available)
	}
	if q.Daily != nil {
		parts += fmt.Sprintf("daily=%d ", *q.Daily)
	}
	if q.Balance != nil {
		parts += fmt.Sprintf("balance=%d", *q.Balance)
	}
	return parts
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
