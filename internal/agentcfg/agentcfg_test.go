package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYAMLUpsertIntoExistingBlock(t *testing.T) {
	in := "# 用户注释\nllm-pi-ai:\n  providers:\n    other:\n      apiKeyEnv: OTHER\n  timeout: 30\nagent-default-model:\n  provider: other\n"
	out, err := YAMLUpsertInline(in, "llm-pi-ai.providers.qoder-proxy",
		map[string]any{"displayName": "Qoder Gateway", "baseURL": "http://x/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# 用户注释") || !strings.Contains(out, "apiKeyEnv: OTHER") {
		t.Fatalf("existing content lost:\n%s", out)
	}
	if !strings.Contains(out, "qoder-proxy:") || !strings.Contains(out, `"baseURL":"http://x/v1"`) {
		t.Fatalf("new provider missing:\n%s", out)
	}
}

func TestYAMLUpsertCreatesNestedPath(t *testing.T) {
	out, err := YAMLUpsertInline("", "llm-pi-ai.providers.qoder-proxy",
		map[string]any{"displayName": "Q"})
	if err != nil {
		t.Fatal(err)
	}
	want := "llm-pi-ai:\n  providers:\n    qoder-proxy: {\"displayName\":\"Q\"}"
	if !strings.Contains(out, want) {
		t.Fatalf("got:\n%s", out)
	}
}

func TestYAMLRefsKeepsCommentAndQuotes(t *testing.T) {
	in := "# creds\nrefs:\n  OTHER: \"abc\"\n"
	out, err := YAMLRefsUpsert(in, credential, "12345")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# creds") || !strings.Contains(out, `OTHER: "abc"`) {
		t.Fatalf("existing content lost:\n%s", out)
	}
	// 纯数字 Key 必须被写成带引号字符串，否则 DSH 无法启动。
	if !strings.Contains(out, `QODER_PROXY_API_KEY: "12345"`) || !RefsValueIsQuotedScalar(out, credential) {
		t.Fatalf("unquoted scalar:\n%s", out)
	}
}

func TestTOMLUpsertPreservesTables(t *testing.T) {
	in := "model = \"old\"\n# keep\n\n[other]\nkey = 1\n"
	out := TOMLUpsertTopKey(in, "model_provider", providerID)
	out = TOMLUpsertTopKey(out, "model", "DeepSeek-V4-Pro")
	out = TOMLUpsertTable(out, "model_providers."+providerID, map[string]any{
		"name": providerName, "base_url": "http://x/v1",
	})
	if !strings.Contains(out, "model_provider = \"qoder-proxy\"") || !strings.Contains(out, "model = \"DeepSeek-V4-Pro\"") {
		t.Fatalf("top keys:\n%s", out)
	}
	if !strings.Contains(out, "[other]") || !strings.Contains(out, "key = 1") {
		t.Fatalf("existing table lost:\n%s", out)
	}
	if !strings.Contains(out, "[model_providers.qoder-proxy]") || !strings.Contains(out, `base_url = "http://x/v1"`) {
		t.Fatalf("provider table:\n%s", out)
	}
}

func TestManagedBlockReplace(t *testing.T) {
	in := "a: 1\n# qoder-proxy:vibe-provider:begin\nold\n# qoder-proxy:vibe-provider:end\nb: 2\n"
	out, err := ManagedBlock(in, "vibe-provider", "new")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "old") || !strings.Contains(out, "new") || !strings.Contains(out, "a: 1") || !strings.Contains(out, "b: 2") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestIntegrateAndRestoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	svc := New(stateDir, home)

	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "{\n  \"env\": {\"FOO\": \"bar\"},\n  \"model\": \"keep-me\"\n}\n"
	if err := os.WriteFile(settings, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := svc.Plan("claude-code", "http://127.0.0.1:8787/v1/", "sk-test-key", "qoder:DeepSeek-V4-Pro", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || !plan.Files[0].Existed {
		t.Fatalf("plan=%#v", plan)
	}
	if !strings.Contains(plan.Files[0].Content, `"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"`) {
		t.Fatalf("base url not normalized: %s", plan.Files[0].Content)
	}

	res, err := svc.Integrate("claude-code", "http://127.0.0.1:8787", "sk-test-key", "DeepSeek-V4-Pro", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Backup == "" {
		t.Fatalf("result=%#v", res)
	}
	written, _ := os.ReadFile(settings)
	for _, want := range []string{"FOO", "keep-me", "ANTHROPIC_AUTH_TOKEN", "DeepSeek-V4-Pro"} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("%q missing:\n%s", want, written)
		}
	}
	// state 不存明文 Key。
	stateRaw, _ := os.ReadFile(filepath.Join(stateDir, "agent-state.json"))
	if strings.Contains(string(stateRaw), "sk-test-key") {
		t.Fatalf("plaintext key persisted: %s", stateRaw)
	}

	overview := svc.Overview()
	found := false
	for _, c := range overview["clients"].([]OverviewClient) {
		if c.ID == "claude-code" && c.Configured {
			found = true
		}
	}
	if !found {
		t.Fatal("overview did not mark claude-code configured")
	}

	if _, err := svc.Restore("claude-code"); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(settings)
	if string(restored) != original {
		t.Fatalf("restore not byte-exact:\nwant %q\ngot  %q", original, restored)
	}
}

func TestIntegrateCreatesAndRestoreDeletes(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	svc := New(stateDir, home)
	if _, err := svc.Integrate("crush", "http://127.0.0.1:8787", "sk", "", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "crush", "crush.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not created: %v", err)
	}
	res, err := svc.Restore("crush")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restored) != 1 || res.Restored[0].Action != "deleted" {
		t.Fatalf("restored=%#v", res.Restored)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config still exists: %v", err)
	}
	// 我们建的空目录也应清掉。
	if _, err := os.Stat(filepath.Join(home, ".config", "crush")); !os.IsNotExist(err) {
		t.Fatalf("empty dir left behind: %v", err)
	}
}

func TestManualClient(t *testing.T) {
	svc := New(t.TempDir(), t.TempDir())
	plan, err := svc.Plan("cursor", "http://127.0.0.1:8787", "sk", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "manual" || len(plan.Files) != 0 {
		t.Fatalf("plan=%#v", plan)
	}
	if !strings.Contains(plan.Guide, "http://127.0.0.1:8787/v1") {
		t.Fatalf("guide=%s", plan.Guide)
	}
	if _, err := svc.Integrate("cursor", "http://127.0.0.1:8787", "sk", "", nil); err == nil {
		t.Fatal("manual client integrate succeeded")
	}
}

func TestTemplateUsesPlaceholder(t *testing.T) {
	svc := New(t.TempDir(), t.TempDir())
	plan, err := svc.Template("codex", "http://127.0.0.1:8787", "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) == 0 {
		t.Fatal("template has no files")
	}
	placeholder := false
	for _, f := range plan.Files {
		if strings.Contains(f.Content, "YOUR_API_KEY") {
			placeholder = true
		}
		if strings.Contains(f.Path, string(filepath.Separator)+".codex"+string(filepath.Separator)) {
			t.Fatalf("template touched real home: %s", f.Path)
		}
	}
	if !placeholder {
		t.Fatalf("template has no placeholder key: %#v", plan.Files)
	}
}

func TestBaseURLValidation(t *testing.T) {
	svc := New(t.TempDir(), t.TempDir())
	if _, err := svc.Plan("crush", "ftp://x", "k", "", nil); err == nil {
		t.Fatal("ftp accepted")
	}
	if _, err := svc.Plan("nope", "http://x", "k", "", nil); err == nil {
		t.Fatal("unknown client accepted")
	}
}
