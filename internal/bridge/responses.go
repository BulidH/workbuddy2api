package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ResponsesToChat 把 Responses API 请求翻译成 Chat Completions 请求。
// 只保留上游能表达的部分：function/custom 工具（custom 降级为 function），
// namespace 容器里的子工具会展开；web_search 等 Responses 专有声明由调用方
// 的 web 工具分支处理。
func ResponsesToChat(payload map[string]any) map[string]any {
	messages := []any{}
	if ins, _ := payload["instructions"].(string); strings.TrimSpace(ins) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": ins})
	}
	pendingReasoning := ""
	appendAssistant := func(item map[string]any) map[string]any {
		if len(messages) > 0 {
			if prev, _ := messages[len(messages)-1].(map[string]any); prev != nil && prev["role"] == "assistant" {
				return prev
			}
		}
		m := map[string]any{"role": "assistant", "content": ""}
		if pendingReasoning != "" {
			m["reasoning_content"] = pendingReasoning
			pendingReasoning = ""
		}
		messages = append(messages, m)
		return m
	}
	addToolCall := func(tc map[string]any) {
		prev := appendAssistant(nil)
		list, _ := prev["tool_calls"].([]any)
		prev["tool_calls"] = append(list, tc)
	}

	switch inp := payload["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": inp})
	case []any:
		for _, raw := range inp {
			switch item := raw.(type) {
			case string:
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				itype := strAny(item["type"])
				switch itype {
				case "", "message":
					body := flattenResponseContent(item["content"])
					if body == "" {
						continue
					}
					role := strAny(item["role"])
					if role == "" {
						role = "user"
					}
					if role == "developer" {
						role = "system"
					}
					if prev, _ := messages[len(messages)-1].(map[string]any); prev != nil && role == "assistant" && prev["role"] == "assistant" {
						if s, _ := prev["content"].(string); s != "" {
							prev["content"] = s + "\n" + body
						} else {
							prev["content"] = body
						}
						if pendingReasoning != "" {
							prev["reasoning_content"] = pendingReasoning
							pendingReasoning = ""
						}
						continue
					}
					msg := map[string]any{"role": role, "content": body}
					if role == "assistant" && pendingReasoning != "" {
						msg["reasoning_content"] = pendingReasoning
						pendingReasoning = ""
					}
					messages = append(messages, msg)
				case "reasoning":
					text := reasoningText(item)
					if text == "" {
						continue
					}
					if prev, _ := messages[len(messages)-1].(map[string]any); prev != nil && prev["role"] == "assistant" {
						prev["reasoning_content"] = text
					} else {
						pendingReasoning = text
					}
				case "function_call_output", "custom_tool_call_output":
					content := flattenResponseContent(item["output"])
					if content == "" {
						if s, ok := item["output"].(string); ok {
							content = s
						} else if item["output"] != nil {
							raw, _ := json.Marshal(item["output"])
							content = string(raw)
						}
					}
					messages = append(messages, map[string]any{
						"role": "tool", "tool_call_id": strAny(item["call_id"]), "content": content,
					})
				case "function_call":
					args := strAny(item["arguments"])
					if args == "" {
						args = "{}"
					}
					addToolCall(map[string]any{
						"id":   firstNonEmpty(strAny(item["call_id"]), strAny(item["id"])),
						"type": "function",
						"function": map[string]any{
							"name": strAny(item["name"]), "arguments": args,
						},
					})
				case "custom_tool_call":
					rawIn := item["input"]
					var s string
					if str, ok := rawIn.(string); ok {
						s = str
					} else if rawIn != nil {
						raw, _ := json.Marshal(rawIn)
						s = string(raw)
					}
					args, _ := json.Marshal(map[string]any{"input": s})
					addToolCall(map[string]any{
						"id":   firstNonEmpty(strAny(item["call_id"]), strAny(item["id"])),
						"type": "function",
						"function": map[string]any{
							"name": strAny(item["name"]), "arguments": string(args),
						},
					})
				}
			}
		}
	}

	chat := map[string]any{"messages": messages}
	for _, k := range []string{"model", "temperature", "top_p", "parallel_tool_calls"} {
		if v, ok := payload[k]; ok && v != nil {
			chat[k] = v
		}
	}
	if v, ok := payload["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if e, _ := payload["reasoning"].(map[string]any); e != nil {
		if eff, _ := e["effort"].(string); eff != "" {
			chat["reasoning_effort"] = eff
		}
	}
	if tools := responsesToolsToChat(payload); len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc, ok := payload["tool_choice"]; ok && tc != nil {
		chat["tool_choice"] = responsesToolChoice(tc)
	}
	if fmtv, _ := payload["text"].(map[string]any); fmtv != nil {
		if f, _ := fmtv["format"].(map[string]any); f != nil && strAny(f["type"]) == "json_object" {
			chat["response_format"] = map[string]any{"type": "json_object"}
		}
	}
	return chat
}

