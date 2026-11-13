// loomylogin — Loomy 账号登录 / 写入凭证文件。
//
// Loomy 没有 refresh token：14 天 session 到期后只能重新登录，所以账号文件里必须
// 同时保存手机号与密码（文件权限 0600）。本 CLI 做两件事：
//
//  1. 首次登录：手机号 + 密码 → 调账号服务换 session → 写 loomy-<uid>.json；
//  2. 续期：对已存在的账号文件重新登录并覆盖 accessToken/expiresAt。
//
// 用法：
//
//	loomylogin --phone 13800000000 --password 'xxx' [--dir ./loomy_auths] [--name 备注]
//	loomylogin --refresh 13800000000      # 按 loginid 找到已有账号文件并重登
//
// 密码也可以走环境变量 LOOMY_PASSWORD，避免出现在 shell 历史与进程列表里。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
)

func main() {
	authDir := flag.String("dir", "./loomy_auths", "账号文件目录（loomy*.json）")
	phone := flag.String("phone", "", "登录手机号（loginid）")
	password := flag.String("password", "", "登录密码（也可用环境变量 LOOMY_PASSWORD）")
	nickname := flag.String("name", "", "昵称（缺省 = 脱敏手机号）")
	refresh := flag.Bool("refresh", false, "只对已有账号重新登录续期（按 --phone 匹配 loginid）")
	flag.Parse()

	loginID := strings.TrimSpace(*phone)
	if loginID == "" {
		log.Fatalf("缺少 --phone")
	}
	pwd := *password
	if pwd == "" {
		pwd = os.Getenv("LOOMY_PASSWORD")
	}
	if pwd == "" {
		log.Fatalf("缺少密码：--password 或环境变量 LOOMY_PASSWORD")
	}
	if err := os.MkdirAll(*authDir, 0o700); err != nil {
		log.Fatalf("创建目录 %s: %v", *authDir, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := loomy.New(loomy.Config{})

	var existing *auth.Auth
	if *refresh {
		auths, err := auth.LoadLoomyDir(*authDir)
		if err != nil {
			log.Fatalf("读取账号目录: %v", err)
		}
		for _, a := range auths {
			if a.LoginID == loginID {
				existing = a
				break
			}
		}
		if existing == nil {
			log.Fatalf("目录 %s 下没有 loginid=%s 的账号", *authDir, maskPhone(loginID))
		}
	}

	ident := loomy.NewIdentity()
	if existing != nil {
		ident.DevID = existing.LoomyDevID
		ident.CampusDeviceID = existing.LoomyCampusDeviceID
	}
	res, err := client.LoginByPassword(ctx, ident, loginID, pwd)
	if err != nil {
		log.Fatalf("登录失败: %v", err)
	}

	a := existing
	if a == nil {
		a = auth.NewLoomyAuth(res.UserID, *nickname, loginID, pwd, res.Session, res.ExpireAt)
		a.LoomyDevID = ident.DevID
		a.LoomyCampusDeviceID = ident.CampusDeviceID
		a.FilePath = auth.LoomyFilePath(*authDir, a.UID)
	} else {
		a.Lock()
		a.AccessToken = res.Session
		a.ExpiresAt = res.ExpireAt
		a.Password = pwd
		if *nickname != "" {
			a.Nickname = *nickname
		}
		a.Unlock()
	}
	if err := a.SaveAtomic(); err != nil {
		log.Fatalf("写入账号文件: %v", err)
	}

	// 顺带查一次余额，既验证 session 可用，也让面板首屏就有数字。
	quotaText := "（余额查询失败，可稍后在面板手动刷新）"
	if q, err := client.Quota(ctx, a); err == nil {
		parts := []string{}
		if q.Available != nil {
			parts = append(parts, fmt.Sprintf("available=%d", *q.Available))
		}
		if q.Daily != nil {
			parts = append(parts, fmt.Sprintf("daily=%d", *q.Daily))
		}
		if q.Balance != nil {
			parts = append(parts, fmt.Sprintf("balance=%d", *q.Balance))
		}
		if len(parts) > 0 {
			quotaText = strings.Join(parts, " ")
		}
	}
	fmt.Printf("已写入 %s\nuid=%s loginid=%s\nsession 到期: %s\n积分: %s\n",
		a.FilePath, a.UID, maskPhone(loginID),
		time.Unix(a.ExpiresAt, 0).Format("2006-01-02 15:04:05"), quotaText)
}

// maskPhone 脱敏手机号（138****8000），避免日志里出现完整号码。
func maskPhone(p string) string {
	if len(p) == 11 {
		return p[:3] + "****" + p[7:]
	}
	if len(p) > 4 {
		return p[:2] + "***" + p[len(p)-2:]
	}
	return "***"
}
