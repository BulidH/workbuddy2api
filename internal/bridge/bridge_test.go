package bridge

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMessagesToChat(t *testing.T) {
	payload := map[string]any{
		"model":      "DeepSeek-V4-Pro",
		"max_tokens": float64(256),
		"system":     "sys top",
		"tools": []any{map[string]any{
			"name": "get_weather", "description": "weather",
			"input_schema": map[string]any{"type": "object"},
		}},
		"tool_choice": map[string]any{"type": "any"},
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": float64(2000)},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": "AAA",
				}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tu1", "name": "get_weather",
					"input": map[string]any{"city": "SH"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu1",
					"content": "sunny"},
			}},
		},
	}
	chat := MessagesToChat(payload)
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d, want system+user+assistant+tool: %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message not system: %#v", msgs[0])
	}
	if chat["reasoning_effort"] != "low" {
		t.Fatalf("effort=%v, want low", chat["reasoning_effort"])
	}
	if chat["tool_choice"] != "required" {
		t.Fatalf("tool_choice=%v", chat["tool_choice"])
	}
	assistant := msgs[2].(map[string]any)
	if len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant tool_calls missing: %#v", assistant)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu1" || toolMsg["content"] != "sunny" {
		t.Fatalf("tool result wrong: %#v", toolMsg)
	}
}

func TestValidateAnthropic(t *testing.T) {
	ok, _ := ValidateAnthropic(map[string]any{"messages": []any{map[string]any{"role": "user"}}, "max_tokens": float64(1)})
	if !ok {
		t.Fatal("valid request rejected")
	}
	if ok, _ := ValidateAnthropic(map[string]any{"messages": []any{}}); ok {
		t.Fatal("empty messages accepted")
	}
}

func TestChatToAnthropicMessage(t *testing.T) {
	chat := map[string]any{
		"model": "m",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "hello",
				"tool_calls": []any{map[string]any{
					"id": "call1", "function": map[string]any{
						"name": "f", "arguments": `{"x":1}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(10), "completion_tokens": float64(3),
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(4)},
		},
	}
	msg := ChatToAnthropicMessage(chat, "m", "off")
	if msg["type"] != "message" || msg["stop_reason"] != "tool_use" {
		t.Fatalf("unexpected: %#v", msg)
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"].(int64) != 6 || usage["cache_read_input_tokens"].(int64) != 4 {
		t.Fatalf("usage=%#v", usage)
	}
	content := msg["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("content=%#v", content)
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	var buf bytes.Buffer
	s := NewAnthropicStream(&buf, "m", "off")
	lines := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"he"}}]}`,
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`data: [DONE]`,
	}
	for _, line := range lines {
		if err := s.Feed([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"message_start", "text_delta", "content_block_start", "input_json_delta", "message_delta", "message_stop", `"stop_reason":"tool_use"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestResponsesToChatAndBack(t *testing.T) {
	payload := map[string]any{
		"model":        "m",
		"instructions": "be nice",
		"tools": []any{map[string]any{
			"type": "function", "name": "f", "description": "d",
			"parameters": map[string]any{"type": "object"},
		}},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		},
	}
	chat := ResponsesToChat(payload)
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d: %#v", len(msgs), msgs)
	}
	if tools, _ := chat["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools=%#v", chat["tools"])
	}
	obj := ChatToResponse(map[string]any{
		"model": "m",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "hello",
				"tool_calls": []any{map[string]any{
					"id": "c2", "function": map[string]any{"name": "f", "arguments": `{"x":1}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": float64(2), "completion_tokens": float64(1)},
	}, "m")
	if obj["object"] != "response" {
		t.Fatalf("object=%v", obj["object"])
	}
	output := obj["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%#v", output)
	}
	if output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("tool output=%#v", output[1])
	}
}

func TestResponsesStreamTranslation(t *testing.T) {
	var buf bytes.Buffer
	s := NewResponsesStream(&buf, "m")
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
	} {
		if err := s.Feed([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"response.created", "response.output_text.delta", "response.function_call_arguments.done", "response.completed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestEstimateAnthropicTokens(t *testing.T) {
	n := EstimateAnthropicTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "你好世界"}},
	})
	if n < 1 {
		t.Fatalf("tokens=%d", n)
	}
	_ = json.Marshal
}
