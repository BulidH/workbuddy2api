package agentcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Service 智能体配置服务。StateDir 存放接入记录与备份；Home 是本机用户主目录。
type Service struct {
	StateDir string
	Home     string
}

// New 构造服务；home 为空时取当前用户主目录。
func New(stateDir, home string) *Service {
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	return &Service{StateDir: stateDir, Home: home}
}

// PlanFile 待写入文件的预览。
type PlanFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Existed bool   `json:"existed"`
}

// Plan 一次 dry-run 的完整结果（与 Integrate 写入内容逐字节一致）。
type Plan struct {
	Client  string     `json:"client"`
	Label   string     `json:"label"`
	BaseURL string     `json:"base_url"`
	Model   string     `json:"model"`
	Mode    string     `json:"mode"`
	Guide   string     `json:"guide,omitempty"`
	Files   []PlanFile `json:"files"`
}

// Result 一次 apply / restore 的结果。
type Result struct {
	Client    string       `json:"client"`
	Label     string       `json:"label"`
	AppliedAt string       `json:"applied_at,omitempty"`
	BaseURL   string       `json:"base_url,omitempty"`
	Model     string       `json:"model,omitempty"`
	Files     []FileRecord `json:"files,omitempty"`
	Restored  []Restored   `json:"restored,omitempty"`
}

// Restored restore 的逐文件动作。
type Restored struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// OverviewClient 本机探测到的客户端。
type OverviewClient struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Desc        string   `json:"desc"`
	Protocol    string   `json:"protocol"`
	Mode        string   `json:"mode"`
	Category    string   `json:"category"`
	Installed   bool     `json:"installed"`
	Evidence    []string `json:"evidence,omitempty"`
	DocsURL     string   `json:"docs_url,omitempty"`
	Guide       string   `json:"guide,omitempty"`
	ConfigPaths []string `json:"config_paths,omitempty"`
	Configured  bool     `json:"configured"`
	AppliedAt   string   `json:"applied_at,omitempty"`
	BaseURL     string   `json:"base_url,omitempty"`
	Model       string   `json:"model,omitempty"`
}

func (c *Client) filesFor(home string) map[string]string {
	if c.files == nil {
		return map[string]string{}
	}
	return c.files(home)
}

