package qoder

import (
	"encoding/json"
	"testing"
)

func decodeRequest(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return m
}

func TestBuildRequestFlattensMessages(t *testing.T) {
	req := decodeRequest(t, `{
		"model": "deepseek-v4-pro",
		"max_tokens": 1234,
		"reasoning_effort": "high",
		"messages": [
			{"role":"system","content":"sys-a"},
			{"role":"developer","content":"sys-b"},
			{"role":"user","content":"你好"},
			{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","function":{"name":"f","arguments":{"x":1}}}]},
			{"role":"tool","tool_call_id":"c1","content":"result"},
			{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
		],
		"tools": [{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]
	}`)

	body, err := BuildRequest(req, QoderAccount{UID: "u1"}, ModelInfo{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", MaxInputToken: 64000}, "cn")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if body["stream"] != true || body["agent_id"] != "agent_common" {
		t.Errorf("stream/agent_id = %v/%v", body["stream"], body["agent_id"])
	}
	mc, _ := body["model_config"].(map[string]any)
	if mc["key"] != "dmodel" || mc["display_name"] != "DeepSeek-V4-Pro" || mc["format"] != "openai" {
		t.Errorf("model_config = %v", mc)
	}
	if mc["max_input_tokens"].(int) != 64000 {
		t.Errorf("max_input_tokens = %v", mc["max_input_tokens"])
	}
	params, _ := body["parameters"].(map[string]any)
	if params["max_tokens"] != 1234 || params["reasoning_effort"] != "high" {
		t.Errorf("parameters = %v", params)
	}

	msgs, _ := body["messages"].([]any)
	// 第一条 system 取 sys-a；user → assistant(tool_calls) → tool → 带图 user 依次直传。
	if len(msgs) != 5 {
		t.Fatalf("messages len = %d (%v)", len(msgs), msgs)
	}
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "sys-a" {
		t.Errorf("system = %v", m0)
	}
	if m1 := msgs[1].(map[string]any); m1["role"] != "user" || m1["content"] != "你好" {
		t.Errorf("first user = %v", m1)
	}
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("assistant = %v", asst)
	}
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", asst["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"x":1}` {
		t.Errorf("tool arguments = %v", fn["arguments"])
	}
	tool := msgs[3].(map[string]any)
	if tool["tool_call_id"] != "c1" || tool["content"] != "result" {
		t.Errorf("tool msg = %v", tool)
	}

	cc, _ := body["chat_context"].(map[string]any)
	txt, _ := cc["text"].(map[string]any)
	if txt["text"] != "看图" {
		t.Errorf("chat_context.text = %v", txt["text"])
	}
	biz, _ := body["business"].(map[string]any)
	if biz["name"] != "看图" || biz["model_key"] != "dmodel" {
		t.Errorf("business = %v", biz)
	}
}

func TestBuildRequestDefaults(t *testing.T) {
	req := decodeRequest(t, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	body, err := BuildRequest(req, QoderAccount{}, ModelInfo{Key: "k"}, "")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if body["aliyun_user_type"] != defaultUserType {
		t.Errorf("aliyun_user_type = %v", body["aliyun_user_type"])
	}
	tools, _ := body["tools"].([]any)
	if tools == nil {
		t.Errorf("tools should be empty slice, got %v", body["tools"])
	}
}

func TestFlattenContentImages(t *testing.T) {
	text, parts, hasImage := flattenContent([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}},
	})
	if text != "a" || !hasImage || len(parts) != 2 {
		t.Fatalf("flattenContent = %q, %v, %v", text, parts, hasImage)
	}
	// parts 需同时保留文字与图片（顺序不变），否则多模态消息会丢文字。
	if p0 := parts[0].(map[string]any); p0["type"] != "text" || p0["text"] != "a" {
		t.Errorf("parts[0] = %v", p0)
	}
	if p1 := parts[1].(map[string]any); p1["type"] != "image_url" {
		t.Errorf("parts[1] = %v", p1)
	}
}
