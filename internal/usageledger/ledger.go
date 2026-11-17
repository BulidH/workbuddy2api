// Package usageledger 网关本地用量账本：每次成功请求记一条事件，供面板「用量统计」聚合。
//
// 为什么从「逐账号拉上游明细」改为本地账本：
//   - 上游只有 WorkBuddy 有按请求的消耗明细接口，Qoder/Loomy 没有，三上游无法统一统计；
//   - 逐账号拉明细要对每个账号各打一轮上游请求（串行 + 间隔防风控），打开页面要等
//     十几秒到几十秒，用户感知「好卡」；
//   - 网关本身在每次成功请求时已从 SSE 末帧拿到 usage（credit/tokens/model），
//     这是权威扣费口径（与 pool.NoteCreditSpend 同一数据源），本地记账零上游开销、
//     毫秒级返回，且天然覆盖三个上游。
//
// 持久化：JSONL 追加写，启动时加载，按保留窗口裁剪，重启不丢历史。
package usageledger

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event 一条用量事件（一次成功请求的实测消耗）。
type Event struct {
	Time     time.Time `json:"t"`     // 请求完成时刻（UTC+8 墙钟，与面板分桶口径一致）
	Provider string    `json:"p"`     // workbuddy / qoder / loomy
	UID      string    `json:"u"`
	Nickname string    `json:"n,omitempty"`
	Model    string    `json:"m,omitempty"`
	Credit   float64   `json:"c"`     // 实测扣费（0 = 免费请求，仍计入调用量）
	Tokens   int       `json:"tok,omitempty"`
}

// DefaultMaxAge 默认保留窗口：与面板 7 天展示窗口对齐。
const DefaultMaxAge = 7 * 24 * time.Hour

// DefaultMaxEvents 内存事件数上限（超出按最旧淘汰；正常量级远达不到）。
const DefaultMaxEvents = 200000

// Ledger 进程内用量账本 + JSONL 持久化。
type Ledger struct {
	mu     sync.Mutex
	events []Event // 按时间升序

	path    string        // JSONL 持久化路径；空 = 纯内存
	maxAge  time.Duration // 保留窗口
	maxN    int           // 内存上限
	file    *os.File      // 追加写句柄
	writer  *bufio.Writer
}

// New 构建账本。path 非空时启用 JSONL 持久化（追加写，启动需调 Load）。
func New(path string, maxAge time.Duration) *Ledger {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	l := &Ledger{path: path, maxAge: maxAge, maxN: DefaultMaxEvents}
	return l
}

// Load 从 JSONL 加载历史事件并按保留窗口裁剪；随后重写文件（顺带压缩）。
// 文件不存在或损坏（跳过坏行）不报错——账本是观测数据，不该阻断启动。
func (l *Ledger) Load() error {
	if l.path == "" {
		return nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return l.open()
		}
		return err
	}
	defer f.Close()

	cutoff := time.Now().Add(-l.maxAge)
	var kept []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // 坏行跳过（半写/手改），不阻断
		}
		if e.Time.Before(cutoff) {
			continue
		}
		kept = append(kept, e)
	}
	l.mu.Lock()
	l.events = kept
	l.mu.Unlock()
	return l.rewrite()
}

// open 打开（或创建）追加写句柄。
func (l *Ledger) open() error {
	if l.path == "" {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	l.writer = bufio.NewWriterSize(f, 64*1024)
	return nil
}

// rewrite 用当前内存事件重写文件（启动加载后压缩一次，避免无限追加膨胀）。
func (l *Ledger) rewrite() error {
	if l.path == "" {
		return nil
	}
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	events := l.snapshot()
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 64*1024)
	enc := json.NewEncoder(w)
	for _, e := range events {
		_ = enc.Encode(e)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	return l.open()
}

// Record 记一条事件：追加内存（超上限淘汰最旧）+ 追加落盘。
func (l *Ledger) Record(e Event) {
	if e.Provider == "" || e.UID == "" {
		return
	}
	l.mu.Lock()
	l.events = append(l.events, e)
	if len(l.events) > l.maxN {
		drop := len(l.events) - l.maxN
		l.events = append(l.events[:0], l.events[drop:]...)
	}
	l.mu.Unlock()
	l.append(e)
}

// append 追加写落盘（失败只影响持久化，不影响内存账本与请求主流程）。
func (l *Ledger) append(e Event) {
	if l.path == "" {
		return
	}
	if l.writer == nil {
		// 惰性建句柄：未显式调过 Load（如测试或纯内存初始化后直接 Record）时也能落盘。
		if err := l.open(); err != nil {
			return
		}
	}
	_ = json.NewEncoder(l.writer).Encode(e)
	_ = l.writer.Flush()
}

// snapshot 返回当前全部事件（调用方持锁或单线程时使用；查询入口用 Since 自带锁）。
func (l *Ledger) snapshot() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// Since 返回 t 之后的全部事件（按时间升序）。账本本身按时间追加，线性扫描即可
// （几万条以内微秒级；远低于逐账号上游请求的秒级开销）。
func (l *Ledger) Since(t time.Time) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if !e.Time.Before(t) {
			out = append(out, e)
		}
	}
	return out
}

// Len 返回当前内存中的事件数。
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}