func reasoningText(item map[string]any) string {
	switch s := item["summary"].(type) {
	case string:
		if s != "" {
			return s
		}
	case []any:
		parts := []string{}
		for _, raw := range s {
			if p, _ := raw.(map[string]any); p != nil {
				if t, _ := p["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	switch c := item["content"].(type) {
	case string:
		return c
	case []any:
		return flattenResponseContent(c)
	}
	return ""
}

func flattenResponseContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, raw := range v {
			switch b := raw.(type) {
			case string:
				parts = append(parts, b)
			case map[string]any:
				switch strAny(b["type"]) {
				case "", "input_text", "output_text", "text":
					if s, ok := b["text"].(string); ok {
						parts = append(parts, s)
					}
				case "input_image", "image_url", "image":
					parts = append(parts, "[image omitted]")
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if s, ok := v["text"].(string); ok {
			return s
		}
	}
	return ""
}

// responsesToolsToChat 展开 namespace 容器，custom 工具降级为 function。
func responsesToolsToChat(payload map[string]any) []any {
	out := []any{}
	seen := map[string]bool{}
	var add func(raw any)
	add = func(raw any) {
		t, _ := raw.(map[string]any)
		if t == nil {
			return
		}
		name := strAny(t["name"])
		if name != "" && seen[name] {
			return
		}
		if name != "" {
			seen[name] = true
		}
		kind := strAny(t["type"])
		if kind == "" || kind == "function" {
			// Responses 的 function 声明是扁平的（name 在顶层），转成 chat 嵌套形状。
			if _, nested := t["function"].(map[string]any); nested {
				out = append(out, t)
				return
			}
			params, _ := t["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, map[string]any{"type": "function", "function": map[string]any{
				"name": name, "description": strAny(t["description"]), "parameters": params,
			}})
			return
		}
		if kind == "custom" {
			hint := "\n\nThis tool takes one freeform string argument named input."
			out = append(out, map[string]any{"type": "function", "function": map[string]any{
				"name":        name,
				"description": strAny(t["description"]) + hint,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"input": map[string]any{"type": "string", "description": "Complete raw payload for this tool, verbatim."},
					},
					"required": []any{"input"},
				},
			}})
			return
		}
		if kind == "namespace" {
			for _, key := range []string{"tools", "children", "value"} {
				if children, ok := t[key].([]any); ok {
					for _, c := range children {
						add(c)
					}
					return
				}
			}
		}
		// web_search / web_search_preview / file_search 等由调用方 web 分支或忽略处理。
	}
	for _, raw := range asSlice(payload["tools"]) {
		add(raw)
	}
	for _, raw := range asSlice(payload["input"]) {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		if strAny(item["type"]) == "additional_tools" {
			for _, c := range asSlice(item["tools"]) {
				add(c)
			}
		}
	}
	return out
}

func responsesToolChoice(v any) any {
	switch tc := v.(type) {
	case string:
		return tc
	case map[string]any:
		if strAny(tc["type"]) == "function" && strAny(tc["name"]) != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": strAny(tc["name"])}}
		}
	}
	return "auto"
}

