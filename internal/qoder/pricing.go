package qoder

import (
	"encoding/json"
	"os"
	"strings"
)

// Price 一组 USD / 1M tokens 参考单价。
type Price struct {
	In     float64 `json:"in"`
	Cached float64 `json:"cached"`
	Out    float64 `json:"out"`
}

// defaultPrices 对齐 qoder2api-hub 的内置参考价（仅用于面板估算，不参与结算）。
var defaultPrices = map[string]Price{
	"dfmodel":       {0.28, 0.028, 0.42},
	"dmodel":        {0.55, 0.14, 2.19},
	"gmodel":        {0.60, 0.15, 2.20},
	"gfmodel":       {0.06, 0.015, 0.40},
	"kmodel":        {0.60, 0.15, 2.50},
	"kmodel_latest": {0.60, 0.15, 2.50},
	"qmodel":        {0.40, 0.10, 1.20},
	"qmodel_latest": {1.20, 0.30, 6.00},
	"qmodel_38max":  {1.20, 0.30, 6.00},
	"qfmodel":       {0.05, 0.012, 0.20},
	"mmodel":        {0.30, 0.075, 1.20},
	"cmodel":        {3.00, 0.75, 15.00},
}

var priceAliases = map[string]string{
	"deepseek": "dfmodel", "deepseek-flash": "dfmodel",
	"deepseek-v4-flash": "dfmodel", "deepseek-v4-pro": "dmodel",
	"glm": "gmodel", "glm-5.3": "gmodel", "glm-5.3-flash": "gfmodel",
	"kimi": "kmodel", "kimi-k2.8-preview": "kmodel", "kimi-k3": "kmodel_latest",
	"qwen3.7-plus": "qmodel", "qwen3.7-max": "qmodel_latest",
	"qwen3.8-max": "qmodel_38max", "qwen3.8-flash": "qfmodel",
	"minimax-m3": "mmodel", "cantus": "cmodel",
}

// LoadPriceOverrides 读取用户覆盖价（JSON: {"model": {"in":..,"cached":..,"out":..}}）。
// 文件不存在或损坏时返回空表，绝不影响主链路。
func LoadPriceOverrides(path string) map[string]Price {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil
	}
	out := map[string]Price{}
	for k, v := range data {
		switch val := v.(type) {
		case map[string]any:
			p := Price{
				In:     numOr(val, "in"),
				Cached: numOr(val, "cached"),
				Out:    numOr(val, "out"),
			}
			if p.Cached == 0 {
				p.Cached = p.In
			}
			out[normalizeKey(k)] = p
		}
	}
	return out
}

// PriceFor 按模型 key / 显示名 / 别名查参考价；未知模型返回 false（不猜数字）。
func PriceFor(name string, overrides map[string]Price) (Price, bool) {
	key := normalizeKey(name)
	if key == "" {
		return Price{}, false
	}
	if p, ok := overrides[key]; ok {
		return p, true
	}
	if p, ok := defaultPrices[key]; ok {
		return p, true
	}
	if target, ok := priceAliases[key]; ok {
		return defaultPrices[target], true
	}
	for alias, target := range priceAliases {
		if strings.HasPrefix(key, alias+"-") || strings.HasPrefix(key, alias+"_") {
			return defaultPrices[target], true
		}
	}
	return Price{}, false
}

// CostOf 按 token 数估算 USD；未知模型返回 false。
func CostOf(name string, prompt, cached, completion int64, overrides map[string]Price) (float64, bool) {
	p, ok := PriceFor(name, overrides)
	if !ok {
		return 0, false
	}
	if prompt < 0 {
		prompt = 0
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	if completion < 0 {
		completion = 0
	}
	return (float64(prompt-cached)*p.In + float64(cached)*p.Cached + float64(completion)*p.Out) / 1e6, true
}

// PriceTableView 输出生效价格表（内置 + 覆盖）。
func PriceTableView(overrides map[string]Price) map[string]any {
	rows := map[string]any{}
	for key, p := range defaultPrices {
		rows[key] = map[string]any{"in": p.In, "cached": p.Cached, "out": p.Out, "source": "builtin"}
	}
	for key, p := range overrides {
		rows[key] = map[string]any{"in": p.In, "cached": p.Cached, "out": p.Out, "source": "override"}
	}
	return rows
}

func normalizeKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
