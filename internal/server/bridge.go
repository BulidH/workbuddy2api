package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/bridge"
)

// 本文件是 Anthropic Messages / OpenAI Responses 两条协议端点的接线层：
// 请求先翻译成 Chat Completions，再复用 chatCompletions 的完整选号/轮转/冷却
// 逻辑（不复制一份路由实现），最后把 chat 结果翻译回目标协议。

const bridgeMaxBody = 32 << 20

// withBridgeAuth 同时接受 Anthropic 的 x-api-key 与 OpenAI 的 Bearer。
func (h *Handler) withBridgeAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey == "" {
			next(w, r)
			return
		}
		key := r.Header.Get("x-api-key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key != h.cfg.APIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"type": "error", "error": map[string]any{
					"type": "authentication_error", "message": "invalid API key",
				},
			})
			return
		}
		next(w, r)
	}
}

func (h *Handler) anthropicThinkingMode() string {
	if strings.EqualFold(h.cfg.AnthropicThinking, "empty") {
		return "empty"
	}
	return "off"
}

func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, bridgeMaxBody+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return nil, false
	}
	if len(body) > bridgeMaxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "message": "invalid JSON: " + err.Error(),
			},
		})
		return nil, false
	}
	return payload, true
}

// dispatchChat 用翻译后的 chat 请求调用主链路，返回录制结果。
func (h *Handler) dispatchChat(w http.ResponseWriter, r *http.Request, chat map[string]any, stream bool) *bridgeWriter {
	chat["stream"] = stream
	raw, _ := json.Marshal(chat)
	rr := r.Clone(r.Context())
	rr.Body = io.NopCloser(bytes.NewReader(raw))
	rr.ContentLength = int64(len(raw))
	rr.Header.Set("Content-Type", "application/json")
	rr.Header.Del("Content-Length")
	bw := newBridgeWriter(w)
	h.chatCompletions(bw, rr)
	return bw
}

// AnthropicMessages 处理 POST /v1/messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	payload, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	if ok, msg := bridge.ValidateAnthropic(payload); !ok {
		writeJSON(w, http.StatusBadRequest, bridge.AnthropicErrorFrame(http.StatusBadRequest, msg, "invalid_request_error"))
		return
	}
	stream := boolAny(payload["stream"])
	chat := bridge.MessagesToChat(payload)
	model := strAny(payload["model"])
	if !stream {
		bw := h.dispatchChat(w, r, chat, false)
		if bw.status >= 400 || bw.buf.Len() == 0 {
			writeJSON(w, bw.statusOr(http.StatusBadGateway),
				bridge.AnthropicErrorFrame(bw.statusOr(http.StatusBadGateway), bw.errorMessage(), ""))
			return
		}
		var obj map[string]any
		if err := json.Unmarshal(bw.buf.Bytes(), &obj); err != nil {
			writeJSON(w, http.StatusBadGateway, bridge.AnthropicErrorFrame(http.StatusBadGateway, "invalid upstream response", ""))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, bridge.ChatToAnthropicMessage(obj, model, h.anthropicThinkingMode()))
		return
	}
	conv := bridge.NewAnthropicStream(w, model, h.anthropicThinkingMode())
	bw := newBridgeWriter(w)
	bw.sink = conv.Feed
	h.chatCompletions(bw, chatRequest(r, chat, true))
	if bw.sawData {
		if err := conv.Finish(); err != nil {
			return
		}
		return
	}
	status := bw.statusOr(http.StatusBadGateway)
	writeSSEError(w, status, bridge.AnthropicErrorFrame(status, bw.errorMessage(), ""))
}

// AnthropicCountTokens 处理 POST /v1/messages/count_tokens（本地估算，不联网）。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	payload, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": bridge.EstimateAnthropicTokens(payload),
	})
}

// Responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	payload, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	stream := boolAny(payload["stream"])
	chat := bridge.ResponsesToChat(payload)
	model := strAny(payload["model"])
	if !stream {
		bw := h.dispatchChat(w, r, chat, false)
		if bw.status >= 400 || bw.buf.Len() == 0 {
			status := bw.statusOr(http.StatusBadGateway)
			writeJSON(w, status, map[string]any{
				"error": map[string]any{"message": bw.errorMessage(), "type": "api_error"},
			})
			return
		}
		var obj map[string]any
		if err := json.Unmarshal(bw.buf.Bytes(), &obj); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "invalid upstream response"}})
			return
		}
		writeJSON(w, http.StatusOK, bridge.ChatToResponse(obj, model))
		return
	}
	conv := bridge.NewResponsesStream(w, model)
	bw := newBridgeWriter(w)
	bw.sink = conv.Feed
	h.chatCompletions(bw, chatRequest(r, chat, true))
	if bw.sawData {
		_ = conv.Finish()
		return
	}
	status := bw.statusOr(http.StatusBadGateway)
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": bw.errorMessage(), "type": "api_error"},
	})
}

func chatRequest(r *http.Request, chat map[string]any, stream bool) *http.Request {
	chat["stream"] = stream
	raw, _ := json.Marshal(chat)
	rr := r.Clone(r.Context())
	rr.Body = io.NopCloser(bytes.NewReader(raw))
	rr.ContentLength = int64(len(raw))
	rr.Header.Set("Content-Type", "application/json")
	rr.Header.Del("Content-Length")
	return rr
}

// bridgeWriter 是 chatCompletions 的输出适配器：
//   - sink != nil：按行解析 SSE，喂给协议转换器（不先写客户端）；
//   - sink == nil：把响应体累积到 buf（非流式转换用）。
type bridgeWriter struct {
	real    http.ResponseWriter
	hdrs    http.Header
	status  int
	buf     bytes.Buffer
	sink    func([]byte) error
	line    bytes.Buffer
	sawData bool
	errored bool
}

func newBridgeWriter(real http.ResponseWriter) *bridgeWriter {
	return &bridgeWriter{real: real, hdrs: real.Header()}
}

func (b *bridgeWriter) Header() http.Header { return b.hdrs }

func (b *bridgeWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bridgeWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.sink == nil {
		b.buf.Write(p)
		return len(p), nil
	}
	b.line.Write(p)
	for {
		raw := b.line.Bytes()
		idx := bytes.IndexByte(raw, '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), raw[:idx+1]...)
		b.line.Next(idx + 1)
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			b.sawData = true
			_ = b.sink(line)
			continue
		}
		// 非 SSE 行（通常是网关的统一错误 JSON）：留给非流式错误出口。
		b.errored = true
		b.buf.Write(line)
	}
	return len(p), nil
}

func (b *bridgeWriter) Flush() {
	if f, ok := b.real.(http.Flusher); ok {
		f.Flush()
	}
}

func (b *bridgeWriter) statusOr(def int) int {
	if b.status != 0 {
		return b.status
	}
	return def
}

func (b *bridgeWriter) errorMessage() string {
	raw := b.buf.String()
	if raw == "" {
		return "upstream request failed"
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) == nil {
		if e, _ := obj["error"].(map[string]any); e != nil {
			if msg, _ := e["message"].(string); msg != "" {
				return msg
			}
		}
		if msg, _ := obj["message"].(string); msg != "" {
			return msg
		}
	}
	if len(raw) > 500 {
		raw = raw[:500]
	}
	return raw
}

func writeSSEError(w http.ResponseWriter, status int, frame map[string]any) {
	raw, _ := json.Marshal(frame)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", raw)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func boolAny(v any) bool {
	b, _ := v.(bool)
	return b
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}
