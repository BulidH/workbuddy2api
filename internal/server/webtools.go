package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/qoder"
)

// tryQoderWebTools 在 Qoder 强路由请求声明了内置 web_search / web_fetch 时，
// 把工具改写成网关自己的 function，并由网关本地执行、把结果回灌模型。
// 返回 true 表示已完整处理（调用方直接 return）。
//
// 仅在 qoder: 强路由下启用：裸名请求可能落到 WorkBuddy，改写后的 web 工具
// 在那边没有执行方，会让客户端拿到一个永远不会完成的 tool_call。
func (h *Handler) tryQoderWebTools(w http.ResponseWriter, r *http.Request, body []byte, model string, stream bool) bool {
	if !h.cfg.QoderWebTools || h.cfg.Qoder == nil {
		return false
	}
	provider, _, _ := resolveRoute(model)
	if provider != auth.ProviderQoder {
		return false
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return false
	}
	tools, _ := chat["tools"].([]any)
	names := qoder.WebToolNames(tools)
	if len(names) == 0 {
		return false
	}
	chat["tools"] = qoder.RewriteWebTools(tools, names)
	messages, _ := chat["messages"].([]any)

	var final map[string]any
	for round := 0; round < qoder.WebToolMaxRounds; round++ {
		chat["messages"] = messages
		chat["stream"] = false
		bw := h.dispatchChat(w, r, chat, false)
		if bw.status >= 400 || bw.buf.Len() == 0 {
			writeOpenAIError(w, bw.statusOr(http.StatusBadGateway), "upstream_error", bw.errorMessage())
			return true
		}
		var obj map[string]any
		if json.Unmarshal(bw.buf.Bytes(), &obj) != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "invalid upstream response")
			return true
		}
		final = obj
		msg := chatMessage(obj)
		calls := webCalls(msg, names)
		if len(calls) == 0 {
			break
		}
		// 模型只要还混着非 web 的普通工具调用，就必须把整轮结果交还给客户端，
		// 否则我们只回灌 web 结果、留下没有 tool 消息的悬空 tool_call，上游会 400。
		if all, _ := msg["tool_calls"].([]any); len(all) != len(calls) {
			break
		}
		assistant := map[string]any{"role": "assistant"}
		if c, ok := msg["content"]; ok && c != nil {
			assistant["content"] = c
		}
		// 部分上游要求在后续轮次回传 reasoning_content；有就带上。
		if rc, ok := msg["reasoning_content"]; ok && rc != nil {
			assistant["reasoning_content"] = rc
		}
		if tc, ok := msg["tool_calls"]; ok {
			assistant["tool_calls"] = tc
		}
		messages = append(messages, assistant)
		for _, call := range calls {
			id, _ := call["id"].(string)
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			result := qoder.RunWebTool(r.Context(), name, args)
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": id, "content": result,
			})
		}
	}
	if final == nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "no response")
		return true
	}
	if !stream {
		writeJSON(w, http.StatusOK, final)
		return true
	}
	writeChatAsSSE(w, final)
	return true
}

func chatMessage(obj map[string]any) map[string]any {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	return msg
}

func webCalls(msg map[string]any, names map[string]bool) []map[string]any {
	out := []map[string]any{}
	calls, _ := msg["tool_calls"].([]any)
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if names[name] {
			out = append(out, call)
		}
	}
	return out
}

// writeChatAsSSE 把一个聚合 chat 对象摊成标准 chat chunk 流回放。
func writeChatAsSSE(w http.ResponseWriter, obj map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	id, _ := obj["id"].(string)
	if id == "" {
		id = "chatcmpl-webtools"
	}
	model, _ := obj["model"].(string)
	created := obj["created"]
	msg := chatMessage(obj)
	content, _ := msg["content"].(string)
	finish := ""
	if choices, _ := obj["choices"].([]any); len(choices) > 0 {
		if c, _ := choices[0].(map[string]any); c != nil {
			finish, _ = c["finish_reason"].(string)
		}
	}
	emit := func(delta map[string]any, finishReason any) {
		frame := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finishReason,
			}},
		}
		raw, _ := json.Marshal(frame)
		_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	}
	emit(map[string]any{"role": "assistant"}, nil)
	if content != "" {
		emit(map[string]any{"content": content}, nil)
	}
	tail := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": firstNonEmptyString(finish, "stop"),
		}},
	}
	if usage, ok := obj["usage"]; ok {
		tail["usage"] = usage
	}
	raw, _ := json.Marshal(tail)
	_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
