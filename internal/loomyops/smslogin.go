// smslogin.go Loomy 免密登录（短信验证码）的一次性登录会话。
//
// 为什么需要这一层：Loomy 上游没有设备授权/回调链接（不像 WorkBuddy 的
// /v2/plugin/auth/state、Qoder 的 PKCE 设备码），账号服务只提供「手机号+密码」
// 与「手机号+短信验证码」两种登录。为了让「给个链接、自己登录、网关拿登录态」
// 这套交互成立，这里把短信登录拆成服务端暂存的三步：
//
//	StartSMSLogin          生成一次性 ticket（15 分钟）
//	SendSMS(ticket, phone) 发验证码（绑定手机号与设备指纹，60s 冷却 / 最多 3 次）
//	VerifySMS(ticket, code) 校验验证码 → 换取 14 天 session → 落盘 → 入池
//
// ticket 是面板/浏览器链接之间的唯一凭据：链接里只带 ticket，手机号与验证码
// 都不进 URL（避免转发链接时泄漏）。两种入口共用同一份实现：
//   - 面板弹窗内直接填手机号/验证码；
//   - 打开 /panel/loomy-login?t=<ticket>（手机浏览器也行），在那边完成登录。
//
// 免密账号没有密码可续期：14 天 session 到期后需要重新走一次验证码登录
// （面板账号卡片会标注「免密」；有密码的账号仍走原有自动续期）。
package loomyops

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
)

const (
	// SMSLoginTTL 一条登录链接/会话的有效期。
	SMSLoginTTL = 15 * time.Minute
	// SMSCodeCooldown 同一 ticket 重发验证码的最小间隔（上游短信有频控，也防刷）。
	SMSCodeCooldown = 60 * time.Second
	// SMSMaxSends 同一 ticket 最多发送验证码次数。
	SMSMaxSends = 3
	// SMSMaxAttempts 同一 ticket 最多校验验证码次数（防穷举）。
	SMSMaxAttempts = 5
	// smsTicketCap ticket 表上限（异常堆积时兜底，正常远低于此）。
	smsTicketCap = 256
)

// smsPhonePattern 只接受中国大陆 11 位手机号。
var smsPhonePattern = regexp.MustCompile(`^1\d{10}$`)

// SMS 登录错误的 Kind（面板据此映射 HTTP 状态码与文案）。
const (
	SMSKindNotFound     = "not_found"
	SMSKindExpired      = "expired"
	SMSKindPhoneInvalid = "phone_invalid"
	SMSKindCooldown     = "cooldown"
	SMSKindTooMany      = "too_many"
	SMSKindBusy         = "busy"
	SMSKindNoCode       = "no_code"
)

// SMSLoginError 短信登录会话的业务错误（上游错误仍是 *loomy.Error，原样透传）。
type SMSLoginError struct {
	Kind    string
	Message string
	RetryIn int // 秒；只有 cooldown 有值
}

func (e *SMSLoginError) Error() string { return e.Message }

// SMSLoginStatus 面板/登录页轮询用的会话状态。
type SMSLoginStatus struct {
	State       string     `json:"state"` // waiting / sent / done / expired / unknown
	PhoneMasked string     `json:"phone_masked,omitempty"`
	Message     string     `json:"message,omitempty"`
	ResendIn    int        `json:"resend_in,omitempty"`
	ExpiresAt   int64      `json:"expires_at,omitempty"`
	Account     *auth.Auth `json:"-"`
}

// SMSSendResult 发送验证码的回执。
type SMSSendResult struct {
	PhoneMasked string `json:"phone_masked"`
	ResendIn    int    `json:"resend_in"`
	ExpiresAt   int64  `json:"expires_at"`
}

type smsTicket struct {
	created   time.Time
	expiresAt time.Time
	phone     string
	msgID     string
	identity  loomy.Identity
	sends     int
	lastSent  time.Time
	attempts  int
	verifying bool
	done      bool
	account   *auth.Auth
}

