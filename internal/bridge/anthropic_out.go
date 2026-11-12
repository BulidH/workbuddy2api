package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// AnthropicUsage 从 chat usage 派生 Anthropic usage（input 不含 cache_read）。
func AnthropicUsage(usage map[string]any) map[string]any {
	get := func(key string) int64 {
		switch v := usage[key].(type) {
		case float64:
			return int64(v)
		case int:
			return int64(v)
		}
		return 0
	}
	prompt := get("prompt_tokens")
	cached := get("cached_tokens")
	if cached == 0 {
		cached = get("prompt_cache_hit_tokens")
	}
	if pdet, _ := usage["prompt_tokens_details"].(map[string]any); pdet != nil && cached == 0 {
		switch v := pdet["cached_tokens"].(type) {
		case float64:
			cached = int64(v)
		}
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	reasoning := int64(0)
	if det, _ := usage["completion_tokens_details"].(map[string]any); det != nil {
		switch v := det["reasoning_tokens"].(type) {
		case float64:
			reasoning = int64(v)
		}
	}
	return map[string]any{
		"input_tokens":                prompt - cached,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
		"output_tokens":               get("completion_tokens"),
		"output_tokens_details":       map[string]any{"reasoning_tokens": reasoning},
	}
}

var anthropicStop = map[string]string{
	"stop": "end_turn", "length": "max_tokens",
	"tool_calls": "tool_use", "function_call": "tool_use",
}

func anthropicStopReason(finish string) string {
	if v, ok := anthropicStop[finish]; ok {
		return v
	}
	return "end_turn"
}

func newBridgeID(prefix string) string {
	return prefix + randomHex(24)
}

// ChatToAnthropicMessage 把 chat.completion 对象翻译成 Anthropic message。
func ChatToAnthropicMessage(chat map[string]any, model, thinkingMode string) map[string]any {
	choices, _ := chat["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	msg, _ := choice["message"].(map[string]any)
	content := []any{}
	if s, ok := msg["content"].(string); ok && s != "" {
		content = append(content, map[string]any{"type": "text", "text": s})
	} else if parts, ok := msg["content"].([]any); ok {
		var b strings.Builder
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		if b.Len() > 0 {
			content = append(content, map[string]any{"type": "text", "text": b.String()})
		}
	}
	if thinkingMode == "empty" {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			content = append(content, map[string]any{
				"type": "thinking", "thinking": rc, "signature": "",
			})
		}
	}
	for _, raw := range asSlice(msg["tool_calls"]) {
		tc, _ := raw.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		var input any = map[string]any{}
		if args, ok := fn["arguments"].(string); ok && strings.TrimSpace(args) != "" {
			if json.Unmarshal([]byte(args), &input) != nil {
				input = map[string]any{}
			}
		}
		if _, ok := input.(map[string]any); !ok {
			input = map[string]any{"value": input}
		}
		id, _ := tc["id"].(string)
		if id == "" {
			id = newBridgeID("toolu_")
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": input,
		})
	}
	usage, _ := chat["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}
	finish, _ := choice["finish_reason"].(string)
	return map[string]any{
		"id":            newBridgeID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         firstNonEmpty(model, strAny(chat["model"])),
		"content":       content,
		"stop_reason":   anthropicStopReason(finish),
		"stop_sequence": nil,
		"usage":         AnthropicUsage(usage),
	}
}

// AnthropicErrorFrame 生成 Anthropic 流/非流的错误体。
func AnthropicErrorFrame(status int, message, kind string) map[string]any {
	if kind == "" {
		kind = "api_error"
		switch status {
		case 400:
			kind = "invalid_request_error"
		case 401:
			kind = "authentication_error"
		case 429:
			kind = "rate_limit_error"
		}
	}
	return map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}}
}

// AnthropicStream 把 OpenAI chat chunk 流增量翻译为 Anthropic SSE 事件。
type AnthropicStream struct {
	w            io.Writer
	model        string
	thinkingMode string

	started   bool
	nextIndex int
	textIndex int
	thinkIdx  int
	toolIndex map[int]int
	pending   map[int]*pendingTool
	open      map[int]bool
	finish    string
	usage     map[string]any
}

type pendingTool struct {
	index   int
	id      string
	name    string
	args    []string
	isObj   bool
	started bool
}

// NewAnthropicStream 构建流翻译器。
func NewAnthropicStream(w io.Writer, model, thinkingMode string) *AnthropicStream {
	return &AnthropicStream{
		w: w, model: model, thinkingMode: thinkingMode,
		textIndex: -1, thinkIdx: -1,
		toolIndex: map[int]int{}, pending: map[int]*pendingTool{}, open: map[int]bool{},
		finish: "stop", usage: map[string]any{},
	}
}

// Feed 喂入一段上游字节（可任意切分）。
func (s *AnthropicStream) Feed(chunk []byte) error {
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			if _, err := fmt.Fprintf(s.w, "%s\n\n", line); err != nil {
				return err
			}
			continue
		}
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
		if err := s.handle(obj); err != nil {
			return err
		}
	}
	return nil
}

