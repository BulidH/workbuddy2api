package loomy

import (
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   upstream.ErrKind
	}{
		{"积分不足", 400, `{"code":"100003","desc":"insufficient points"}`, upstream.ErrHardCredit},
		{"余额不足 402", 402, `{"msg":"payment required"}`, upstream.ErrHardCredit},
		{"会话失效业务码", 200, `{"code":"020002"}`, upstream.ErrSessionDead},
		{"会话失效 100002", 400, `{"code":"100002"}`, upstream.ErrSessionDead},
		{"401", 401, `{"msg":"unauthorized"}`, upstream.ErrSessionDead},
		{"403 风控", 403, `{"msg":"forbidden"}`, upstream.ErrSoftRate},
		{"429", 429, `{"msg":"too many requests"}`, upstream.ErrSoftRate},
		{"限流文案", 400, `{"msg":"rate limit exceeded"}`, upstream.ErrSoftRate},
		{"404", 404, `{"msg":"not found"}`, upstream.ErrNotFound},
		{"500", 500, `oops`, upstream.ErrServer},
		{"参数错误", 400, `{"msg":"invalid request: messages required"}`, upstream.ErrBadParams},
		{"其它 4xx", 400, `{"msg":"something"}`, upstream.ErrClient},
		{"200 无错误", 200, `{}`, upstream.ErrNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

func TestCatalogFallbackLookup(t *testing.T) {
	c := NewCatalog()
	// 兜底清单里必须有 deepseek（混合模型的落点）。
	if _, ok := c.Lookup(DefaultModel); !ok {
		t.Fatalf("兜底目录应认得 %s", DefaultModel)
	}
	// 大小写/连字符不敏感。
	if _, ok := c.Lookup("DeepSeek-V4-Flash-0731"); !ok {
		t.Error("归一化匹配应当命中")
	}
	if _, ok := c.Lookup("gpt-4o"); ok {
		t.Error("不属于 Loomy 的模型不应命中")
	}
}

func TestCatalogReplace(t *testing.T) {
	c := NewCatalog()
	if c.Fresh() {
		t.Fatal("初始状态不应是 fresh")
	}
	c.Replace([]ModelInfo{{ID: "custom-model"}})
	if !c.Fresh() {
		t.Fatal("Replace 后应为 fresh")
	}
	if _, ok := c.Lookup("custom-model"); !ok {
		t.Error("动态目录应命中")
	}
	// 空结果不覆盖已有动态目录。
	c.Replace(nil)
	if _, ok := c.Lookup("custom-model"); !ok {
		t.Error("空结果不应清空目录")
	}
}

func TestNormalizeModelName(t *testing.T) {
	if NormalizeModelName(" DeepSeek_V4.Flash ") != "deepseekv4flash" {
		t.Errorf("got %q", NormalizeModelName(" DeepSeek_V4.Flash "))
	}
}

func TestOnboardingTaskTable(t *testing.T) {
	sum := 0
	keys := map[string]bool{}
	for _, task := range OnboardingTasks {
		if task.Key == "" || task.Title == "" {
			t.Fatalf("任务定义不完整: %+v", task)
		}
		if keys[task.Key] {
			t.Fatalf("重复的 key: %s", task.Key)
		}
		keys[task.Key] = true
		sum += task.Points
	}
	if len(OnboardingTasks) != 8 {
		t.Errorf("新手任务应为 8 项，得 %d", len(OnboardingTasks))
	}
	if sum != OnboardingTotalPoints {
		t.Errorf("任务总积分 %d != %d", sum, OnboardingTotalPoints)
	}
}

// TestTruncateStripsHTMLErrorPage 上游故障时返回整页 HTML，错误文案不应把标签带进面板。
func TestTruncateStripsHTMLErrorPage(t *testing.T) {
	raw := "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1></center>\r\n<hr><center>xunfei</center>\r\n</body>\r\n</html>"
	got := truncate(raw, 200)
	if strings.ContainsAny(got, "<>") {
		t.Fatalf("仍含 HTML 标签: %q", got)
	}
	if !strings.Contains(got, "502 Bad Gateway") {
		t.Fatalf("丢失关键信息: %q", got)
	}
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Fatalf("未折叠换行: %q", got)
	}
}

// TestTruncateKeepsPlainAndLimitsLength 纯文本原样保留关键内容，并按上限截断。
func TestTruncateKeepsPlainAndLimitsLength(t *testing.T) {
	if got := truncate(`{"code":"000000"}`, 200); got != `{"code":"000000"}` {
		t.Fatalf("纯文本被改写: %q", got)
	}
	long := strings.Repeat("a", 500)
	if got := truncate(long, 20); len(got) != 20 {
		t.Fatalf("长度=%d, want 20", len(got))
	}
}
