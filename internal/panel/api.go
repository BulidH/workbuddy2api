// api.go 面板的只读/运维接口：概览、账号、模型、日志、重载、重启。
package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"workbuddy2api/internal/auth"
)

// handleOverview 概览：健康态 + 账号池计数 + 运行环境。
func (p *Panel) handleOverview(w http.ResponseWriter, r *http.Request) {
	status := p.statusMap()

	// 健康判定与 /healthz 同口径：healthy>0 即视为可服务。
	total := intOf(status["total"])
	healthy := intOf(status["healthy"])
	cooling := intOf(status["cooling"])
	disabled := intOf(status["disabled"])

	// auths 目录文件数（与池内账号数不一致时说明有孤儿文件或加载失败）
	authFiles := 0
	if ents, err := os.ReadDir(p.deps.AuthDir); err == nil {
		for _, e := range ents {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
				authFiles++
			}
		}
	}

	okJSON(w, map[string]any{
		"ok":          true,
		"service":     "workbuddy2api",
		"panel_ver":   Version,
		"gateway_ver": p.deps.GatewayVer,
		"healthy":     healthy > 0,
		"counts": map[string]any{
			"total":         total,
			"healthy":       healthy,
			"cooling":       cooling,
			"disabled":      disabled,
			"in_flight":     status["in_flight_full"],
			"auth_files":    authFiles,
			"sticky":        status["sticky_sessions"],
		},
		"realm_totals": status["realm_totals"],
		"redis_mode":   status["redis_mode"],
		"paths": map[string]string{
			"config": p.deps.ConfigPath,
			"auths":  p.deps.AuthDir,
			"state":  p.deps.StateFile,
		},
		"now": time.Now().Format(time.RFC3339),
	})
}

// accountView 面板账号视图：池状态 + 凭证文件信息合并。
type accountView struct {
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	Realm       string `json:"realm"`
	Credits     int64  `json:"credits"`
	Cooling     bool   `json:"cooling"`
	Disabled    bool   `json:"disabled"`
	InFlight    int    `json:"in_flight"`
	Success     int64  `json:"success_count"`
	ErrTotal    int64  `json:"err_total"`
	LastSuccess string `json:"last_success,omitempty"`
	LastErr     string `json:"last_err,omitempty"`
	CoolRemain  int64  `json:"cool_remaining_sec"`
	CoolKind    string `json:"cool_kind,omitempty"`
	Reason      string `json:"reason,omitempty"`

	// 以下来自凭证文件
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Domain    string `json:"domain,omitempty"`
	File      string `json:"file,omitempty"`
}

// statusMap 取 /status 载荷并规整为通用 JSON 结构（map[string]any / []any / float64）。
//
// 为什么必须规整：StatusJSON() 里的 accounts 是 pool.List() 返回的 []pool.Status ——
// **具体类型切片**。对 map[string]any 里的这种值做 `.([]any)` 断言必然失败，
// 于是面板只能拿 auth 文件兜底，导致积分/成功数/最近成功/冷却/禁用全部显示为 0 或空。
// 走一次 JSON 往返可以把任意具体类型统一成通用结构，比逐个断言健壮。
func (p *Panel) statusMap() map[string]any {
	if p.deps.StatusJSON == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(p.deps.StatusJSON())
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// handleAccounts 账号列表：池状态与 auths 目录凭证合并（并集）。
func (p *Panel) handleAccounts(w http.ResponseWriter, r *http.Request) {
	status := p.statusMap()

	byUID := map[string]*accountView{}
	order := []string{}

	// 1) 池内账号（statusMap 已规整，此处断言必定成功）
	if list, ok := status["accounts"].([]any); ok {
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			uid := strOf(m["uid"])
			if uid == "" {
				continue
			}
			v := &accountView{
				UID:        uid,
				Nickname:   strOf(m["nickname"]),
				Realm:      strOf(m["realm"]),
				Credits:    int64Of(m["credits"]),
				Cooling:    boolOf(m["cooling"]),
				Disabled:   boolOf(m["disabled"]),
				InFlight:   intOf(m["in_flight"]),
				Success:    int64Of(m["success_count"]),
				ErrTotal:   int64Of(m["err_total"]),
				CoolRemain: int64Of(m["cool_remaining_sec"]),
				CoolKind:   strOf(m["cool_kind"]),
				Reason:     strOf(m["reason"]),
				LastSuccess: timeStr(m["last_success"]),
				LastErr:     timeStr(m["last_err"]),
			}
			byUID[uid] = v
			order = append(order, uid)
		}
	}

	// 2) auths 目录凭证（补全到期时间/域名；池里没有的也列出来，便于发现加载失败）
	if auths, err := auth.LoadDir(p.deps.AuthDir); err == nil {
		for _, a := range auths {
			v, ok := byUID[a.UID]
			if !ok {
				v = &accountView{UID: a.UID}
				byUID[a.UID] = v
				order = append(order, a.UID)
			}
			v.ExpiresAt = a.ExpiresAt
			v.Domain = a.Domain
			v.File = filepath.Base(a.FilePath)
			if v.Nickname == "" {
				v.Nickname = a.Nickname
			}
			if v.Realm == "" {
				v.Realm = a.Realm()
			}
		}
	}

	out := make([]*accountView, 0, len(order))
	for _, uid := range order {
		out = append(out, byUID[uid])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].UID < out[j].UID
	})

	okJSON(w, map[string]any{"ok": true, "accounts": out, "total": len(out)})
}

