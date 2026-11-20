package panel

import "testing"

// TestDeepMergeNullDeletesKey patch 里的 null 必须真正删除键，而不是写成 null。
//
// 回归背景：混合路由页「删除混合模型」用 {aliases:{name:null}} 表达删除。
// 旧实现把 null 原样写进配置 → 配置里留下 `"name": null` → 反序列化后该别名
// 仍然存在于 map 中，页面刷新后又出现，用户看到「删不掉」。
func TestDeepMergeNullDeletesKey(t *testing.T) {
	base := map[string]any{
		"aliases": map[string]any{
			"keep":   map[string]any{"loomy": "A"},
			"remove": map[string]any{"loomy": "B"},
		},
	}
	patch := map[string]any{
		"aliases": map[string]any{
			"remove": nil,
		},
	}
	out := deepMerge(base, patch)
	aliases, ok := out["aliases"].(map[string]any)
	if !ok {
		t.Fatalf("aliases 不是 map: %#v", out["aliases"])
	}
	if _, exists := aliases["remove"]; exists {
		t.Fatalf("null 未删除键，aliases=%#v", aliases)
	}
	if _, exists := aliases["keep"]; !exists {
		t.Fatalf("未涉及的键不应被删除，aliases=%#v", aliases)
	}
}

// TestDeepMergeNullTopLevel 顶层 null 同样删除键。
func TestDeepMergeNullTopLevel(t *testing.T) {
	out := deepMerge(map[string]any{"a": 1, "b": 2}, map[string]any{"b": nil})
	if _, ok := out["b"]; ok {
		t.Fatalf("顶层 null 未删除键: %#v", out)
	}
	if out["a"] != 1 {
		t.Fatalf("其他键被破坏: %#v", out)
	}
}

// TestDeepMergePatchWins 非 null 的 patch 值仍整体覆盖（数组/标量语义不变）。
func TestDeepMergePatchWins(t *testing.T) {
	out := deepMerge(
		map[string]any{"hours": []any{9, 21}, "n": 1},
		map[string]any{"hours": []any{10}, "n": 2},
	)
	if got := out["hours"].([]any); len(got) != 1 || got[0] != 10 {
		t.Fatalf("数组应整体覆盖，得 %#v", got)
	}
	if out["n"] != 2 {
		t.Fatalf("标量应覆盖，得 %#v", out["n"])
	}
}

// TestDeepMergeNestedObject 两边都是 object 时递归合并。
func TestDeepMergeNestedObject(t *testing.T) {
	out := deepMerge(
		map[string]any{"a": map[string]any{"x": 1, "y": 2}},
		map[string]any{"a": map[string]any{"y": 9, "z": 3}},
	)
	a := out["a"].(map[string]any)
	if a["x"] != 1 || a["y"] != 9 || a["z"] != 3 {
		t.Fatalf("递归合并结果错误: %#v", a)
	}
}