// StartSMSLogin 生成一条一次性登录会话，返回 ticket 与过期时间。
func (s *Service) StartSMSLogin() (ticket string, expiresAt int64) {
	now := time.Now()
	s.smsMu.Lock()
	defer s.smsMu.Unlock()
	s.cleanupSMSLocked(now)
	if s.smsTickets == nil {
		s.smsTickets = map[string]*smsTicket{}
	}
	// 表满时先清已过期的；仍然饱满则拒绝新会话，避免无界增长。
	if len(s.smsTickets) >= smsTicketCap {
		for id, t := range s.smsTickets {
			if !now.Before(t.expiresAt) {
				delete(s.smsTickets, id)
			}
		}
		if len(s.smsTickets) >= smsTicketCap {
			return "", 0
		}
	}
	id := newSMSTicketID()
	deadline := now.Add(SMSLoginTTL)
	s.smsTickets[id] = &smsTicket{
		created:   now,
		expiresAt: deadline,
		identity:  loomy.NewIdentity(),
	}
	return id, deadline.Unix()
}

// SendSMS 为 ticket 绑定手机号并发送登录验证码。
//
// 同一手机号若已在池内，沿用它的设备指纹（devid/campus_device_id）——重新登录
// 不该让上游把同一个人当成新设备，这一点与密码登录路径一致。
func (s *Service) SendSMS(ctx context.Context, ticket, phone string) (SMSSendResult, error) {
	phone = strings.TrimSpace(phone)
	if !smsPhonePattern.MatchString(phone) {
		return SMSSendResult{}, &SMSLoginError{Kind: SMSKindPhoneInvalid, Message: "请输入 11 位大陆手机号"}
	}
	now := time.Now()
	s.smsMu.Lock()
	t, err := s.smsTicketLocked(ticket, now, &SMSLoginError{
		Kind: SMSKindNotFound, Message: "登录链接不存在，请重新生成",
	})
	if err != nil {
		s.smsMu.Unlock()
		return SMSSendResult{}, err
	}
	if t.done {
		s.smsMu.Unlock()
		return SMSSendResult{}, &SMSLoginError{Kind: SMSKindTooMany, Message: "该链接已完成登录，请重新生成"}
	}
	if t.sends > 0 && now.Sub(t.lastSent) < SMSCodeCooldown {
		wait := int((SMSCodeCooldown - now.Sub(t.lastSent)).Seconds())
		if wait < 1 {
			wait = 1
		}
		s.smsMu.Unlock()
		return SMSSendResult{}, &SMSLoginError{
			Kind: SMSKindCooldown, Message: fmt.Sprintf("请 %d 秒后再发验证码", wait), RetryIn: wait,
		}
	}
	if t.sends >= SMSMaxSends {
		s.smsMu.Unlock()
		return SMSSendResult{}, &SMSLoginError{
			Kind: SMSKindTooMany, Message: fmt.Sprintf("该链接已发送 %d 次验证码，请重新生成", SMSMaxSends),
		}
	}
	// 同一手机号在其它 ticket（刚生成的新链接）上也受 60s 冷却约束，避免换条链接连环刷短信。
	for _, other := range s.smsTickets {
		if other == t || other.phone != phone || other.sends == 0 || other.done {
			continue
		}
		if elapsed := now.Sub(other.lastSent); elapsed < SMSCodeCooldown {
			wait := int((SMSCodeCooldown - elapsed).Seconds())
			if wait < 1 {
				wait = 1
			}
			s.smsMu.Unlock()
			return SMSSendResult{}, &SMSLoginError{
				Kind: SMSKindCooldown, Message: fmt.Sprintf("请 %d 秒后再发验证码", wait), RetryIn: wait,
			}
		}
	}
	// 换手机号 = 重新开始：丢弃上一条 msgid，并重新分配设备指纹。
	if t.phone != "" && t.phone != phone {
		t.msgID = ""
		t.identity = loomy.NewIdentity()
	}
	t.phone = phone
	if existing := s.findByLoginID(phone); existing != nil {
		existing.Lock()
		if existing.LoomyDevID != "" {
			t.identity.DevID = existing.LoomyDevID
		}
		if existing.LoomyCampusDeviceID != "" {
			t.identity.CampusDeviceID = existing.LoomyCampusDeviceID
		}
		existing.Unlock()
	}
	ident := t.identity
	t.sends++
	t.lastSent = now
	deadline := t.expiresAt
	s.smsMu.Unlock()

	// 网络调用不持锁：发送短信可能耗时数秒，不能挡住状态轮询。
	msgID, err := s.cfg.Client.SendSMSCode(ctx, ident, phone)
	if err != nil {
		return SMSSendResult{}, err
	}

	s.smsMu.Lock()
	if cur, ok := s.smsTickets[ticket]; ok && cur.phone == phone {
		cur.msgID = msgID
		if msgID == "" {
			// 上游没给 msgid 时仍允许校验（部分环境只回成功码）。
			cur.msgID = "sms-" + newSMSTicketID()
		}
	}
	s.smsMu.Unlock()
	return SMSSendResult{
		PhoneMasked: maskLoginPhone(phone),
		ResendIn:    int(SMSCodeCooldown.Seconds()),
		ExpiresAt:   deadline.Unix(),
	}, nil
}

