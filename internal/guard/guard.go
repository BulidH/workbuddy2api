// Package guard 实现 Qoder 账号的出站流量闸门：单号并发上限 + 请求节奏控制 +
// 排队/拒绝遥测。
//
// 设计边界：
//   - 闸门按账号 UID 分片，仅作用于 Qoder 上游（WorkBuddy 侧沿用既有 pool 语义）；
//   - 并发上限独立于 pool.MaxInFlight：pool 是"抢不到就换号"，闸门是"在
//     max_wait 内排队等"，两者叠加用于压住单号瞬时并发；
//   - 节奏控制保证同一账号两次出站开始之间至少间隔 min_gap（± jitter），
//     避免批量工具调用把同一账号打到风控阈值；
//   - 等待超时后 fail-open：放行并计数，绝不把用户的正常请求卡死。
package guard

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Config 闸门配置。
type Config struct {
	Enabled        bool
	MaxConcurrency int           // 单号并发上限，默认 1
	MinGap         time.Duration // 同号两次出站最小间隔，默认 400ms
	MaxWait        time.Duration // 等并发名额的上限，默认 20s；超时 fail-open
	Jitter         time.Duration // 节奏抖动上限，默认 120ms
}

// DefaultConfig 返回与 qoder2api-hub 对齐的默认值（开关由调用方决定）。
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		MaxConcurrency: 1,
		MinGap:         400 * time.Millisecond,
		MaxWait:        20 * time.Second,
		Jitter:         120 * time.Millisecond,
	}
}

func (c Config) normalized() Config {
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 1
	}
	if c.MinGap < 0 {
		c.MinGap = 0
	}
	if c.MaxWait < 0 {
		c.MaxWait = 0
	}
	if c.Jitter < 0 {
		c.Jitter = 0
	}
	return c
}

type accountState struct {
	sem  chan struct{}
	mu   sync.Mutex
	last time.Time
}

// Stats 闸门累计计数，供 /healthz 与面板排队遥测。
type Stats struct {
	Enabled        bool  `json:"enabled"`
	MaxConcurrency int   `json:"max_concurrency"`
	MinGapMS       int64 `json:"min_gap_ms"`
	Waiting        int64 `json:"waiting"`  // 当前等待名额的请求数
	InFlight       int64 `json:"inflight"` // 当前持闸在途数
	QueuedTotal    int64 `json:"queued_total"`
	WaitedMS       int64 `json:"waited_ms_total"`
	Rejected       int64 `json:"rejected"` // 等待超时/fail-open 次数
	PacedTotal     int64 `json:"paced_total"`
	LastWaitMS     int64 `json:"last_wait_ms"`
}

// Gate 账号级流量闸门。
type Gate struct {
	cfg Config

	mu    sync.Mutex
	accts map[string]*accountState

	waiting   atomic.Int64
	inflight  atomic.Int64
	queued    atomic.Int64
	waitedMS  atomic.Int64
	rejected  atomic.Int64
	paced     atomic.Int64
	lastWaitM atomic.Int64

	rndMu sync.Mutex
	rnd   *rand.Rand
}

// New 构建闸门。
func New(cfg Config) *Gate {
	cfg = cfg.normalized()
	return &Gate{
		cfg:   cfg,
		accts: map[string]*accountState{},
		rnd:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Enabled 报告闸门是否启用。
func (g *Gate) Enabled() bool { return g != nil && g.cfg.Enabled }

func (g *Gate) state(uid string) *accountState {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.accts[uid]
	if !ok {
		st = &accountState{sem: make(chan struct{}, g.cfg.MaxConcurrency)}
		g.accts[uid] = st
	}
	return st
}

// Lease 一次已获准的出站配额；Release 幂等。
type Lease struct {
	g   *Gate
	uid string
	one sync.Once
}

// Acquire 为指定账号申请出站配额。
//
// 返回的 lease 永不为 nil（闸门关闭时是空 lease）；ok=false 表示等待超时后
// fail-open 放行（调用方照常请求，统计已计入 rejected）。
func (g *Gate) Acquire(ctx context.Context, uid string) (*Lease, bool) {
	if !g.Enabled() || uid == "" {
		return &Lease{}, true
	}
	st := g.state(uid)
	start := time.Now()
	g.waiting.Add(1)
	g.queued.Add(1)

	acquired := false
	select {
	case st.sem <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		g.waiting.Add(-1)
		lease := &Lease{}
		return lease, false
	case <-time.After(g.cfg.MaxWait):
		// fail-open：不占并发配额，但允许请求继续，避免闸门自身变成故障点。
		g.waiting.Add(-1)
		g.rejected.Add(1)
		lease := &Lease{}
		return lease, false
	}

	g.waiting.Add(-1)
	g.inflight.Add(1)

	// 节奏控制：同号两次出站开始之间至少间隔 MinGap（含抖动）。
	wait := g.pacingWait(st)
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			g.inflight.Add(-1)
			if acquired {
				<-st.sem
			}
			return &Lease{}, false
		}
	}
	g.paced.Add(1)
	elapsed := time.Since(start)
	g.waitedMS.Add(elapsed.Milliseconds())
	g.lastWaitM.Store(elapsed.Milliseconds())

	return &Lease{g: g, uid: uid}, true
}

// pacingWait 计算本号距上次出站开始还需等待的时长，并登记本次开始时间。
func (g *Gate) pacingWait(st *accountState) time.Duration {
	if g.cfg.MinGap <= 0 {
		return 0
	}
	jitter := time.Duration(0)
	if g.cfg.Jitter > 0 {
		g.rndMu.Lock()
		jitter = time.Duration(g.rnd.Int63n(int64(g.cfg.Jitter) + 1))
		g.rndMu.Unlock()
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last.IsZero() {
		st.last = time.Now()
		return 0
	}
	next := st.last.Add(g.cfg.MinGap + jitter)
	if now := time.Now(); now.Before(next) {
		st.last = next
		return next.Sub(now)
	}
	st.last = time.Now()
	return 0
}

// Release 归还在途配额（幂等）。
func (l *Lease) Release() {
	if l == nil || l.g == nil {
		return
	}
	l.one.Do(func() {
		g := l.g
		g.inflight.Add(-1)
		if st, ok := g.accts[l.uid]; ok {
			select {
			case <-st.sem:
			default:
			}
		}
	})
}

// Snapshot 返回闸门计数快照。
func (g *Gate) Snapshot() Stats {
	if g == nil {
		return Stats{}
	}
	return Stats{
		Enabled:        g.cfg.Enabled,
		MaxConcurrency: g.cfg.MaxConcurrency,
		MinGapMS:       g.cfg.MinGap.Milliseconds(),
		Waiting:        g.waiting.Load(),
		InFlight:       g.inflight.Load(),
		QueuedTotal:    g.queued.Load(),
		WaitedMS:       g.waitedMS.Load(),
		Rejected:       g.rejected.Load(),
		PacedTotal:     g.paced.Load(),
		LastWaitMS:     g.lastWaitM.Load(),
	}
}
