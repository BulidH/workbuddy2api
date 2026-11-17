package usageledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAndSince(t *testing.T) {
	l := New("", time.Hour)
	base := time.Now().Truncate(time.Second)
	l.Record(Event{Time: base.Add(-2 * time.Hour), Provider: "workbuddy", UID: "u1"})
	l.Record(Event{Time: base.Add(-1 * time.Hour), Provider: "qoder", UID: "u2", Credit: 3.5})
	l.Record(Event{Time: base, Provider: "loomy", UID: "u3"})

	got := l.Since(base.Add(-90 * time.Minute))
	if len(got) != 2 {
		t.Fatalf("Since 应返回 2 条，got %d", len(got))
	}
	if got[0].Provider != "qoder" || got[0].Credit != 3.5 {
		t.Errorf("got[0]=%+v", got[0])
	}
	if got[1].Provider != "loomy" {
		t.Errorf("got[1]=%+v", got[1])
	}
}

func TestRecordSkipsInvalid(t *testing.T) {
	l := New("", time.Hour)
	l.Record(Event{Provider: "", UID: "u1"})
	l.Record(Event{Provider: "workbuddy", UID: ""})
	if l.Len() != 0 {
		t.Fatalf("缺 provider/uid 的事件不应入账，got %d", l.Len())
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage_ledger.jsonl")
	l := New(path, DefaultMaxAge)
	base := time.Now().Truncate(time.Second)
	l.Record(Event{Time: base.Add(-time.Hour), Provider: "qoder", UID: "q1", Credit: 2, Model: "DeepSeek-Flash"})
	l.Record(Event{Time: base, Provider: "loomy", UID: "l1"})

	// 新账本从文件加载：事件不丢，坏行跳过。
	l2 := New(path, DefaultMaxAge)
	if err := l2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if l2.Len() != 2 {
		t.Fatalf("加载后应有 2 条，got %d", l2.Len())
	}
	got := l2.Since(base.Add(-2 * time.Hour))
	if len(got) != 2 || got[0].Provider != "qoder" || got[0].Model != "DeepSeek-Flash" {
		t.Errorf("round-trip 数据不一致: %+v", got)
	}
}

func TestLoadPrunesOldEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage_ledger.jsonl")
	l := New(path, time.Hour) // 保留窗口 1 小时
	base := time.Now().Truncate(time.Second)
	l.Record(Event{Time: base.Add(-3 * time.Hour), Provider: "workbuddy", UID: "old"})
	l.Record(Event{Time: base, Provider: "workbuddy", UID: "new"})

	l2 := New(path, time.Hour)
	if err := l2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if l2.Len() != 1 {
		t.Fatalf("超窗口事件应被裁剪，got %d", l2.Len())
	}
}

func TestLoadCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage_ledger.jsonl")
	base := time.Now().Truncate(time.Second)
	line := `{"t":"` + base.Format(time.RFC3339Nano) + `","p":"workbuddy","u":"u1"}`
	if err := os.WriteFile(path, []byte("not json\n"+line+"\n{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(path, DefaultMaxAge)
	if err := l.Load(); err != nil {
		t.Fatalf("坏行不应阻断加载: %v", err)
	}
	if l.Len() != 1 {
		t.Fatalf("应加载 1 条好行，got %d", l.Len())
	}
}

func TestRingEviction(t *testing.T) {
	l := New("", time.Hour)
	l.maxN = 3
	base := time.Now().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		l.Record(Event{Time: base.Add(time.Duration(i) * time.Minute), Provider: "workbuddy", UID: "u"})
	}
	if l.Len() != 3 {
		t.Fatalf("超上限应淘汰最旧，got %d", l.Len())
	}
	got := l.Since(base)
	if len(got) != 3 || !got[0].Time.Equal(base.Add(2 * time.Minute)) {
		t.Errorf("淘汰后应保留最新 3 条（首条为 base+2min），got %+v", got)
	}
}
