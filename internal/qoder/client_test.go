package qoder

import (
	"io"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return string(b)
}

// Qoder 上游把 OpenAI chunk 包在 {"body":"...","statusCodeValue":200} 信封里；
// 解包后应还原成标准 data: 行并以 [DONE] 结束。
func TestUnwrapSSEEnvelope(t *testing.T) {
	c := New(Config{Timeout: time.Second})
	in := strings.Join([]string{
		": keep-alive",
		`data: {"headers":{},"body":"{\"choices\":[{\"delta\":{\"content\":\"你\"}}]}","statusCodeValue":200}`,
		"",
		`data: {"headers":{},"body":"{\"choices\":[{\"delta\":{\"content\":\"好\"}}]}","statusCodeValue":200}`,
		"",
		`data: {"headers":{},"body":"[DONE]","statusCodeValue":200}`,
	}, "\n")

	rc, status, detail := c.unwrapSSE(io.NopCloser(strings.NewReader(in)), "")
	if rc == nil {
		t.Fatalf("unwrapSSE returned error: %d %s", status, detail)
	}
	out := readAll(t, rc)
	if !strings.Contains(out, `"content":"你"`) || !strings.Contains(out, `"content":"好"`) {
		t.Errorf("missing content chunks: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Errorf("unexpected non-data line %q in %s", line, out)
		}
		if strings.Contains(line, "statusCodeValue") {
			t.Errorf("envelope not unwrapped: %q", line)
		}
	}
}

func TestUnwrapSSEFirstFrameError(t *testing.T) {
	c := New(Config{Timeout: time.Second})
	in := `data: {"headers":{},"body":"{\"error\":\"bad token\"}","statusCodeValue":401}` + "\n"
	rc, status, detail := c.unwrapSSE(io.NopCloser(strings.NewReader(in)), "")
	if rc != nil {
		t.Fatalf("expected error, got stream")
	}
	if status != 401 || !strings.Contains(string(detail), "bad token") {
		t.Errorf("status/detail = %d %s", status, detail)
	}
}

func TestUnwrapSSEWithoutFrames(t *testing.T) {
	c := New(Config{Timeout: time.Second})
	rc, status, detail := c.unwrapSSE(io.NopCloser(strings.NewReader("\n\n: ping\n")), "")
	if rc != nil {
		t.Fatalf("expected error, got stream")
	}
	if status != 502 {
		t.Errorf("status = %d, want 502", status)
	}
	if !strings.Contains(string(detail), "without any SSE frame") {
		t.Errorf("detail = %s", detail)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   upstream.ErrKind
	}{
		{401, "unauthorized", upstream.ErrSessionDead},
		{403, "forbidden", upstream.ErrSessionDead},
		{402, "payment required", upstream.ErrHardCredit},
		{400, "insufficient credit balance", upstream.ErrHardCredit},
		{429, "too many requests", upstream.ErrSoftRate},
		{400, `{"code":"6004"}`, upstream.ErrSoftRate},
		{404, "not found", upstream.ErrNotFound},
		{503, "unavailable", upstream.ErrServer},
		{400, "invalid model", upstream.ErrBadParams},
		{422, "weird", upstream.ErrClient},
		{200, "", upstream.ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestRewriteFrameModel(t *testing.T) {
	in := `{"id":"x","object":"chat.completion.chunk","model":"auto","choices":[]}`
	got := rewriteFrameModel(in, "DeepSeek-Flash")
	if !strings.Contains(got, `"model":"DeepSeek-Flash"`) {
		t.Fatalf("model not rewritten: %s", got)
	}
	if strings.Contains(got, `"auto"`) {
		t.Fatalf("auto leaked: %s", got)
	}
	// usage.credits（Qoder）补成 usage.credit（网关成本账本口径）。
	withUsage := `{"id":"x","model":"auto","usage":{"credits":0.013,"total_tokens":7}}`
	got = rewriteFrameModel(withUsage, "")
	if !strings.Contains(got, `"credit":0.013`) || !strings.Contains(got, `"credits":0.013`) {
		t.Fatalf("credit not backfilled: %s", got)
	}
	// 已有 credit 时不覆盖。
	both := `{"id":"x","model":"auto","usage":{"credit":1,"credits":2}}`
	if out := rewriteFrameModel(both, ""); !strings.Contains(out, `"credit":1`) {
		t.Fatalf("existing credit overwritten: %s", out)
	}
	// 非 JSON / 无 model / 空模型名：原样返回。
	for _, c := range []struct{ in, model string }{
		{`{"firstTokenDuration":1}`, "DeepSeek-Flash"},
		{`not json`, "DeepSeek-Flash"},
		{in, ""},
	} {
		if out := rewriteFrameModel(c.in, c.model); out != c.in {
			t.Errorf("rewriteFrameModel(%q,%q) = %q, want unchanged", c.in, c.model, out)
		}
	}
}