// evidence 做只读探测：命令、配置目录、IDE 扩展目录。不读任何配置内容。
func (c *Client) evidence(home string) []string {
	var out []string
	for _, cmd := range c.Commands {
		if p := which(cmd); p != "" {
			out = append(out, "PATH: "+filepath.Base(p))
		}
	}
	for _, rel := range c.ProbeDirs {
		if st, err := os.Stat(filepath.Join(home, rel)); err == nil && st.IsDir() {
			out = append(out, "目录: "+rel)
		}
	}
	if len(c.Extensions) > 0 {
		var exts []string
		for _, rel := range []string{".vscode/extensions", ".vscode-insiders/extensions", ".cursor/extensions", ".windsurf/extensions"} {
			entries, err := os.ReadDir(filepath.Join(home, rel))
			if err != nil {
				continue
			}
			for _, e := range entries {
				exts = append(exts, strings.ToLower(e.Name()))
			}
		}
		for _, prefix := range c.Extensions {
			for _, e := range exts {
				if e == prefix || strings.HasPrefix(e, prefix+"-") {
					out = append(out, "扩展: "+prefix)
					break
				}
			}
		}
	}
	for _, p := range c.filesFor(home) {
		if _, err := os.Stat(p); err == nil {
			out = append(out, "配置: "+p)
			break
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// Overview 返回全部客户端及接入状态（只读，不落盘）。
func (s *Service) Overview() map[string]any {
	state := s.loadState()
	out := make([]OverviewClient, 0, len(clients))
	ids := make([]string, 0, len(clients))
	for id := range clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := clients[id]
		evidence := c.evidence(s.Home)
		installed := len(evidence) > 0
		record, hasRecord := state.Clients[id]
		o := OverviewClient{
			ID: id, Label: c.Label, Desc: c.Desc, Protocol: c.Protocol,
			Mode: c.Mode, Category: c.Category, Installed: installed,
			Evidence: evidence, DocsURL: c.DocsURL, Guide: c.Guide,
			ConfigPaths: valuesOf(c.filesFor(s.Home)),
			Configured:  hasRecord && len(record.Files) > 0,
		}
		if o.Configured {
			o.AppliedAt, o.BaseURL, o.Model = record.AppliedAt, record.BaseURL, record.Model
		}
		out = append(out, o)
	}
	return map[string]any{"clients": out, "home": s.Home}
}

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func normalizeBase(raw string) (string, string, error) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !regexp.MustCompile(`^https?://`).MatchString(base) {
		return "", "", fmt.Errorf("base_url must start with http:// or https://")
	}
	v1 := base
	if !strings.HasSuffix(v1, "/v1") {
		v1 += "/v1"
	}
	return base, v1, nil
}

func (s *Service) lookup(clientID string) (*Client, error) {
	c := clients[clientID]
	if c == nil {
		ids := make([]string, 0, len(clients))
		for id := range clients {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("unknown client %q (have: %s)", clientID, strings.Join(ids, ", "))
	}
	return c, nil
}

func buildContext(baseURL, apiKey, model string, models []ModelEntry) (Context, error) {
	base, v1, err := normalizeBase(baseURL)
	if err != nil {
		return Context{}, err
	}
	return Context{
		BaseURL: base, BaseURLV1: v1, APIKey: apiKey,
		Model: strings.TrimSpace(model), Models: models,
	}, nil
}

// Plan 生成将写入的确切内容，不落盘。manual 客户端返回指引与空文件。
func (s *Service) Plan(clientID, baseURL, apiKey, model string, models []ModelEntry) (*Plan, error) {
	c, err := s.lookup(clientID)
	if err != nil {
		return nil, err
	}
	ctx, err := buildContext(baseURL, apiKey, model, models)
	if err != nil {
		return nil, err
	}
	guide := strings.ReplaceAll(strings.ReplaceAll(c.Guide, "{BASE_URL_V1}", ctx.BaseURLV1), "{MODEL}", firstNonEmpty(ctx.Model, "auto"))
	plan := &Plan{Client: c.ID, Label: c.Label, BaseURL: ctx.BaseURL, Model: ctx.Model, Mode: c.Mode, Guide: guide}
	if c.Mode == "manual" || c.apply == nil {
		plan.Mode = "manual"
		return plan, nil
	}
	paths := c.filesFor(s.Home)
	newContent, err := c.apply(paths, ctx, readText)
	if err != nil {
		return nil, fmt.Errorf("failed to build %s config: %w", clientID, err)
	}
	for _, path := range sortedPaths(paths) {
		content, ok := newContent[path]
		if !ok {
			continue
		}
		_, statErr := os.Stat(path)
		plan.Files = append(plan.Files, PlanFile{Path: path, Content: content, Existed: statErr == nil})
	}
	return plan, nil
}

func sortedPaths(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Integrate 把客户端指向网关；先备份、事务式写入、失败整体回滚。
// 首次接入的原件备份永久保留，restore 永远回到第一次接入前。
func (s *Service) Integrate(clientID, baseURL, apiKey, model string, models []ModelEntry) (*Result, error) {
	plan, err := s.Plan(clientID, baseURL, apiKey, model, models)
	if err != nil {
		return nil, err
	}
	if plan.Mode == "manual" {
		return nil, fmt.Errorf("该客户端仅支持检测与手动接入，请查看官方配置指引")
	}
	state := s.loadState()
	prior := map[string]FileRecord{}
	if rec, ok := state.Clients[clientID]; ok {
		for _, f := range rec.Files {
			prior[f.Path] = f
		}
	}
	type stagedFile struct {
		path    string
		content []byte
		existed bool
		backup  string
		record  FileRecord
	}
	var staged []stagedFile
	rollback := map[string][]byte{}
	for _, f := range plan.Files {
		content := []byte(f.Content)
		var original []byte
		existed := f.Existed
		if existed {
			original, err = readBytes(f.Path)
			if err != nil {
				return nil, err
			}
			rollback[f.Path] = original
		} else {
			rollback[f.Path] = nil
		}
		// 上一次是我们创建的文件且仍是原样：restore 应删除而不是复活旧字节。
		if p, ok := prior[f.Path]; ok && !p.Existed && existed {
			if raw, rerr := readBytes(f.Path); rerr == nil && sha256Hex(raw) == p.SHA256 {
				existed = false
			}
		}
		backup := ""
		if existed {
			if p, ok := prior[f.Path]; ok && p.Backup != "" {
				backup = p.Backup
			} else if backup, err = s.backupFile(clientID, f.Path, original); err != nil {
				return nil, err
			}
		}
		rec := FileRecord{
			Path: f.Path, Existed: existed, Backup: backup,
			SHA256: sha256Hex(content), Bytes: len(content),
		}
		staged = append(staged, stagedFile{path: f.Path, content: content, existed: existed, backup: backup, record: rec})
	}

	written := make([]stagedFile, 0, len(staged))
	for _, f := range staged {
		if err := atomicWrite(f.path, f.content); err != nil {
			for i := len(written) - 1; i >= 0; i-- {
				w := written[i]
				if rollback[w.path] != nil {
					_ = atomicWrite(w.path, rollback[w.path])
				} else if os.Remove(w.path) != nil {
					// 回滚删除失败时不掩盖原始错误。
				}
			}
			return nil, fmt.Errorf("failed to write %s files: %w", clientID, err)
		}
		written = append(written, f)
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	result := &Result{Client: clientID, Label: plan.Label, AppliedAt: now, BaseURL: plan.BaseURL, Model: plan.Model}
	records := make([]FileRecord, 0, len(staged))
	for _, f := range staged {
		records = append(records, f.record)
		result.Files = append(result.Files, f.record)
	}
	state.Clients[clientID] = State{AppliedAt: now, BaseURL: plan.BaseURL, Model: plan.Model, Files: records}
	if err := s.saveState(state); err != nil {
		return nil, err
	}
	return result, nil
}

// Restore 精确还原 Integrate 之前的文件；我们创建的文件删除并清空父目录。
func (s *Service) Restore(clientID string) (*Result, error) {
	c, err := s.lookup(clientID)
	if err != nil {
		return nil, err
	}
	state := s.loadState()
	record, ok := state.Clients[clientID]
	if !ok || len(record.Files) == 0 {
		return nil, fmt.Errorf("%s has no recorded integration to restore", c.Label)
	}
	result := &Result{Client: clientID, Label: c.Label}
	for _, f := range record.Files {
		if f.Existed {
			if f.Backup == "" {
				return nil, fmt.Errorf("backup missing for %s", f.Path)
			}
			raw, err := readBytes(f.Backup)
			if err != nil {
				return nil, fmt.Errorf("backup missing for %s: %w", f.Path, err)
			}
			if err := atomicWrite(f.Path, raw); err != nil {
				return nil, err
			}
			result.Restored = append(result.Restored, Restored{Path: f.Path, Action: "restored"})
			continue
		}
		if err := os.Remove(f.Path); err != nil {
			if os.IsNotExist(err) {
				result.Restored = append(result.Restored, Restored{Path: f.Path, Action: "already-absent"})
			} else {
				return nil, err
			}
		} else {
			result.Restored = append(result.Restored, Restored{Path: f.Path, Action: "deleted"})
		}
		pruneEmptyParents(f.Path, s.Home)
	}
	delete(state.Clients, clientID)
	if err := s.saveState(state); err != nil {
		return nil, err
	}
	return result, nil
}

// Template 用占位 Key 在临时目录构建配置样例，不读写本机真实配置。
func (s *Service) Template(clientID, baseURL, model string) (*Plan, error) {
	c, err := s.lookup(clientID)
	if err != nil {
		return nil, err
	}
	ctx, err := buildContext(baseURL, "YOUR_API_KEY", model, nil)
	if err != nil {
		return nil, err
	}
	guide := strings.ReplaceAll(strings.ReplaceAll(c.Guide, "{BASE_URL_V1}", ctx.BaseURLV1), "{MODEL}", firstNonEmpty(ctx.Model, "auto"))
	plan := &Plan{Client: c.ID, Label: c.Label, BaseURL: ctx.BaseURL, Model: ctx.Model, Mode: c.Mode, Guide: guide}
	if c.Mode == "manual" || c.apply == nil {
		plan.Mode = "manual"
		return plan, nil
	}
	tempHome, err := os.MkdirTemp("", "qoder-agent-template-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempHome)
	roles := c.filesFor(tempHome)
	paths := map[string]string{}
	for role := range roles {
		paths[role] = filepath.Join(tempHome, role+".template")
	}
	if ctx.Models == nil {
		ctx.Models = []ModelEntry{{ID: firstNonEmpty(ctx.Model, "auto")}}
	}
	newContent, err := c.apply(paths, ctx, readText)
	if err != nil {
		return nil, err
	}
	for _, path := range sortedPaths(paths) {
		if content, ok := newContent[path]; ok {
			plan.Files = append(plan.Files, PlanFile{Path: path, Content: content, Existed: false})
		}
	}
	return plan, nil
}
