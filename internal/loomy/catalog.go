package loomy

import (
	"strings"
	"sync"
	"time"
)

// Catalog 是 Loomy 模型目录：内嵌兜底清单 + 上游动态刷新覆盖。
//
// 目录用途有二：一是 /v1/models 合并展示，二是裸名合并调度时判断「这个 Provider
// 认不认识该模型」——认不认识决定是否把它放进候选链路。
type Catalog struct {
	mu      sync.RWMutex
	dynamic []ModelInfo
	dynAt   time.Time
	ttl     time.Duration
}

// NewCatalog 构建目录（初始为兜底清单）。
func NewCatalog() *Catalog {
	return &Catalog{ttl: time.Hour}
}

// Replace 用上游拉取结果覆盖（空结果不覆盖）。
func (c *Catalog) Replace(rows []ModelInfo) {
	if len(rows) == 0 {
		return
	}
	c.mu.Lock()
	c.dynamic = rows
	c.dynAt = time.Now()
	c.mu.Unlock()
}

// Fresh 报告动态目录是否仍在 TTL 内。
func (c *Catalog) Fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.dynamic) > 0 && time.Since(c.dynAt) < c.ttl
}

// Models 返回当前目录（动态优先，否则兜底）。
func (c *Catalog) Models() []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.dynamic) > 0 {
		return c.dynamic
	}
	out := make([]ModelInfo, 0, len(FallbackModels))
	for _, m := range FallbackModels {
		out = append(out, ModelInfo{ID: m.ID, Name: m.Name, ContextLength: int64(m.ContextLength)})
	}
	return out
}

// Lookup 按模型名匹配目录（大小写/连字符不敏感）。
//
// 兜底清单也参与匹配：目录尚未刷新时也要能判断出「这是 Loomy 认得的模型」，
// 否则冷启动阶段所有 Loomy 路由都会被判成「不支持」而跳过。
func (c *Catalog) Lookup(name string) (ModelInfo, bool) {
	target := NormalizeModelName(name)
	if target == "" {
		return ModelInfo{}, false
	}
	for _, m := range c.Models() {
		if strings.EqualFold(m.ID, name) || NormalizeModelName(m.ID) == target ||
			NormalizeModelName(m.Name) == target {
			return m, true
		}
	}
	for _, m := range FallbackModels {
		if strings.EqualFold(m.ID, name) || NormalizeModelName(m.ID) == target ||
			NormalizeModelName(m.Name) == target {
			return ModelInfo{ID: m.ID, Name: m.Name, ContextLength: int64(m.ContextLength)}, true
		}
	}
	return ModelInfo{}, false
}

// NormalizeModelName 归一化模型名（小写，只保留字母数字）。
func NormalizeModelName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
