// Package panel 为 workbuddy2api 提供内置的 Web 管理面板（后台风格，无鉴权）。
//
// 设计要点：
//   - 前端静态资源 go:embed 进二进制，部署自包含；若 Deps.WebDir 非空且目录存在，
//     则优先从磁盘读取——本地开发改完刷新即刻生效，免重新编译。
//   - 面板自身不重复实现 pool / upstream 语义：账号对齐、状态快照、模型列表、
//     配置校验全部通过 cmd/server 注入的回调复用既有实现，避免语义漂移。
//   - 全部接口挂在 /panel/api/ 下，与 OpenAI 兼容面（/v1/*）完全隔离。
package panel

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Version 面板版本（前端页脚展示）。
const Version = "1.0.0"

//go:embed web
var webFS embed.FS

// Deps 面板依赖，全部由 cmd/server 注入。
type Deps struct {
	ConfigPath string // config.json 路径
	AuthDir    string // auths 目录
	StateFile  string // state.json 路径
	GatewayVer string // 网关版本标识

	// WebDir 非空且为已存在目录时，前端从该目录读（开发热加载）；否则用内嵌资源。
	WebDir string

	// StatusJSON 返回与 GET /status 一致的载荷。
	StatusJSON func() map[string]any
	// ModelList 返回与 GET /v1/models 一致的载荷。
	ModelList func() map[string]any
	// ValidateConfig 用真实配置语义校验一份 config JSON（dry-run，不落盘）。
	ValidateConfig func(raw []byte) error
	// ReloadAuths 重扫 auths 目录并对齐账号池，返回对齐后的账号数。
	ReloadAuths func() (int, error)
	// Restart 触发进程退出（依赖 docker restart 策略拉起）。
	Restart func()

	// Logs 进程日志环形缓冲（可为 nil）。
	Logs *Ring

	// HTTPClient 出站客户端（OAuth 用）；nil 时用默认。
	HTTPClient *http.Client
}

// Panel 面板 HTTP 处理器。
type Panel struct {
	deps Deps
	mux  *http.ServeMux

	mu     sync.Mutex
	states map[string]loginState // OAuth state → realm（服务端暂存，poll 时校验）
}

type loginState struct {
	Realm string
	At    time.Time
}

// New 构造面板。
func New(deps Deps) *Panel {
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	p := &Panel{deps: deps, mux: http.NewServeMux(), states: map[string]loginState{}}

	// ── 只读 ──────────────────────────────────────────────
	p.mux.HandleFunc("GET /panel/api/overview", p.handleOverview)
	p.mux.HandleFunc("GET /panel/api/accounts", p.handleAccounts)
	p.mux.HandleFunc("GET /panel/api/config", p.handleConfigGet)
	p.mux.HandleFunc("GET /panel/api/models", p.handleModels)
	p.mux.HandleFunc("GET /panel/api/logs", p.handleLogs)

	// ── 写操作 ────────────────────────────────────────────
	p.mux.HandleFunc("PUT /panel/api/config", p.handleConfigPut)
	p.mux.HandleFunc("POST /panel/api/accounts/delete", p.handleAccountDelete)
	p.mux.HandleFunc("POST /panel/api/reload", p.handleReload)
	p.mux.HandleFunc("POST /panel/api/restart", p.handleRestart)

	// ── OAuth 加号 ────────────────────────────────────────
	p.mux.HandleFunc("POST /panel/api/login/start", p.handleLoginStart)
	p.mux.HandleFunc("GET /panel/api/login/poll", p.handleLoginPoll)
	p.mux.HandleFunc("POST /panel/api/login/region", p.handleLoginRegion)

	// ── 静态资源 ──────────────────────────────────────────
	p.mux.Handle("GET /panel/", http.StripPrefix("/panel/", http.FileServerFS(p.staticFS())))

	return p
}

// ServeHTTP 实现 http.Handler。/panel 无斜杠时重定向到 /panel/（ServeMux 语义）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 面板是运维面：禁缓存，避免改完前端还在吃旧 JS。
	w.Header().Set("Cache-Control", "no-store")
	p.mux.ServeHTTP(w, r)
}

// staticFS 返回前端资源文件系统：优先磁盘（开发），否则内嵌。
func (p *Panel) staticFS() fs.FS {
	if dir := strings.TrimSpace(p.deps.WebDir); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return os.DirFS(dir)
		}
		log.Printf("[panel] web_dir=%q 不可用，回落内嵌资源", dir)
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		// 内嵌资源必然存在，走到这里说明构建产物异常，退化为空 FS 而非 panic。
		log.Printf("[panel] 内嵌资源异常: %v", err)
		return webFS
	}
	return sub
}

// ─── 通用工具 ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func okJSON(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// authFilePath 把 uid 映射为 auths 目录下的凭证文件路径。
//
// 安全：uid 来自前端/上游，必须防路径穿越（../ 或绝对路径）。只接受
// [A-Za-z0-9_-] 组成的 uid，其余一律拒绝。
func (p *Panel) authFilePath(uid string) (string, bool) {
	if uid == "" || len(uid) > 128 {
		return "", false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return "", false
		}
	}
	abs := filepath.Join(p.deps.AuthDir, "workbuddy-"+uid+".json")
	// 二次确认：拼接结果必须仍在 AuthDir 内
	if !strings.HasPrefix(filepath.Clean(abs), filepath.Clean(p.deps.AuthDir)) {
		return "", false
	}
	return abs, true
}