// ChatToResponse 把 chat.completion 翻译为 Responses API 对象。
func ChatToResponse(chat map[string]any, model string) map[string]any {
	choices, _ := chat["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	msg, _ := choice["message"].(map[string]any)
	output := []any{}
	if rc := strAny(msg["reasoning_content"]); rc != "" {
		output = append(output, map[string]any{
			"id": newBridgeID("rs_"), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
		})
	}
	text := ""
	switch c := msg["content"].(type) {
	case string:
		text = c
	case []any:
		for _, p := range c {
			if pm, _ := p.(map[string]any); pm != nil {
				if s, ok := pm["text"].(string); ok {
					text += s
				}
			}
		}
	}
	if text != "" {
		output = append(output, map[string]any{
			"id": newBridgeID("msg_"), "type": "message", "role": "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		})
	}
	for _, raw := range asSlice(msg["tool_calls"]) {
		tc, _ := raw.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		name := strAny(fn["name"])
		if name == "" {
			continue
		}
		args := strAny(fn["arguments"])
		output = append(output, map[string]any{
			"id": newBridgeID("fc_"), "type": "function_call", "status": "completed",
			"call_id": firstNonEmpty(strAny(tc["id"]), newBridgeID("call_")),
			"name":    name, "arguments": args,
		})
	}
	usage, _ := chat["usage"].(map[string]any)
	return map[string]any{
		"id":                  newBridgeID("resp_"),
		"object":              "response",
		"created_at":          nowUnix(),
		"status":              "completed",
		"model":               firstNonEmpty(model, strAny(chat["model"])),
		"output":              output,
		"usage":               responsesUsage(usage),
		"parallel_tool_calls": true,
	}
}

func responsesUsage(u map[string]any) map[string]any {
	gi := func(key string) int64 {
		switch v := u[key].(type) {
		case float64:
			return int64(v)
		case int:
			return int64(v)
		}
		return 0
	}
	in, out := gi("prompt_tokens"), gi("completion_tokens")
	cached, reasoning := int64(0), int64(0)
	if d, _ := u["prompt_tokens_details"].(map[string]any); d != nil {
		switch v := d["cached_tokens"].(type) {
		case float64:
			cached = int64(v)
		}
	}
	if d, _ := u["completion_tokens_details"].(map[string]any); d != nil {
		switch v := d["reasoning_tokens"].(type) {
		case float64:
			reasoning = int64(v)
		}
	}
	return map[string]any{
		"input_tokens":          in,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         out,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          in + out,
	}
}

// ResponsesStream 把 OpenAI chat chunk 流翻译为 Responses SSE 事件。
type ResponsesStream struct {
	w       io.Writer
	model   string
	seq     int
	id      string
	started bool
	text    strings.Builder
	textID  string
	textOut int
	tools   map[int]*respTool
	order   []int
	finish  string
	usage   map[string]any
}

type respTool struct {
	index int
	id    string
	name  string
	args  strings.Builder
	out   int
}

// NewResponsesStream 构建 Responses 流翻译器。
func NewResponsesStream(w io.Writer, model string) *ResponsesStream {
	return &ResponsesStream{
		w: w, model: model, id: newBridgeID("resp_"),
		tools: map[int]*respTool{},
	}
}

// Feed 喂入上游字节。
func (s *ResponsesStream) Feed(chunk []byte) error {
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if body == "" || body == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(body), &obj) != nil {
			continue
		}
		if u, _ := obj["usage"].(map[string]any); u != nil {
			s.usage = mergeUsage(s.usage, u)
		}
		for _, raw := range asSlice(obj["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if txt, ok := delta["content"].(string); ok && txt != "" {
				if err := s.textDelta(txt); err != nil {
					return err
				}
			}
			for _, tcRaw := range asSlice(delta["tool_calls"]) {
				tc, _ := tcRaw.(map[string]any)
				ci := 0
				switch v := tc["index"].(type) {
				case float64:
					ci = int(v)
				case int:
					ci = v
				}
				slot, ok := s.tools[ci]
				if !ok {
					slot = &respTool{index: ci, id: newBridgeID("fc_"), out: len(s.order)}
					s.tools[ci] = slot
					s.order = append(s.order, ci)
				}
				fn, _ := tc["function"].(map[string]any)
				if slot.name == "" {
					slot.name = strAny(fn["name"])
					if slot.name != "" {
						if err := s.toolStart(slot); err != nil {
							return err
						}
					}
				}
				if args := strAny(fn["arguments"]); args != "" {
					slot.args.WriteString(args)
					if slot.name != "" {
						if err := s.emit("response.function_call_arguments.delta", map[string]any{
							"item_id": slot.id, "output_index": 1 + slot.out, "delta": args,
						}); err != nil {
							return err
						}
					}
				}
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				s.finish = fr
			}
		}
	}
	return nil
}

func (s *ResponsesStream) textDelta(txt string) error {
	if !s.started {
		if err := s.ensureStart(); err != nil {
			return err
		}
	}
	if s.textID == "" {
		s.textID = newBridgeID("msg_")
		s.textOut = len(s.order)
		s.order = append(s.order, -1)
		if err := s.emit("response.output_item.added", map[string]any{
			"output_index": s.textOut,
			"item": map[string]any{
				"id": s.textID, "type": "message", "role": "assistant",
				"status": "in_progress", "content": []any{},
			},
		}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.added", map[string]any{
			"item_id": s.textID, "output_index": s.textOut, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}); err != nil {
			return err
		}
	}
	s.text.WriteString(txt)
	return s.emit("response.output_text.delta", map[string]any{
		"item_id": s.textID, "output_index": s.textOut, "content_index": 0, "delta": txt,
	})
}