func (s *AnthropicStream) handle(obj map[string]any) error {
	if u, _ := obj["usage"].(map[string]any); u != nil {
		s.usage = mergeUsage(s.usage, u)
	}
	for _, raw := range asSlice(obj["choices"]) {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if piece, ok := delta["reasoning_content"].(string); ok && piece != "" && s.thinkingMode == "empty" {
			if err := s.ensureStart(); err != nil {
				return err
			}
			if s.thinkIdx < 0 {
				s.thinkIdx = s.nextIndex
				s.nextIndex++
				s.open[s.thinkIdx] = true
				if err := s.event("content_block_start", map[string]any{
					"index": s.thinkIdx,
					"content_block": map[string]any{
						"type": "thinking", "thinking": "", "signature": "",
					},
				}); err != nil {
					return err
				}
			}
			if err := s.event("content_block_delta", map[string]any{
				"index": s.thinkIdx, "delta": map[string]any{
					"type": "thinking_delta", "thinking": piece,
				},
			}); err != nil {
				return err
			}
		}
		if txt, ok := delta["content"].(string); ok && txt != "" {
			if err := s.ensureStart(); err != nil {
				return err
			}
			if s.textIndex < 0 {
				s.textIndex = s.nextIndex
				s.nextIndex++
				s.open[s.textIndex] = true
				if err := s.event("content_block_start", map[string]any{
					"index":         s.textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}); err != nil {
					return err
				}
			}
			if err := s.event("content_block_delta", map[string]any{
				"index": s.textIndex,
				"delta": map[string]any{"type": "text_delta", "text": txt},
			}); err != nil {
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
			default:
				ci = len(s.toolIndex)
			}
			fn, _ := tc["function"].(map[string]any)
			slot, ok := s.pending[ci]
			if !ok {
				slot = &pendingTool{index: s.nextIndex}
				s.nextIndex++
				s.toolIndex[ci] = slot.index
				s.pending[ci] = slot
			}
			if slot.name == "" {
				slot.name = strAny(fn["name"])
			}
			if slot.id == "" {
				slot.id = strAny(tc["id"])
			}
			args := strAny(fn["arguments"])
			if args != "" {
				slot.args = append(slot.args, args)
				if strings.TrimSpace(args) != "" && !slot.isObj {
					slot.isObj = strings.HasPrefix(strings.TrimSpace(args), "{")
				}
			}
			if slot.name != "" && !slot.started {
				if err := s.ensureStart(); err != nil {
					return err
				}
				slot.started = true
				s.open[slot.index] = true
				id := slot.id
				if id == "" {
					id = newBridgeID("toolu_")
				}
				if err := s.event("content_block_start", map[string]any{
					"index": slot.index,
					"content_block": map[string]any{
						"type": "tool_use", "id": id, "name": slot.name, "input": map[string]any{},
					},
				}); err != nil {
					return err
				}
				if slot.isObj {
					queued := slot.args
					slot.args = nil
					for _, piece := range queued {
						if err := s.inputDelta(slot.index, piece); err != nil {
							return err
						}
					}
				}
			}
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			s.finish = fr
		}
	}
	return nil
}

func (s *AnthropicStream) inputDelta(index int, piece string) error {
	return s.event("content_block_delta", map[string]any{
		"index": index, "delta": map[string]any{
			"type": "input_json_delta", "partial_json": piece,
		},
	})
}

func (s *AnthropicStream) ensureStart() error {
	if s.started {
		return nil
	}
	s.started = true
	u := AnthropicUsage(s.usage)
	return s.event("message_start", map[string]any{"message": map[string]any{
		"id": newBridgeID("msg_"), "type": "message", "role": "assistant",
		"model": s.model, "content": []any{}, "stop_reason": nil,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                u["input_tokens"],
			"cache_read_input_tokens":     u["cache_read_input_tokens"],
			"cache_creation_input_tokens": 0,
			"output_tokens":               0,
		},
	}})
}

// Finish 关闭所有块并发出终态事件（幂等）。
func (s *AnthropicStream) Finish() error {
	if !s.started {
		// 没有任何内容：也返回一个合法的空 message，避免客户端挂死。
		if err := s.ensureStart(); err != nil {
			return err
		}
	}
	for ci, slot := range s.pending {
		_ = ci
		if !slot.started {
			continue // 无 name 的坏块：丢弃（不发出空 name 的 start）
		}
		if len(slot.args) > 0 {
			joined := strings.Join(slot.args, "")
			if slot.isObj {
				for _, piece := range slot.args {
					if err := s.inputDelta(slot.index, piece); err != nil {
						return err
					}
				}
			} else {
				var decoded any
				if json.Unmarshal([]byte(joined), &decoded) == nil {
					if _, ok := decoded.(map[string]any); !ok {
						decoded = map[string]any{"value": decoded}
					}
					raw, _ := json.Marshal(decoded)
					if err := s.inputDelta(slot.index, string(raw)); err != nil {
						return err
					}
				}
			}
			slot.args = nil
		}
	}
	idxs := make([]int, 0, len(s.open))
	for i := range s.open {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		if err := s.event("content_block_stop", map[string]any{"index": i}); err != nil {
			return err
		}
	}
	final := AnthropicUsage(s.usage)
	if err := s.event("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason": anthropicStopReason(s.finish), "stop_sequence": nil,
		},
		"usage": final,
	}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{})
}

func (s *AnthropicStream) event(name string, body map[string]any) error {
	body["type"] = name
	raw, _ := json.Marshal(body)
	_, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	return err
}

func mergeUsage(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if n, ok := v.(float64); ok {
			if prev, ok := dst[k].(float64); !ok || n > prev {
				dst[k] = n
			}
			continue
		}
		if _, ok := dst[k]; !ok {
			dst[k] = v
		}
	}
	return dst
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}
