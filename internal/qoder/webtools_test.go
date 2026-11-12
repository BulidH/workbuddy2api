package qoder

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebToolNamesAndRewrite(t *testing.T) {
	tools := []any{
		map[string]any{"type": "web_search_preview"},
		map[string]any{"type": "web_fetch"},
		map[string]any{"type": "function", "function": map[string]any{"name": "f"}},
	}
	names := WebToolNames(tools)
	if !names[WebSearchName] || !names[WebFetchName] {
		t.Fatalf("names=%v", names)
	}
	out := RewriteWebTools(tools, names)
	if len(out) != 3 {
		t.Fatalf("rewritten len=%d, want 3 (1 custom + 2 web)", len(out))
	}
	got := map[string]bool{}
	for _, raw := range out {
		m := raw.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		got[name] = true
	}
	if !got["f"] || !got[WebSearchName] || !got[WebFetchName] {
		t.Fatalf("rewritten=%v", got)
	}
	// 原声明里的内置工具不能残留。
	for _, raw := range out {
		kind, _ := raw.(map[string]any)["type"].(string)
		if kind == "web_search_preview" || kind == "web_fetch" {
			t.Fatalf("builtin declaration leaked: %#v", raw)
		}
	}
}

func TestRunWebToolErrorsAsJSON(t *testing.T) {
	for _, name := range []string{"nope", WebFetchName} {
		args := `{}`
		got := RunWebTool(context.Background(), name, args)
		var obj map[string]any
		if err := json.Unmarshal([]byte(got), &obj); err != nil {
			t.Fatalf("%s: not json: %s", name, got)
		}
		if _, ok := obj["error"]; !ok {
			t.Fatalf("%s: no error field: %s", name, got)
		}
	}
	if got := RunWebTool(context.Background(), WebSearchName, `{}`); !strings.Contains(got, "query is required") {
		t.Fatalf("empty query: %s", got)
	}
}

func TestIsPublicIP(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	private := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1"}
	for _, s := range public {
		if !IsPublicIP(net.ParseIP(s)) {
			t.Fatalf("%s should be public", s)
		}
	}
	for _, s := range private {
		if IsPublicIP(net.ParseIP(s)) {
			t.Fatalf("%s should be blocked", s)
		}
	}
}

func TestValidateURL(t *testing.T) {
	bad := []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"https://user:pass@example.com/",
		"",
	}
	for _, raw := range bad {
		if _, err := ValidateURL(raw); err == nil {
			t.Fatalf("%q accepted", raw)
		}
	}
	if u, err := ValidateURL("https://example.com/a?b=1"); err != nil || u.Host != "example.com" {
		t.Fatalf("valid url rejected: %v %v", u, err)
	}
}

// 本地地址必须在拨号阶段被挡住，而不是先连上再判断。
func TestWebFetchBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>secret</body></html>"))
	}))
	defer srv.Close()
	if _, err := WebFetch(context.Background(), srv.URL); err == nil {
		t.Fatal("loopback fetch was not blocked")
	}
}

func TestCleanHTML(t *testing.T) {
	in := `<html><head><script>alert("x")</script><style>p{}</style></head>` +
		`<body><h1>标题</h1><p>正文 &amp; more</p></body></html>`
	out := cleanHTML(in)
	if strings.Contains(out, "alert") || strings.Contains(out, "p{}") {
		t.Fatalf("script/style leaked: %q", out)
	}
	if !strings.Contains(out, "正文") || !strings.Contains(out, "&") {
		t.Fatalf("text lost: %q", out)
	}
}

func TestDecodeDDGLink(t *testing.T) {
	wrapped := "//duckduckgo.com/l/?uddg=" +
		"https%3A%2F%2Fexample.com%2Fp%3Fa%3D1&rut=zz"
	if got := decodeDDGLink(wrapped); got != "https://example.com/p?a=1" {
		t.Fatalf("got %q", got)
	}
	if got := decodeDDGLink("https://example.com/direct"); got != "https://example.com/direct" {
		t.Fatalf("direct got %q", got)
	}
}
