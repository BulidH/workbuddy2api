// loomylogin — Loomy 账号登录 / 写入凭证文件（无界面场景用）。
//
// 注意：正常加号请走 Web 面板「添加账号 · Loomy」，那里同样调用 loomyops.AddAccount，
// 两条路径的落盘与入池语义完全一致。本 CLI 只用于面板不可达的服务器 / 脚本场景。
//
// Loomy 没有 refresh token：14 天 session 到期后只能重新登录，所以账号文件里必须
// 同时保存手机号与密码（文件权限 0600）。再次运行（同一手机号）即为续期。
//
// 用法：
//
//	loomylogin --phone 13800000000 --password 'xxx' [--dir ./loomy_auths] [--name 备注]
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

	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/loomyops"
)

func main() {
	authDir := flag.String("dir", "./loomy_auths", "账号文件目录（loomy*.json）")
	phone := flag.String("phone", "", "登录手机号（loginid）")
	password := flag.String("password", "", "登录密码（也可用环境变量 LOOMY_PASSWORD）")
	nickname := flag.String("name", "", "昵称（缺省 = 脱敏手机号）")
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
	svc := loomyops.New(loomyops.Config{Client: client, AuthDir: *authDir})

	// Pool 为 nil：CLI 只负责写文件，入池交给网关进程重载。
	a, err := svc.AddAccount(ctx, loginID, pwd, *nickname)
	if err != nil {
		log.Fatalf("登录失败: %v", err)
	}

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