// VerifySMS 校验验证码并落盘账号；成功返回账号（含积分）。
func (s *Service) VerifySMS(ctx context.Context, ticket, code, nickname string) (*auth.Auth, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, &SMSLoginError{Kind: SMSKindNoCode, Message: "请输入短信验证码"}
	}
	now := time.Now()
	s.smsMu.Lock()
	t, err := s.smsTicketLocked(ticket, now, &SMSLoginError{
		Kind: SMSKindNotFound, Message: "登录链接不存在，请重新生成",
	})
	if err != nil {
		s.smsMu.Unlock()
		return nil, err
	}
	if t.done && t.account != nil {
		a := t.account
		s.smsMu.Unlock()
		return a, nil
	}
	if t.verifying {
		s.smsMu.Unlock()
		return nil, &SMSLoginError{Kind: SMSKindBusy, Message: "正在登录中，请稍候"}
	}
	if t.msgID == "" || t.phone == "" {
		s.smsMu.Unlock()
		return nil, &SMSLoginError{Kind: SMSKindNoCode, Message: "请先获取短信验证码"}
	}
	if t.attempts >= SMSMaxAttempts {
		s.smsMu.Unlock()
		return nil, &SMSLoginError{
			Kind: SMSKindTooMany, Message: "验证码错误次数过多，请重新生成登录链接",
		}
	}
	t.verifying = true
	t.attempts++
	phone, msgID, ident := t.phone, t.msgID, t.identity
	deadline := t.expiresAt
	s.smsMu.Unlock()

	res, err := s.cfg.Client.LoginBySMS(ctx, ident, phone, code, msgID)

	s.smsMu.Lock()
	if cur, ok := s.smsTickets[ticket]; ok {
		cur.verifying = false
	}
	s.smsMu.Unlock()
	if err != nil {
		return nil, err
	}

	a, err := s.saveSMSAccount(ctx, res, phone, nickname, ident)
	if err != nil {
		return nil, err
	}
	s.smsMu.Lock()
	if cur, ok := s.smsTickets[ticket]; ok {
		cur.done = true
		cur.account = a
		cur.expiresAt = deadline // 保持原 TTL，不因成功而延长
	}
	s.smsMu.Unlock()
	logf("loomy sms login ok uid=%s phone=%s", shortUID(a.UID), maskLoginPhone(phone))
	return a, nil
}

