package qoder

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed catalog_cn.json
var catalogCN []byte

//go:embed catalog_intl.json
var catalogIntl []byte

// ModelInfo 单个 Qoder 对话模型（目录条目）。
// 未显式列出的远端字段（strategies/icon/status 等）原样丢弃，不影响出站请求。
type ModelInfo struct {
	Key           string `json:"key"`
	DisplayName   string `json:"display_name"`
	IsVL          bool   `json:"is_vl"`
	IsReasoning   bool   `json:"is_reasoning"`
	MaxInputToken int    `json:"max_input_tokens"`
	Enable        *bool  `json:"enable"`

	// 计费与元数据（面板展示 / 模型清单透出）。
	PriceFactor         float64        `json:"price_factor"`
	OriginalPriceFactor float64        `json:"original_price_factor"`
	IsFree              bool           `json:"is_free"`
	IsNew               bool           `json:"is_new"`
	ContextConfig       map[string]any `json:"context_config"`
	ThinkingConfig      map[string]any `json:"thinking_config"`
	Promotion           map[string]any `json:"promotion"`
}


// Enabled 缺省视为启用（目录条目未带 enable 时）。
func (m ModelInfo) Enabled() bool { return m.Enable == nil || *m.Enable }

// Name 返回对外展示名（缺省回落 key）。
func (m ModelInfo) Name() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Key
}

// NormalizeModelName 归一化模型名用于跨 provider 匹配：
// 小写并只保留字母数字（deepseek-v4-pro == DeepSeek-V4-Pro）。
func NormalizeModelName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type catalogState struct {
	models map[string][]ModelInfo // realm -> 有序模型列表
}

// Catalog 是 Qoder 双区域模型目录：内嵌快照兜底 + 动态刷新覆盖。
type Catalog struct {
	mu      sync.RWMutex
	static  map[string][]ModelInfo
	dynamic map[string][]ModelInfo
	dynAt   map[string]time.Time
	ttl     time.Duration
}

// NewCatalog 加载内嵌目录快照。
func NewCatalog() *Catalog {
	c := &Catalog{
		static:  map[string][]ModelInfo{"cn": parseStatic(catalogCN), "intl": parseStatic(catalogIntl)},
		dynamic: map[string][]ModelInfo{},
		dynAt:   map[string]time.Time{},
		ttl:     time.Hour,
	}
	return c
}

func parseStatic(raw []byte) []ModelInfo {
	var rows []ModelInfo
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, m := range rows {
		if m.Key == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Realms 返回目录覆盖的区域。
func (c *Catalog) Realms() []string { return []string{"cn", "intl"} }

// Models 返回某区域的模型列表（动态优先，未刷新则用内嵌快照）。
func (c *Catalog) Models(realm string) []ModelInfo {
	realm = NormalizeRealm(realm)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if rows, ok := c.dynamic[realm]; ok && len(rows) > 0 {
		return rows
	}
	return c.static[realm]
}

// ReplaceDynamic 用动态拉取结果覆盖某区域目录（拉取失败时不覆盖）。
func (c *Catalog) ReplaceDynamic(realm string, rows []ModelInfo) {
	realm = NormalizeRealm(realm)
	if len(rows) == 0 {
		return
	}
	c.mu.Lock()
	c.dynamic[realm] = rows
	c.dynAt[realm] = time.Now()
	c.mu.Unlock()
}

// Fresh 报告动态目录是否仍在 TTL 内。
func (c *Catalog) Fresh(realm string) bool {
	realm = NormalizeRealm(realm)
	c.mu.RLock()
	defer c.mu.RUnlock()
	at, ok := c.dynAt[realm]
	if !ok || len(c.dynamic[realm]) == 0 {
		return false
	}
	return time.Since(at) < c.ttl
}

// Lookup 在指定区域按 key / display_name（归一化）匹配模型。
func (c *Catalog) Lookup(realm, name string) (ModelInfo, bool) {
	target := NormalizeModelName(name)
	if target == "" {
		return ModelInfo{}, false
	}
	for _, m := range c.Models(realm) {
		if !m.Enabled() || m.Key == "auto" || m.Key == "ultimate" || m.Key == "performance" || m.Key == "efficient" {
			continue
		}
		if strings.EqualFold(m.Key, name) || NormalizeModelName(m.Key) == target ||
			NormalizeModelName(m.DisplayName) == target {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// LookupAny 跨区域按名称匹配，返回所有命中（区域去重）。
func (c *Catalog) LookupAny(name string) []ModelInfo {
	var out []ModelInfo
	seen := map[string]bool{}
	for _, realm := range c.Realms() {
		if m, ok := c.Lookup(realm, name); ok {
			if seen[m.Key] {
				continue
			}
			seen[m.Key] = true
			out = append(out, m)
		}
	}
	return out
}

// NormalizeRealm 把 Qoder 区域归一化：intl/global/国际版 -> intl，其余 cn。
func NormalizeRealm(realm string) string {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "intl", "global", "international":
		return "intl"
	}
	return "cn"
}
