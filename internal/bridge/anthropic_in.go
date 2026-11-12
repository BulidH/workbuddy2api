// Package bridge 把 Anthropic Messages / OpenAI Responses 协议翻译成网关内部的
// Chat Completions 形状，并把返回的 chat 对象 / SSE 片段翻译回对应协议。
//
// 设计边界（与 qoder2api-hub 的 qoder_anthropic 对齐）：
//   - 只做协议翻译，不做账号/路由决策；形态决策（结构化工具历史）由上游 body 构造
//     单点负责；
//   - 无法生成 Anthropic thinking signature：默认不暴露 thinking，可用
//     ThinkingMode=empty 暴露空签名块；
//   - 错误统一走各自协议的 error 形状。
package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

var budgetSteps = []struct {
	limit int64
	name  string
}{
	{1024, "minimal"},
	{4096, "low"},
	{16384, "medium"},
	{32768, "high"},
	{0, "max"},
}

// ValidateAnthropic 校验 Anthropic 必填项。
func ValidateAnthropic(payload map[string]any) (bool, string) {
	if payload == nil {
		return false, "request body must be a JSON object"
	}
	if msgs, _ := payload["messages"].([]any); len(msgs) == 0 {
		return false, "messages is required"
	}
	mt, ok := payload["max_tokens"]
	if !ok || mt == nil {
		return false, "max_tokens is required"
	}
	switch v := mt.(type) {
	case float64:
		if v <= 0 {
			return false, "max_tokens must be positive"
		}
	case int:
		if v <= 0 {
			return false, "max_tokens must be positive"
		}
	default:
		return false, "max_tokens must be an integer"
	}
	return true, ""
}