// handleAccountDelete 删除账号：移除凭证文件后对齐账号池。
func (p *Panel) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID string `json:"uid"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	path, ok := p.authFilePath(body.UID)
	if !ok {
		errJSON(w, http.StatusBadRequest, "非法 uid")
		return
	}
	if _, err := os.Stat(path); err != nil {
		errJSON(w, http.StatusNotFound, "凭证文件不存在: "+filepath.Base(path))
		return
	}
	// 先备份再删：误删可从 backups/ 找回
	bakDir := filepath.Join(filepath.Dir(p.deps.AuthDir), "backups")
	_ = os.MkdirAll(bakDir, 0o700)
	bak := filepath.Join(bakDir, filepath.Base(path)+"."+time.Now().Format("20060102-150405")+".bak")
	if raw, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(bak, raw, 0o600)
	}
	if err := os.Remove(path); err != nil {
		errJSON(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	n, err := p.reload()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "账号池对齐失败: "+err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "deleted": filepath.Base(path), "backup": filepath.Base(bak), "accounts": n})
}

// handleModels 复用网关的模型清单。
func (p *Panel) handleModels(w http.ResponseWriter, r *http.Request) {
	if p.deps.ModelList == nil {
		errJSON(w, http.StatusServiceUnavailable, "模型清单不可用")
		return
	}
	okJSON(w, p.deps.ModelList())
}

// handleLogs 返回最近日志（环形缓冲）。
func (p *Panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 300
	}
	if n > 2000 {
		n = 2000
	}
	lines := []LogLine{}
	if p.deps.Logs != nil {
		lines = p.deps.Logs.Tail(n)
	}
	okJSON(w, map[string]any{"ok": true, "lines": lines, "count": len(lines)})
}

// handleReload 重扫 auths 目录并对齐账号池（加/删凭证文件后免重启）。
func (p *Panel) handleReload(w http.ResponseWriter, r *http.Request) {
	n, err := p.reload()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "accounts": n})
}

// handleCheckin 立即执行一轮全量签到 + 余额查询。
//
// 用途：定时签到只在配置的整点跑（默认 9/21 点）、且调度器启动时不跑，
// 新加的账号在下一个整点前积分一直显示 0。此入口让用户当场查一次余额。
func (p *Panel) handleCheckin(w http.ResponseWriter, r *http.Request) {
	if p.deps.RunCheckin == nil {
		errJSON(w, http.StatusServiceUnavailable, "签到入口不可用")
		return
	}
	outcomes, err := p.deps.RunCheckin()
	if err != nil {
		// 与定时任务撞车（ErrBusy）不是故障，提示稍后重试即可
		errJSON(w, http.StatusConflict, "签到未执行："+err.Error())
		return
	}
	okN, alreadyN, failN, skipN := 0, 0, 0, 0
	var credited int
	for _, o := range outcomes {
		switch o.Status {
		case "ok":
			okN++
		case "already":
			alreadyN++
		case "skipped":
			skipN++
		default:
			failN++
		}
		if o.Credits != nil {
			credited++
		}
	}
	okJSON(w, map[string]any{
		"ok":       true,
		"outcomes": outcomes,
		"summary": map[string]int{
			"total": len(outcomes), "ok": okN, "already": alreadyN,
			"fail": failN, "skipped": skipN, "credits_read": credited,
		},
	})
}

// handleRestart 触发进程退出（依赖 docker restart 策略拉起，用于 listen 等装配期配置）。
func (p *Panel) handleRestart(w http.ResponseWriter, r *http.Request) {
	if p.deps.Restart == nil {
		errJSON(w, http.StatusServiceUnavailable, "重启不可用")
		return
	}
	okJSON(w, map[string]any{"ok": true, "msg": "网关正在重启，约 2 秒后自动恢复"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(300 * time.Millisecond) // 留出响应落盘时间
		p.deps.Restart()
	}()
}

func (p *Panel) reload() (int, error) {
	if p.deps.ReloadAuths == nil {
		return 0, nil
	}
	return p.deps.ReloadAuths()
}

// ─── any 取值小工具（/status 是 map[string]any，数值是 float64）───

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	}
	return 0
}

func int64Of(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	}
	return 0
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolOf(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// timeStr 把 time.Time 的 JSON 形态转成 HH:MM:SS；零值返回空串。
func timeStr(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		if t.IsZero() || t.Year() <= 1 {
			return ""
		}
		return t.Local().Format("01-02 15:04:05")
	}
	return ""
}
