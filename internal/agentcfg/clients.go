package agentcfg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	providerID   = "qoder-proxy"
	providerName = "Qoder Gateway"
	credential   = "QODER_PROXY_API_KEY"
)

// 客户端元数据。
type Client struct {
	ID         string
	Label      string
	Desc       string
	Protocol   string
	Mode       string // automatic | manual
	Category   string
	Commands   []string
	ProbeDirs  []string
	Extensions []string
	DocsURL    string
	Guide      string

	files func(home string) map[string]string
	apply func(paths map[string]string, ctx Context, read func(string) (string, error)) (map[string]string, error)
}

// Context 构建配置所需的网关信息。APIKey 只写进客户端文件，不落 state。
type Context struct {
	BaseURL   string
	BaseURLV1 string
	APIKey    string
	Model     string
	Models    []ModelEntry
}

// ModelEntry 供客户端模型清单使用的元数据。
type ModelEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxOutput     int    `json:"max_output,omitempty"`
}

var clients = buildClients()

func buildClients() map[string]*Client {
	m := map[string]*Client{}

	claudeSettings := func(home string) string {
		if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
			return filepath.Join(dir, "settings.json")
		}
		return filepath.Join(home, ".claude", "settings.json")
	}
	claude := &Client{
		ID: "claude-code", Label: "Claude Code", Protocol: "anthropic", Mode: "automatic",
		Category: "CLI / Harness", Commands: []string{"claude"},
		Desc:    "Anthropic CLI；走网关原生 /v1/messages（ANTHROPIC_BASE_URL）。",
		DocsURL: "https://docs.anthropic.com/en/docs/claude-code",
		Guide:   "写入 ~/.claude/settings.json 的 env 段，保留其它设置。",
		files:   func(home string) map[string]string { return map[string]string{"settings": claudeSettings(home)} },
	}
	claude.apply = func(paths map[string]string, ctx Context, read func(string) (string, error)) (map[string]string, error) {
		path := paths["settings"]
		data, err := loadJSON(path, "claude settings")
		if err != nil {
			return nil, err
		}
		env := map[string]any{}
		if prev, ok := data["env"].(map[string]any); ok {
			env = prev
		}
		// Claude Code 自己会拼 /v1，给它裸根地址。
		env["ANTHROPIC_BASE_URL"] = regexp.MustCompile(`/v1/?$`).ReplaceAllString(ctx.BaseURL, "")
		env["ANTHROPIC_AUTH_TOKEN"] = ctx.APIKey
		if ctx.Model != "" {
			for _, name := range []string{
				"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
				"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
			} {
				env[name] = ctx.Model
			}
		}
		data["env"] = env
		content, err := dumpJSON(data)
		if err != nil {
			return nil, err
		}
		return map[string]string{path: content}, nil
	}
	m[claude.ID] = claude

	codexHome := func(home string) string {
		if dir := os.Getenv("CODEX_HOME"); dir != "" {
			return dir
		}
		return filepath.Join(home, ".codex")
	}
	codex := &Client{
		ID: "codex", Label: "Codex CLI", Protocol: "openai", Mode: "automatic",
		Category: "CLI / Harness", Commands: []string{"codex"},
		Desc:    "OpenAI Codex CLI；注册 qoder-proxy Responses provider。",
		DocsURL: "https://github.com/openai/codex",
		Guide:   "写入 ~/.codex/config.toml 与 auth.json。",
		files: func(home string) map[string]string {
			return map[string]string{
				"config": filepath.Join(codexHome(home), "config.toml"),
				"auth":   filepath.Join(codexHome(home), "auth.json"),
			}
		},
	}
	codex.apply = func(paths map[string]string, ctx Context, read func(string) (string, error)) (map[string]string, error) {
		configPath := paths["config"]
		text, err := read(configPath)
		if err != nil {
			return nil, err
		}
		text = TOMLUpsertTopKey(text, "model_provider", providerID)
		if ctx.Model != "" {
			text = TOMLUpsertTopKey(text, "model", ctx.Model)
		}
		text = TOMLUpsertTable(text, "model_providers."+providerID, map[string]any{
			"name": providerName, "base_url": ctx.BaseURLV1,
			"wire_api": "responses", "env_key": "OPENAI_API_KEY",
		})
		authPath := paths["auth"]
		auth, err := loadJSON(authPath, "codex auth")
		if err != nil {
			return nil, err
		}
		auth = deepMerge(auth, map[string]any{"OPENAI_API_KEY": ctx.APIKey})
		authText, err := dumpJSON(auth)
		if err != nil {
			return nil, err
		}
		return map[string]string{configPath: text, authPath: authText}, nil
	}
	m[codex.ID] = codex

	opencode := &Client{
		ID: "opencode", Label: "OpenCode", Protocol: "openai", Mode: "automatic",
		Category: "CLI / Harness", Commands: []string{"opencode"},
		Desc:    "OpenCode；注册 openai-compatible provider。",
		DocsURL: "https://opencode.ai/docs/providers/",
		Guide:   "写入 ~/.config/opencode/opencode.json 的 provider 段。",
		files: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".config", "opencode", "opencode.json")}
		},
	}
	opencode.apply = func(paths map[string]string, ctx Context, read func(string) (string, error)) (map[string]string, error) {
		path := paths["config"]
		data, err := loadJSON(path, "opencode config")
		if err != nil {
			return nil, err
		}
		models := map[string]any{}
		rows := ctx.Models
		if len(rows) == 0 {
			rows = []ModelEntry{{ID: firstNonEmpty(ctx.Model, "auto")}}
		}
		for _, e := range rows {
			if e.ID == "" {
				continue
			}
			item := map[string]any{"name": e.ID}
			limits := map[string]any{}
			if e.ContextWindow > 0 {
				limits["context"] = e.ContextWindow
			}
			if e.MaxOutput > 0 {
				limits["output"] = e.MaxOutput
			}
			if len(limits) > 0 {
				item["limit"] = limits
			}
			models[e.ID] = item
		}
		provider := map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": providerName,
			"options": map[string]any{"baseURL": ctx.BaseURLV1, "apiKey": ctx.APIKey},
			"models":  models,
		}
		data = deepMerge(data, map[string]any{"provider": map[string]any{providerID: provider}})
		if ctx.Model != "" {
			data["model"] = providerID + "/" + ctx.Model
		}
		content, err := dumpJSON(data)
		if err != nil {
			return nil, err
		}
		return map[string]string{path: content}, nil
	}
	m[opencode.ID] = opencode

	dshHome := func(home string) string {
		if dir := os.Getenv("DSH_HOME"); dir != "" {
			return dir
		}
		return filepath.Join(home, ".dsh")
	}
	dsh := &Client{
		ID: "dsh", Label: "DSH (DeepSeek Harness)", Protocol: "openai", Mode: "automatic",
		Category: "CLI / Harness", Commands: []string{"dsh"},
		Desc:    "DSH；llm-pi-ai 注册 qoder-proxy provider，Key 走 credentials refs。",
		DocsURL: "https://github.com/deepseek-ai/dsh",
		Guide:   "写入 ~/.dsh/settings.yaml 与 .credentials.yaml。",
		files: func(home string) map[string]string {
			return map[string]string{
				"settings":    filepath.Join(dshHome(home), "settings.yaml"),
				"credentials": filepath.Join(dshHome(home), ".credentials.yaml"),
			}
		},
	}
	dsh.apply = func(paths map[string]string, ctx Context, read func(string) (string, error)) (map[string]string, error) {
		settingsPath := paths["settings"]
		text, err := read(settingsPath)
		if err != nil {
			return nil, err
		}
		var models []any
		for _, e := range ctx.Models {
			if e.ID == "" {
				continue
			}
			item := map[string]any{"id": e.ID}
			if e.ContextWindow > 0 {
				item["contextWindow"] = e.ContextWindow
			}
			models = append(models, item)
		}
		provider := map[string]any{
			"displayName": providerName, "apiKeyEnv": credential,
			"api": "openai-completions", "baseURL": ctx.BaseURLV1,
		}
		if len(models) > 0 {
			provider["models"] = models
		}
		text, err = YAMLUpsertInline(text, "llm-pi-ai.providers."+providerID, provider)
		if err != nil {
			return nil, err
		}
		if ctx.Model != "" {
			text, err = YAMLUpsertInline(text, "agent-default-model",
				map[string]any{"provider": providerID, "model": ctx.Model})
			if err != nil {
				return nil, err
			}
		}
		credPath := paths["credentials"]
		credText, err := read(credPath)
		if err != nil {
			return nil, err
		}
		credText, err = YAMLRefsUpsert(credText, credential, ctx.APIKey)
		if err != nil {
			return nil, err
		}
		// 写后自检：DSH 要求 refs 的值是字符串标量，否则整个客户端无法启动。
		if !RefsValueIsQuotedScalar(credText, credential) {
			return nil, fmt.Errorf("DSH credentials 自检失败：refs.%s 未写成带引号的字符串标量（纯数字/布尔 Key 会被 YAML 解析成非字符串）", credential)
		}
		return map[string]string{settingsPath: text, credPath: credText}, nil
	}
	m[dsh.ID] = dsh

	crush := &Client{
		ID: "crush", Label: "Crush (Charm)", Protocol: "openai", Mode: "automatic",
		Category: "CLI / Harness", Commands: []string{"crush"},
		Desc:    "Charm Crush；providers 注册 openai 型 qoder-proxy。",
		DocsURL: "https://github.com/charmbracelet/crush",
		Guide:   "写入 ~/.config/crush/crush.json 的 providers 段。",
		files: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".config", "crush", "crush.json")}
		},
	}
	crush.apply = func(paths map[string]string, ctx Context, read func(string) (string, error)) (map[string]string, error) {
		path := paths["config"]
		data, err := loadJSON(path, "crush config")
		if err != nil {
			return nil, err
		}
		var models []any
		for _, e := range ctx.Models {
			if e.ID == "" {
				continue
			}
			item := map[string]any{"id": e.ID, "name": e.ID}
			if e.ContextWindow > 0 {
				item["context_window"] = e.ContextWindow
			}
			if e.MaxOutput > 0 {
				item["default_max_tokens"] = e.MaxOutput
			}
			models = append(models, item)
		}
		provider := map[string]any{
			"name": providerName, "type": "openai",
			"base_url": ctx.BaseURLV1, "api_key": ctx.APIKey, "models": models,
		}
		data = deepMerge(data, map[string]any{"providers": map[string]any{providerID: provider}})
		content, err := dumpJSON(data)
		if err != nil {
			return nil, err
		}
		return map[string]string{path: content}, nil
	}
	m[crush.ID] = crush

	// 仅检测 + 手动接入指引的客户端（不碰私有状态库）。
	manuals := []struct {
		id, label, category, command, docs, guide string
		dirs, extensions                          []string
	}{
		{"cursor", "Cursor", "IDE / 桌面", "cursor", "https://docs.cursor.com/settings/api-keys",
			"Settings → Models → Override OpenAI Base URL：URL={BASE_URL_V1}，Key=YOUR_API_KEY，模型={MODEL}。",
			[]string{".cursor"}, nil},
		{"cline", "Cline", "IDE 扩展", "", "https://docs.cline.bot/provider-config/openai-compatible",
			"Cline 设置 → API Provider=OpenAI Compatible；Base URL={BASE_URL_V1}；Key=YOUR_API_KEY；Model={MODEL}。",
			nil, []string{"saoudrizwan.claude-dev"}},
		{"roo-code", "Roo Code", "IDE 扩展", "", "https://docs.roocode.com/providers/openai-compatible",
			"Roo Code 设置 → OpenAI Compatible；Base URL={BASE_URL_V1}；Key=YOUR_API_KEY；Model={MODEL}。",
			nil, []string{"rooveterinaryinc.roo-cline"}},
		{"zed", "Zed", "IDE / 桌面", "zed", "https://zed.dev/docs/ai/llm-providers",
			"Settings → AI → OpenAI Compatible；api_url={BASE_URL_V1}；模型={MODEL}。",
			[]string{".config/zed"}, nil},
		{"goose", "Goose", "CLI / Harness", "goose", "https://goose-docs.ai/docs/getting-started/providers/",
			"goose configure → Custom Providers / OpenAI Compatible；Base URL={BASE_URL_V1}；Key=YOUR_API_KEY；模型={MODEL}。",
			[]string{".config/goose"}, nil},
		{"qwen-code", "Qwen Code", "CLI / Harness", "qwen", "https://qwenlm.github.io/qwen-code-docs/",
			"OPENAI_BASE_URL={BASE_URL_V1}；OPENAI_API_KEY=YOUR_API_KEY；OPENAI_MODEL={MODEL}。",
			[]string{".qwen"}, nil},
		{"cherry-studio", "Cherry Studio", "IDE / 桌面", "", "https://docs.cherry-ai.com/",
			"设置 → 模型服务 → OpenAI；地址={BASE_URL_V1}，Key=YOUR_API_KEY。",
			nil, nil},
		{"open-webui", "Open WebUI", "Web / 自部署", "open-webui", "https://docs.openwebui.com/",
			"Admin → Connections → OpenAI API，填 {BASE_URL_V1} 和 YOUR_API_KEY。",
			nil, nil},
	}
	for _, item := range manuals {
		cmds := []string{}
		if item.command != "" {
			cmds = []string{item.command}
		}
		protocol := "openai"
		if item.id == "cursor" || item.id == "zed" {
			protocol = "openai"
		}
		m[item.id] = &Client{
			ID: item.id, Label: item.label, Desc: "提供本机检测与明确接入指引，不直接改私有状态或凭据库。",
			Protocol: protocol, Mode: "manual", Category: item.category,
			Commands: cmds, ProbeDirs: item.dirs, Extensions: item.extensions,
			DocsURL: item.docs, Guide: item.guide,
			files: func(string) map[string]string { return map[string]string{} },
		}
	}
	return m
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func which(name string) string {
	if name == "" {
		return ""
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