// SMSLoginStatus 查询会话状态（不改变状态）。
func (s *Service) SMSLoginStatus(ticket string) SMSLoginStatus {
	now := time.Now()
	s.smsMu.Lock()
	defer s.smsMu.Unlock()
	t, ok := s.smsTickets[ticket]
	if !ok || ticket == "" {
		return SMSLoginStatus{State: "unknown", Message: "登录链接不存在或已过期，请重新生成"}
	}
	st := SMSLoginStatus{PhoneMasked: maskLoginPhone(t.phone), ExpiresAt: t.expiresAt.Unix()}
	if !now.Before(t.expiresAt) {
		st.State = "expired"
		st.Message = "登录链接已过期，请重新生成"
		delete(s.smsTickets, ticket)
		return st
	}
	switch {
	case t.done:
		st.State = "done"
		st.Account = t.account
		st.Message = "登录成功"
	case t.msgID != "":
		st.State = "sent"
		st.Message = "验证码已发送，请在登录页输入"
		if elapsed := now.Sub(t.lastSent); elapsed < SMSCodeCooldown {
			st.ResendIn = int((SMSCodeCooldown - elapsed).Seconds())
			if st.ResendIn < 1 {
				st.ResendIn = 1
			}
		}
	default:
		st.State = "waiting"
		st.Message = "等待填写手机号"
	}
	return st
}

// saveSMSAccount 把短信登录结果落盘并入池（无密码，14 天后需重新验证码登录）。
func (s *Service) saveSMSAccount(ctx context.Context, res loomy.LoginResult, phone, nickname string, ident loomy.Identity) (*auth.Auth, error) {
	a := s.findByLoginID(phone)
	if a == nil {
		a = auth.NewLoomyAuth(res.UserID, nickname, phone, "", res.Session, res.ExpireAt)
		a.Source = "sms"
		a.LoomyDevID = ident.DevID
		a.LoomyCampusDeviceID = ident.CampusDeviceID
		a.FilePath = auth.LoomyFilePath(s.cfg.AuthDir, a.UID)
	} else {
		a.Lock()
		a.AccessToken = res.Session
		a.ExpiresAt = res.ExpireAt
		a.LoginID = phone
		if nickname != "" {
			a.Nickname = nickname
		}
		if a.LoomyDevID == "" {
			a.LoomyDevID = ident.DevID
		}
		if a.LoomyCampusDeviceID == "" {
			a.LoomyCampusDeviceID = ident.CampusDeviceID
		}
		if a.Source == "" {
			a.Source = "sms"
		}
		a.Unlock()
	}
	if a.FilePath == "" {
		a.FilePath = auth.LoomyFilePath(s.cfg.AuthDir, a.UID)
	}
	if err := os.MkdirAll(s.cfg.AuthDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建账号目录: %w", err)
	}
	if err := a.SaveAtomic(); err != nil {
		return nil, fmt.Errorf("写入账号文件: %w", err)
	}
	if s.cfg.Pool != nil {
		s.cfg.Pool.Add(a)
		s.cfg.Pool.ReviveDisabled(a.UID)
	}
	s.seedCredits(ctx, a)
	return a, nil
}

// smsTicketLocked 取有效 ticket；过期会话直接删除并返回 expired。
func (s *Service) smsTicketLocked(ticket string, now time.Time, missing *SMSLoginError) (*smsTicket, error) {
	if ticket == "" {
		return nil, missing
	}
	t, ok := s.smsTickets[ticket]
	if !ok {
		return nil, missing
	}
	if !now.Before(t.expiresAt) {
		delete(s.smsTickets, ticket)
		return nil, &SMSLoginError{Kind: SMSKindExpired, Message: "登录链接已过期，请重新生成"}
	}
	return t, nil
}

func (s *Service) cleanupSMSLocked(now time.Time) {
	for id, t := range s.smsTickets {
		if !now.Before(t.expiresAt) {
			delete(s.smsTickets, id)
		}
	}
}

// newSMSTicketID 32 字节随机数的 base64url（不可枚举的一次性票据）。
func newSMSTicketID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化为时间戳 + 进程内计数不用于安全边界，只保证唯一。
		return fmt.Sprintf("t%dx", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// maskLoginPhone 脱敏手机号（138****8000）；非 11 位原样返回。
func maskLoginPhone(phone string) string {
	if len(phone) == 11 {
		return phone[:3] + "****" + phone[7:]
	}
	return phone
}

// IsSMSLoginError 判断是否为短信登录会话错误，并取出 Kind。
func IsSMSLoginError(err error) (string, bool) {
	var se *SMSLoginError
	if errors.As(err, &se) {
		return se.Kind, true
	}
	return "", false
}
