package qoder

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

//go:embed baseprompt.json
var basePromptRaw []byte

// basePrompt 是官方 CLI 的请求骨架（每次请求深拷贝后覆写）。
var basePrompt map[string]any

func init() {
	if err := json.Unmarshal(basePromptRaw, &basePrompt); err != nil {
		panic("qoder: invalid embedded baseprompt.json: " + err.Error())
	}
}

// BuildRequest 把 OpenAI Chat Completions 请求体转换成 Qoder agent_chat_generation 请求体。
func BuildRequest(req map[string]any, acct QoderAccount, meta ModelInfo, realm string) (map[string]any, error) {
	body, err := deepCopyMap(basePrompt)
	if err != nil {
		return nil, err
	}
	messages, _ := req["messages"].([]any)
	systemText, flat := flattenMessages(messages)

	tmplSystem := make([]any, 0, 1)
	if rows, ok := body["messages"].([]any); ok {
		for _, m := range rows {
			if mm, ok := m.(map[string]any); ok && (mm["role"] == "system" || mm["role"] == "developer") {
				tmplSystem = append(tmplSystem, mm)
			}
		}
	}
	if systemText == nil {
		body["messages"] = tmplSystem
	} else {
		body["messages"] = []any{map[string]any{"role": "system", "content": *systemText}}
	}
	if rows, ok := body["messages"].([]any); ok {
		for _, m := range flat {
			rows = append(rows, m)
		}
		body["messages"] = rows
	}

	prompt := lastUserText(flat)
	nid := newUUID4()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = newUUID4()
	body["session_id"] = newUUID4()
	body["stream"] = true
	body["agent_id"] = "agent_common"
	body["aliyun_user_type"] = acct.QoderUserType()

	mc, _ := body["model_config"].(map[string]any)
	if mc == nil {
		mc = map[string]any{}
	}
	maxInput := meta.MaxInputToken
	if maxInput <= 0 {
		maxInput = 180000
	}
	mc["key"] = meta.Key
	mc["display_name"] = meta.Name()
	mc["model"] = ""
	mc["format"] = "openai"
	mc["is_vl"] = meta.IsVL
	mc["is_reasoning"] = meta.IsReasoning
	mc["api_key"] = ""
	mc["url"] = ""
	mc["source"] = "system"
	mc["max_input_tokens"] = maxInput
	body["model_config"] = mc

	cc, _ := body["chat_context"].(map[string]any)
	if cc == nil {
		cc = map[string]any{}
	}
	txt, _ := cc["text"].(map[string]any)
	if txt == nil {
		txt = map[string]any{}
	}
	txt["text"] = prompt
	cc["text"] = txt
	extra, _ := cc["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
	}
	oc, _ := extra["originalContent"].(map[string]any)
	if oc == nil {
		oc = map[string]any{}
	}
	oc["text"] = prompt
	extra["originalContent"] = oc
	extra["modelConfig"] = cloneMap(mc)
	cc["extra"] = extra
	body["chat_context"] = cc

	params, _ := body["parameters"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	if v, ok := intField(req, "max_tokens", "max_completion_tokens"); ok {
		params["max_tokens"] = v
	}
	if effort := reasoningEffort(req); effort != "" {
		params["reasoning_effort"] = effort
	}
	body["parameters"] = params

	body["tools"] = normalizeTools(req["tools"])

	biz, _ := body["business"].(map[string]any)
	if biz == nil {
		biz = map[string]any{}
	}
	biz["id"] = newUUID4()
	biz["begin_at"] = time.Now().UnixMilli()
	if prompt != "" {
		biz["name"] = truncateRunes(prompt, 30)
	} else {
		biz["name"] = "chat"
	}
	biz["model_key"] = meta.Key
	body["business"] = biz
	return body, nil
}

// QoderAccount 是构造请求所需的账号最小视图（避免 qoder 包反向依赖 auth）。
type QoderAccount struct {
	UID       string
	Nickname  string
	UserType  string
	OrgID     string
	OrgName   string
	AccessTok string
	Refresh   string
}

// QoderUserType 返回账号用户类型（缺省 personal_professional_trial）。
func (a QoderAccount) QoderUserType() string {
	if a.UserType == "" {
		return defaultUserType
	}
	return a.UserType
}

func deepCopyMap(src map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// flattenMessages 把客户端会话压平成 Qoder 上游可接受的 {role, content} 序列。
// 返回 (首个 system 文本, 会话)。tool 结果与 assistant tool_calls 采用结构化直传。
func flattenMessages(messages []any) (*string, []map[string]any) {
	var systemText *string
	flat := make([]map[string]any, 0, len(messages))
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strField(m, "role")
		text, parts, hasImage := flattenContent(m["content"])
		if role == "system" || role == "developer" {
			if systemText == nil && text != "" {
				system := text
				systemText = &system
			}
			continue
		}
		switch role {
		case "tool":
			item := map[string]any{
				"role":         "tool",
				"tool_call_id": strField(m, "tool_call_id"),
				"content":      text,
			}
			flat = append(flat, item)
		case "assistant":
			if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
				norm := make([]any, 0, len(calls))
				for _, c := range calls {
					tc, _ := c.(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					args := fn["arguments"]
					switch v := args.(type) {
					case string:
					case nil:
						args = ""
					default:
						b, _ := json.Marshal(v)
						args = string(b)
					}
					norm = append(norm, map[string]any{
						"id":   strField(tc, "id"),
						"type": defaultStr(strField(tc, "type"), "function"),
						"function": map[string]any{
							"name":      strField(fn, "name"),
							"arguments": args,
						},
					})
				}
				item := map[string]any{"role": "assistant", "content": text, "tool_calls": norm}
				if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
					item["reasoning_content"] = rc
				}
				flat = append(flat, item)
				continue
			}
			content := any(text)
			if hasImage {
				content = parts
			}
			item := map[string]any{"role": "assistant", "content": content}
			if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
				item["reasoning_content"] = rc
			}
			flat = append(flat, item)
		case "user":
			content := any(text)
			if hasImage {
				content = parts
			}
			flat = append(flat, map[string]any{"role": "user", "content": content})
		default:
			if role != "" {
				flat = append(flat, map[string]any{"role": "user", "content": text})
			}
		}
	}
	return systemText, flat
}

