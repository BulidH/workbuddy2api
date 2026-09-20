// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// buildVersion 由构建时注入：-ldflags "-X main.buildVersion=$(git rev-parse --short HEAD)"。
// 缺省 "dev"；面板页脚展示，便于确认线上跑的是哪个提交。
var buildVersion = "dev"

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	// 已实测免费账号的权重加成（pool.free_tier_bonus，默认 3.0）。
	// 配合 pick.go 的过滤修正：免费号优先但不独占，避免池子塌缩成单账号。
	p.SetFreeTierBonus(cfg.Pool.FreeTierBonus)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:  cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:      !cfg.Schedule.SchoolEnabled,
		CatDisabled:         !cfg.Schedule.CatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}

	// ── 内置 Web 管理面板 ─────────────────────────────────────────────
	// 日志环形缓冲：标准 log 输出 tee 一份给面板；聊天表格日志走 os.Stdout（不经 log），
	// 由 server.SetChatLogSink 旁路投喂。
	logRing := panel.NewRing(1000)
	log.SetOutput(io.MultiWriter(os.Stderr, logRing))
	server.SetChatLogSink(func(line string) { logRing.Add("chat", line) })

	// 面板要复用 handler 的 StatusPayload/ModelList，而 handler 又要拿到面板实例——
	// 用前向声明的 h 打破循环：闭包在请求期才求值，届时 h 必已赋值。
	var h *server.Handler
	var panelHandler http.Handler
	if cfg.Panel.Enabled {
		panelHandler = panel.New(panel.Deps{
			ConfigPath:     *cfgPath,
			AuthDir:        cfg.AuthDir,
			StateFile:      cfg.StateFile,
			GatewayVer:     buildVersion,
			WebDir:         cfg.Panel.Dir,
			StatusJSON:     func() map[string]any { return h.StatusPayload() },
			ModelList:      func() map[string]any { return h.ModelList() },
			ValidateConfig: ValidateRaw,
			// 重扫 auths 目录并对齐账号池 —— 加/删账号免重启的关键。
			ReloadAuths: func() (int, error) {
				auths, err := auth.LoadDir(cfg.AuthDir)
				if err != nil {
					return 0, err
				}
				p.SyncToDir(auths)
				return len(auths), nil
			},
			Restart: func() {
				log.Printf("[panel] 收到重启请求，进程退出（容器 restart 策略将自动拉起）")
				p.Flush()
				os.Exit(0)
			},
			// 手动签到 / 查余额：定时任务只在 checkin_hours 整点触发且启动时不跑，
			// 新加的账号在下一个整点前积分恒为 0，故开放这个当场触发的入口。
			RunCheckin: func() ([]panel.CheckinOutcome, error) {
				res, err := sch.CheckinAll()
				if err != nil {
					return nil, err
				}
				out := make([]panel.CheckinOutcome, 0, len(res))
				for _, o := range res {
					out = append(out, panel.CheckinOutcome{
						UID:      o.UID,
						Nickname: o.Nickname,
						Status:   string(o.Status),
						Credits:  o.Credits,
						Detail:   o.Detail,
					})
				}
				return out, nil
			},
			// 只读余额查询（含国际版）：上游 D4 门控刻意让 global 账号不参与签到任务、
			// 不对该域发起任何自动调用以免触发风控。这里保持门控不动，只把「查余额」
			// 做成用户显式点击才发生的一次 get-user-resource 只读请求。
			QueryBalances: func() ([]panel.BalanceOutcome, error) {
				var out []panel.BalanceOutcome
				for i, st := range p.List() {
					if st.Disabled {
						continue
					}
					// 账号间错开一点：实测连续快速请求 workbuddy.ai 的 billing 接口会
					// 偶发超时/500（每次失败的账号都不同，重试即好）。串行 + 小间隔
					// 比并发轰炸更稳，也避免看起来像在扫接口。
					if i > 0 {
						time.Sleep(400 * time.Millisecond)
					}
					a := p.AuthByUID(st.UID)
					if a == nil {
						out = append(out, panel.BalanceOutcome{
							UID: st.UID, Nickname: st.Nickname, Realm: st.Realm,
							Error: "无有效凭证",
						})
						continue
					}
					oc := panel.BalanceOutcome{UID: st.UID, Nickname: st.Nickname, Realm: a.Realm()}
					remain, buckets, err := up.UserResourceDetailed(a, cfg.ExpiringSoonDur)
					if err != nil {
						oc.Error = err.Error()
						log.Printf("[panel] balance %s: %v", logfmt.UID8(st.UID), err)
						out = append(out, oc)
						continue
					}
					// 与签到路径同口径：余额恢复解冻冷却账号 + 写入总量/快过架子集
					p.ReenableIfCredits(st.UID, remain)
					p.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
					r := remain
					oc.Credits = &r
					out = append(out, oc)
				}
				return out, nil
			},
			Logs: logRing,
		})
		log.Printf("内置管理面板已启用：http://<host>%s/panel/", cfg.Listen)
	}

	h = server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		MaxBodyBytes: int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		// 单请求最多换号次数（server.max_rotate，默认 3）——号多时调大可避免
		// 「只试了前几个就报全部账号不可用」。
		MaxRotate: cfg.Server.MaxRotate,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		Panel:         panelHandler,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 取值大于 MaxBodyMB 在常规带宽下的上传耗时；聊天请求体上限默认 8MB。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
