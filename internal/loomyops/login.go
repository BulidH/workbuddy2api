// login.go 账号接入：手机号 + 密码登录、账号文件导入。
//
// 面板「添加账号」与 CLI 共用同一份实现，避免两条路径的落盘/入池语义漂移。
// Loomy 没有 refresh token，密码登录会把手机号 + 密码一起写进账号文件（0600），
// 用于 14 天后的自动续期；这也是为什么面板要求填密码而不是只发验证码。
package loomyops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
)

// AddAccount 用手机号 + 密码登录一个 Loomy 账号：写账号文件 → 入池 → 刷新余额。
//
// 同一个手机号重复添加视为「重新登录」：沿用已有的设备指纹（devid/campus_device_id），
// 就地更新 session 与密码，避免上游把同一个人当成新设备而触发风控。
func (s *Service) AddAccount(ctx context.Context, loginID, password, nickname string) (*auth.Auth, error) {
	loginID = strings.TrimSpace(loginID)
	password = strings.TrimSpace(password)
	if loginID == "" || password == "" {
		return nil, errors.New("手机号与密码都不能为空")
	}
	existing := s.findByLoginID(loginID)
	ident := loomy.NewIdentity()
	if existing != nil {
		existing.Lock()
		ident.DevID = existing.LoomyDevID
		ident.CampusDeviceID = existing.LoomyCampusDeviceID
		existing.Unlock()
	}
	res, err := s.cfg.Client.LoginByPassword(ctx, ident, loginID, password)
	if err != nil {
		return nil, err
	}

	a := existing
	if a == nil {
		a = auth.NewLoomyAuth(res.UserID, nickname, loginID, password, res.Session, res.ExpireAt)
		a.LoomyDevID = ident.DevID
		a.LoomyCampusDeviceID = ident.CampusDeviceID
		a.FilePath = auth.LoomyFilePath(s.cfg.AuthDir, a.UID)
	} else {
		a.Lock()
		a.AccessToken = res.Session
		a.ExpiresAt = res.ExpireAt
		a.Password = password
		a.LoginID = loginID
		if n := strings.TrimSpace(nickname); n != "" {
			a.Nickname = n
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

// ImportAccounts 导入 loomy-*.json（兼容 loomy2api 的账号文件形状）。
// 返回成功写入的账号与逐条失败原因；已存在的 uid 就地覆盖 session。
func (s *Service) ImportAccounts(items [][]byte) ([]*auth.Auth, []error) {
	var imported []*auth.Auth
	var errs []error
	for _, raw := range items {
		a, err := auth.ParseLoomy(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// 已存在的手机号视为续期：保留原 uid，避免池里出现两个同人账号。
		if old := s.findByLoginID(a.LoginID); old != nil && a.LoginID != "" {
			a.UID = old.UID
			if a.Nickname == "" {
				a.Nickname = old.Nickname
			}
			if a.LoomyDevID == "" {
				a.LoomyDevID = old.LoomyDevID
			}
			if a.LoomyCampusDeviceID == "" {
				a.LoomyCampusDeviceID = old.LoomyCampusDeviceID
			}
		}
		a.FilePath = auth.LoomyFilePath(s.cfg.AuthDir, a.UID)
		if err := os.MkdirAll(s.cfg.AuthDir, 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			errs = append(errs, err)
			continue
		}
		if s.cfg.Pool != nil {
			s.cfg.Pool.Add(a)
			s.cfg.Pool.ReviveDisabled(a.UID)
		}
		imported = append(imported, a)
	}
	return imported, errs
}

// findByLoginID 在池内找同一手机号的账号（用于重新登录时沿用设备指纹）。
func (s *Service) findByLoginID(loginID string) *auth.Auth {
	if s.cfg.Pool == nil || loginID == "" {
		return nil
	}
	for _, st := range s.cfg.Pool.List() {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		a.Lock()
		match := a.LoginID == loginID
		a.Unlock()
		if match {
			return a
		}
	}
	return nil
}

// seedCredits 登录后顺手查一次余额：面板首屏就有数字，也顺手验证 session 可用。
func (s *Service) seedCredits(ctx context.Context, a *auth.Auth) {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	q, err := s.cfg.Client.Quota(qctx, a)
	if err != nil {
		logf("WARN: loomy login %s quota: %v", shortUID(a.UID), err)
		return
	}
	if remain := pickRemain(q); remain != nil {
		if s.cfg.Pool != nil {
			s.cfg.Pool.SetCreditsDetailed(a.UID, *remain, 0)
		}
	}
	if err := a.UpdateLoomyCredits(quotaMap(q)); err != nil {
		logf("WARN: loomy login %s credits save: %v", shortUID(a.UID), err)
	}
}

// logf 统一日志出口（便于测试静音）。
func logf(format string, args ...any) { log.Printf(format, args...) }