func (s *ResponsesStream) toolStart(slot *respTool) error {
	if err := s.ensureStart(); err != nil {
		return err
	}
	return s.emit("response.output_item.added", map[string]any{
		"output_index": 1 + slot.out,
		"item": map[string]any{
			"id": slot.id, "type": "function_call", "status": "in_progress",
			"call_id": newBridgeID("call_"), "name": slot.name, "arguments": "",
		},
	})
}

func (s *ResponsesStream) ensureStart() error {
	if s.started {
		return nil
	}
	s.started = true
	return s.emit("response.created", map[string]any{"response": map[string]any{
		"id": s.id, "object": "response", "created_at": nowUnix(),
		"status": "in_progress", "model": s.model, "output": []any{},
	}})
}

// Finish 发终态事件（幂等）。
func (s *ResponsesStream) Finish() error {
	if err := s.ensureStart(); err != nil {
		return err
	}
	output := []any{}
	if s.textID != "" {
		text := s.text.String()
		if err := s.emit("response.output_text.done", map[string]any{
			"item_id": s.textID, "output_index": s.textOut, "content_index": 0,
			"text": text,
		}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.done", map[string]any{
			"item_id": s.textID, "output_index": s.textOut, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": s.textID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
		if err := s.emit("response.output_item.done", map[string]any{
			"output_index": s.textOut, "item": item,
		}); err != nil {
			return err
		}
		output = append(output, item)
	}
	for _, ci := range s.order {
		if ci < 0 {
			continue
		}
		slot := s.tools[ci]
		if slot == nil || slot.name == "" {
			continue
		}
		args := slot.args.String()
		if err := s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": slot.id, "output_index": 1 + slot.out, "arguments": args,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": slot.id, "type": "function_call", "status": "completed",
			"call_id": newBridgeID("call_"), "name": slot.name, "arguments": args,
		}
		if err := s.emit("response.output_item.done", map[string]any{
			"output_index": 1 + slot.out, "item": item,
		}); err != nil {
			return err
		}
		output = append(output, item)
	}
	return s.emit("response.completed", map[string]any{"response": map[string]any{
		"id": s.id, "object": "response", "created_at": nowUnix(),
		"status": "completed", "model": s.model, "output": output,
		"usage": responsesUsage(s.usage),
	}})
}

func (s *ResponsesStream) emit(name string, body map[string]any) error {
	body["type"] = name
	body["sequence_number"] = s.seq
	s.seq++
	raw, _ := json.Marshal(body)
	_, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	return err
}

func nowUnix() int64 { return time.Now().Unix() }
