package qoder

import "testing"

func TestLookupAliasNormalizes(t *testing.T) {
	aliases := map[string]ModelAlias{
		"deepseek-flash": {WorkBuddy: "deepseek-v4.1-flash", Qoder: "DeepSeek-Flash"},
	}
	for _, name := range []string{"deepseek-flash", "DeepSeek-Flash", " DEEPSEEK_FLASH "} {
		a, ok := LookupAlias(aliases, name)
		if !ok {
			t.Fatalf("%q not matched", name)
		}
		if a.Target("qoder") != "DeepSeek-Flash" || a.Target("workbuddy") != "deepseek-v4.1-flash" {
			t.Fatalf("%q targets=%#v", name, a)
		}
	}
	if _, ok := LookupAlias(aliases, "deepseek-v4-pro"); ok {
		t.Fatal("unrelated model matched")
	}
	if _, ok := LookupAlias(nil, "deepseek-flash"); ok {
		t.Fatal("nil aliases matched")
	}
	if a, _ := LookupAlias(aliases, "deepseek-flash"); a.Target("global") != "deepseek-v4.1-flash" {
		t.Fatalf("global target=%q", a.Target("global"))
	}
}
