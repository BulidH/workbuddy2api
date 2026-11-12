package qoder

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// effortRank 思考档位排序（与 Anthropic 桥接层共用）。
var effortRank = map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// Efforts 返回模型 thinking_config 声明的档位（升序）；未声明返回 nil。
func (m ModelInfo) Efforts() []string {
	src := m.ThinkingConfig
	if src == nil {
		return nil
	}
	var box any
	if en, ok := src["enabled"].(map[string]any); ok {
		box = en["efforts"]
	} else {
		box = src["efforts"]
	}
	var names []string
	switch v := box.(type) {
	case map[string]any:
		for k := range v {
			if _, ok := effortRank[strings.ToLower(k)]; ok {
				names = append(names, strings.ToLower(k))
			}
		}
	case []any:
		for _, x := range v {
			s, _ := x.(string)
			s = strings.ToLower(strings.TrimSpace(s))
			if _, ok := effortRank[s]; ok {
				names = append(names, s)
			}
		}
	}
	sort.Slice(names, func(i, j int) bool { return effortRank[names[i]] < effortRank[names[j]] })
	return names
}

// DefaultEffort 返回模型默认思考档位；无声明返回空串。
func (m ModelInfo) DefaultEffort() string {
	en, _ := m.ThinkingConfig["enabled"].(map[string]any)
	if en == nil {
		return ""
	}
	efforts, _ := en["efforts"].(map[string]any)
	for name, raw := range efforts {
		cfg, _ := raw.(map[string]any)
		if b, _ := cfg["is_default"].(bool); b {
			if _, ok := effortRank[strings.ToLower(name)]; ok {
				return strings.ToLower(name)
			}
		}
	}
	return ""
}

// ContextWindow 返回默认上下文窗口（context_config.is_default，缺省 max_input_tokens）。
func (m ModelInfo) ContextWindow() int {
	if m.ContextConfig != nil {
		for _, raw := range m.ContextConfig {
			cfg, _ := raw.(map[string]any)
			if b, _ := cfg["is_default"].(bool); b {
				if n := intFromAny(cfg["token_count"]); n > 0 {
					return n
				}
			}
		}
		// 没有 is_default 时取最大窗口（更宽松，避免客户端误截断）。
		max := 0
		for _, raw := range m.ContextConfig {
			cfg, _ := raw.(map[string]any)
			if n := intFromAny(cfg["token_count"]); n > max {
				max = n
			}
		}
		if max > 0 {
			return max
		}
	}
	return m.MaxInputToken
}

// PromotionView 返回峰谷价信息（无 promotion 返回 nil）。
func (m ModelInfo) PromotionView(now time.Time) map[string]any {
	if m.Promotion == nil {
		return nil
	}
	active := boolFromAny(m.Promotion["active"])
	view := map[string]any{
		"active":        active,
		"active_now":    active && m.OffPeakActive(now),
		"window_start":  m.Promotion["window_start"],
		"window_end":    m.Promotion["window_end"],
		"timezone":      m.Promotion["timezone"],
		"discount":      m.Promotion["discount_factor"],
		"badge":         m.Promotion["badge"],
		"description":   m.Promotion["description"],
		"peak_factor":   m.Promotion["before_promotion_price_factor"],
		"valley_factor": m.valleyFactor(),
	}
	return view
}

// valleyFactor = peak × discount（官方口径）。
func (m ModelInfo) valleyFactor() float64 {
	peak := floatFromAny(m.Promotion["before_promotion_price_factor"])
	disc := floatFromAny(m.Promotion["discount_factor"])
	if peak <= 0 || disc <= 0 {
		return 0
	}
	return peak * disc
}

// OffPeakActive 判断当前是否处于促销低谷窗口（支持跨午夜，如 22:00-08:00）。
func (m ModelInfo) OffPeakActive(now time.Time) bool {
	if !boolFromAny(m.Promotion["active"]) {
		return false
	}
	locName, _ := m.Promotion["timezone"].(string)
	loc := time.FixedZone("CST", 8*3600)
	if locName != "" {
		if l, err := time.LoadLocation(locName); err == nil {
			loc = l
		}
	}
	startS, _ := m.Promotion["window_start"].(string)
	endS, _ := m.Promotion["window_end"].(string)
	sh, sm, ok1 := parseClock(startS)
	eh, em, ok2 := parseClock(endS)
	if !ok1 || !ok2 {
		return false
	}
	t := now.In(loc)
	cur := t.Hour()*60 + t.Minute()
	start, end := sh*60+sm, eh*60+em
	if start == end {
		return true
	}
	if start < end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end
}

func parseClock(s string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func floatFromAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func boolFromAny(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	}
	return false
}

// EffortListing 返回对外透出的档位元数据；档位为空时返回 nil（不下发空数组）。
func (m ModelInfo) EffortListing() ([]string, string) {
	efforts := m.Efforts()
	if len(efforts) == 0 {
		return nil, ""
	}
	return efforts, m.DefaultEffort()
}

// MetaView 返回模型清单可透出的计费/窗口/思考元数据。
func (m ModelInfo) MetaView(now time.Time, prices map[string]Price) map[string]any {
	out := map[string]any{
		"context_length": m.ContextWindow(),
		"price_factor":   m.PriceFactor,
		"is_free":        m.IsFree,
	}
	if m.OriginalPriceFactor > 0 {
		out["original_price_factor"] = m.OriginalPriceFactor
	}
	if m.IsNew {
		out["is_new"] = true
	}
	if efforts, def := m.EffortListing(); efforts != nil {
		out["reasoning_supported_efforts"] = efforts
		if def != "" {
			out["reasoning_default_effort"] = def
		}
	}
	if promo := m.PromotionView(now); promo != nil {
		out["promotion"] = promo
	}
	if p, ok := PriceFor(m.Key, prices); ok {
		out["pricing"] = map[string]any{
			"currency": "USD",
			"unit":     "1M tokens",
			"in":       p.In,
			"cached":   p.Cached,
			"out":      p.Out,
		}
	}
	return out
}
