package qoder

import "testing"

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4-pro":  "deepseekv4pro",
		"DeepSeek-V4-Pro":  "deepseekv4pro",
		" DeepSeek V4 Pro": "deepseekv4pro",
		"kimi-k2.7":        "kimik27",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogLookupAlias(t *testing.T) {
	c := NewCatalog()
	if len(c.Models("cn")) == 0 || len(c.Models("intl")) == 0 {
		t.Fatal("embedded catalogs are empty")
	}
	// 按 key、display_name、归一化裸名三种写法都应命中同一模型。
	var sample ModelInfo
	for _, m := range c.Models("cn") {
		if m.Enabled() && m.Key != "auto" && m.DisplayName != "" {
			sample = m
			break
		}
	}
	if sample.Key == "" {
		t.Fatal("no usable sample model in cn catalog")
	}
	for _, name := range []string{sample.Key, sample.DisplayName, NormalizeModelName(sample.DisplayName)} {
		got, ok := c.Lookup("cn", name)
		if !ok || got.Key != sample.Key {
			t.Errorf("Lookup(%q) = %v, %v; want %q", name, got.Key, ok, sample.Key)
		}
	}
	if hits := c.LookupAny("deepseek-v4-pro"); len(hits) == 0 {
		t.Error("LookupAny(deepseek-v4-pro) should hit at least one realm")
	}
}

func TestNormalizeRealm(t *testing.T) {
	cases := map[string]string{
		"":              "cn",
		"cn":            "cn",
		"intl":          "intl",
		"global":        "intl",
		"international": "intl",
	}
	for in, want := range cases {
		if got := NormalizeRealm(in); got != want {
			t.Errorf("NormalizeRealm(%q) = %q, want %q", in, got, want)
		}
	}
}