// MessagesToChat 把 Anthropic /v1/messages 请求翻译为 Chat Completions 请求。
func MessagesToChat(payload map[string]any) map[string]any {
	messages := make([]any, 0, len(asSlice(payload["messages"]))+1)
	for _, raw := range asSlice(payload["messages"]) {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		blocks := anthropicBlocks(m["content"])
		var texts, images []string
		var toolCalls, toolResults []any
		for _, b := range blocks {
			bt, _ := b["type"].(string)
			if bt == "" {
				if s, ok := b["text"].(string); ok {
					texts = append(texts, s)
				}
				continue
			}
			switch bt {
			case "text":
				if s, ok := b["text"].(string); ok {
					texts = append(texts, s)
				}
			case "image":
				if uri := imageDataURI(b); uri != "" {
					images = append(images, uri)
				}
			case "tool_use":
				args := b["input"]
				if s, ok := args.(string); ok {
					var decoded any
					if json.Unmarshal([]byte(s), &decoded) == nil {
						if _, ok := decoded.(map[string]any); ok {
							args = decoded
						} else if _, ok := decoded.([]any); ok {
							args = decoded
						}
					}
				}
				if _, ok := args.(string); !ok {
					raw, _ := json.Marshal(args)
					args = string(raw)
				}
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				toolResults = append(toolResults, map[string]any{
					"role": "tool", "tool_call_id": id,
					"content": toolResultText(b),
				})
			case "thinking":
				// Anthropic 历史里的 thinking 不进入上游（无法验证签名）。
			}
		}
		for _, tr := range toolResults {
			messages = append(messages, tr)
		}
		text := strings.Join(texts, "")
		switch {
		case len(toolCalls) > 0:
			messages = append(messages, map[string]any{
				"role": "assistant", "content": text, "tool_calls": toolCalls,
			})
		case role == "system":
			// messages 内的 system：就地降级为 user，避免污染最前面的缓存前缀。
			if strings.TrimSpace(text) != "" {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
		case len(images) > 0:
			parts := []any{}
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, u := range images {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			}
			messages = append(messages, map[string]any{"role": role, "content": parts})
		case text != "":
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
	}
	if sys := systemPrompt(payload); sys != "" {
		messages = append([]any{map[string]any{"role": "system", "content": sys}}, messages...)
	}

	chat := map[string]any{"messages": messages}
	for _, k := range []string{"model", "max_tokens", "temperature", "top_p"} {
		if v, ok := payload[k]; ok && v != nil {
			chat[k] = v
		}
	}
	if stops := asSlice(payload["stop_sequences"]); len(stops) > 0 {
		chat["stop"] = stops
	}
	if tools := toolsToChat(payload["tools"]); len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc, ok := payload["tool_choice"]; ok && tc != nil {
		chat["tool_choice"] = toolChoiceToChat(tc)
	}
	if effort := effortFromAnthropic(payload); effort != "" {
		chat["reasoning_effort"] = effort
	}
	if meta, _ := payload["metadata"].(map[string]any); meta != nil {
		if uid, _ := meta["user_id"].(string); uid != "" {
			chat["metadata"] = map[string]any{"user_id": uid}
		}
	}
	return chat
}

func anthropicBlocks(content any) []map[string]any {
	switch v := content.(type) {
	case string:
		return []map[string]any{{"type": "text", "text": v}}
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, raw := range v {
			switch b := raw.(type) {
			case string:
				out = append(out, map[string]any{"type": "text", "text": b})
			case map[string]any:
				out = append(out, b)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

func imageDataURI(block map[string]any) string {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch src["type"] {
	case "base64":
		mt, _ := src["media_type"].(string)
		if mt == "" {
			mt = "image/png"
		}
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		return "data:" + mt + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

func toolResultText(block map[string]any) string {
	c := block["content"]
	if s, ok := c.(string); ok {
		return s
	}
	parts := []string{}
	for _, raw := range asSlice(c) {
		switch b := raw.(type) {
		case string:
			parts = append(parts, b)
		case map[string]any:
			if b["type"] == nil || b["type"] == "text" {
				if s, ok := b["text"].(string); ok {
					parts = append(parts, s)
				}
			} else if t, ok := b["type"].(string); ok {
				parts = append(parts, fmt.Sprintf("[%s omitted]", t))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func systemPrompt(payload map[string]any) string {
	var parts []string
	switch sys := payload["system"].(type) {
	case string:
		if strings.TrimSpace(sys) != "" {
			parts = append(parts, sys)
		}
	case []any:
		var b strings.Builder
		for _, raw := range sys {
			block, _ := raw.(map[string]any)
			if s, ok := block["text"].(string); ok {
				b.WriteString(s)
			}
		}
		if strings.TrimSpace(b.String()) != "" {
			parts = append(parts, b.String())
		}
	}
	return strings.Join(parts, "\n\n")
}

func toolsToChat(raw any) []any {
	out := []any{}
	for _, item := range asSlice(raw) {
		t, _ := item.(map[string]any)
		if t == nil {
			continue
		}
		if _, ok := t["function"].(map[string]any); ok {
			out = append(out, t)
			continue
		}
		name, _ := t["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		schema, _ := t["input_schema"].(map[string]any)
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		desc, _ := t["description"].(string)
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": desc, "parameters": schema,
		}})
	}
	return out
}

func toolChoiceToChat(tc any) any {
	if s, ok := tc.(string); ok {
		switch s {
		case "any":
			return "required"
		case "none":
			return "none"
		}
		return "auto"
	}
	m, _ := tc.(map[string]any)
	switch m["type"] {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name, _ := m["name"].(string); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return "auto"
}

func effortFromAnthropic(payload map[string]any) string {
	target := ""
	if th, _ := payload["thinking"].(map[string]any); th != nil {
		if strings.EqualFold(fmt.Sprint(th["type"]), "disabled") {
			return ""
		}
		if e, _ := th["effort"].(string); e != "" {
			target = strings.ToLower(e)
		}
		if target == "" {
			switch b := th["budget_tokens"].(type) {
			case float64:
				target = budgetToEffort(int64(b))
			case int:
				target = budgetToEffort(int64(b))
			}
		}
	}
	if oc, _ := payload["output_config"].(map[string]any); oc != nil {
		if e, _ := oc["effort"].(string); e != "" {
			target = strings.ToLower(e)
		}
	}
	if _, ok := effortRank[target]; !ok {
		return ""
	}
	return target
}

func budgetToEffort(budget int64) string {
	for _, step := range budgetSteps {
		if step.limit == 0 || budget <= step.limit {
			return step.name
		}
	}
	return "max"
}

// EstimateAnthropicTokens 本地估算（CJK 感知，不联网），用于 count_tokens。
func EstimateAnthropicTokens(payload map[string]any) int64 {
	var sb strings.Builder
	sb.WriteString(systemPrompt(payload))
	for _, raw := range asSlice(payload["messages"]) {
		m, _ := raw.(map[string]any)
		switch c := m["content"].(type) {
		case string:
			sb.WriteString(c)
		case []any:
			for _, b := range c {
				if bm, ok := b.(map[string]any); ok {
					if s, ok := bm["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
		}
	}
	if tools, err := json.Marshal(payload["tools"]); err == nil {
		sb.Write(tools)
	}
	cjk, other := 0, 0
	for _, r := range sb.String() {
		if r > 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	n := int64(cjk + (other+3)/4)
	if n < 1 {
		n = 1
	}
	return n
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