// flattenContent 把 message.content 压平成文本；带图时同时保留结构化 parts。
func flattenContent(content any) (text string, parts []any, hasImage bool) {
	switch v := content.(type) {
	case string:
		return v, nil, false
	case []any:
		var b strings.Builder
		for _, p := range v {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch strField(part, "type") {
			case "text":
				b.WriteString(strField(part, "text"))
				// 保留文字 part：带图消息用 parts 承载内容，丢掉文字会让
				// chat_context/business 的 prompt 退化到上一条用户消息。
				parts = append(parts, part)
			case "image_url":
				hasImage = true
				parts = append(parts, part)
			default:
				parts = append(parts, part)
			}
		}
		return b.String(), parts, hasImage
	default:
		return "", nil, false
	}
}

func lastUserText(flat []map[string]any) string {
	for i := len(flat) - 1; i >= 0; i-- {
		if flat[i]["role"] != "user" {
			continue
		}
		switch c := flat[i]["content"].(type) {
		case string:
			if c != "" {
				return c
			}
		case []any:
			var b strings.Builder
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok && strField(pm, "type") == "text" {
					b.WriteString(strField(pm, "text"))
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	return ""
}

// normalizeTools 统一成 {type:"function", function:{...}} 形态（上游要求）。
func normalizeTools(raw any) []any {
	rows, ok := raw.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		t, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := t["function"].(map[string]any); ok {
			if strField(fn, "name") == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": defaultStr(strField(t, "type"), "function"),
				"function": map[string]any{
					"name":        strField(fn, "name"),
					"description": strField(fn, "description"),
					"parameters":  fn["parameters"],
				},
			})
			continue
		}
		if name := strField(t, "name"); name != "" {
			out = append(out, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": strField(t, "description"),
					"parameters":  t["parameters"],
				},
			})
		}
	}
	return out
}

func strField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func intField(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			if v > 0 {
				return int(v), true
			}
		case json.Number:
			if n, err := v.Int64(); err == nil && n > 0 {
				return int(n), true
			}
		case string:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

func reasoningEffort(req map[string]any) string {
	if s := strings.ToLower(strings.TrimSpace(strField(req, "reasoning_effort"))); s != "" {
		return s
	}
	if r, ok := req["reasoning"].(map[string]any); ok {
		return strings.ToLower(strings.TrimSpace(strField(r, "effort")))
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
