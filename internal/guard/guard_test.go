package guard

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGateConcurrencyAndPacing(t *testing.T) {
	g := New(Config{Enabled: true, MaxConcurrency: 1, MinGap: 0, MaxWait: time.Second})
	ctx := context.Background()
	l1, ok := g.Acquire(ctx, "u1")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	// 第二个请求必须等待，取消后返回且不泄漏 waiting 计数。
	ctx2, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := g.Acquire(ctx2, "u1"); ok {
		t.Fatal("second acquire should time out while slot is held")
	}
	if got := g.Snapshot().Waiting; got != 0 {
		t.Fatalf("waiting=%d, want 0", got)
	}
	l1.Release()
	l1.Release() // 幂等
	if got := g.Snapshot().InFlight; got != 0 {
		t.Fatalf("inflight=%d, want 0", got)
	}
}

func TestGateFailOpenCountsRejected(t *testing.T) {
	g := New(Config{Enabled: true, MaxConcurrency: 1, MinGap: 0, MaxWait: 20 * time.Millisecond})
	l1, _ := g.Acquire(context.Background(), "u1")
	defer l1.Release()
	start := time.Now()
	lease, ok := g.Acquire(context.Background(), "u1")
	if ok {
		t.Fatal("acquire should report fail-open when max_wait expires")
	}
	if lease == nil {
		t.Fatal("fail-open must still return a lease")
	}
	lease.Release()
	if time.Since(start) < 15*time.Millisecond {
		t.Fatal("fail-open returned before max_wait")
	}
	if got := g.Snapshot().Rejected; got != 1 {
		t.Fatalf("rejected=%d, want 1", got)
	}
}

func TestGatePacing(t *testing.T) {
	g := New(Config{Enabled: true, MaxConcurrency: 2, MinGap: 80 * time.Millisecond, MaxWait: time.Second})
	l1, _ := g.Acquire(context.Background(), "u1")
	l1.Release()
	start := time.Now()
	l2, _ := g.Acquire(context.Background(), "u1")
	l2.Release()
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("pacing wait = %v, want >= ~80ms", elapsed)
	}
	if got := g.Snapshot().PacedTotal; got != 2 {
		t.Fatalf("paced=%d, want 2", got)
	}
}

func TestGateDisabledIsNoop(t *testing.T) {
	g := New(Config{Enabled: false})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, ok := g.Acquire(context.Background(), "u1")
			if !ok {
				t.Error("disabled gate must always allow")
			}
			l.Release()
		}()
	}
	wg.Wait()
	if g.Snapshot().InFlight != 0 {
		t.Fatal("inflight should return to zero")
	}
}
