// logring.go 进程内日志环形缓冲：给面板的「日志」页提供近实时日志。
//
// 两个来源：
//   - Ring 实现 io.Writer，被 main 用 log.SetOutput(io.MultiWriter(os.Stderr, ring))
//     挂上，捕获标准 log 输出（启动信息、调度器、错误）。
//   - server 包的聊天表格日志走 fmt.Fprintf(os.Stdout)，不经 log，故由 server 通过
//     SetChatLogSink 回调直接投喂（见 internal/server/logging.go）。
package panel

import (
	"strings"
	"sync"
	"time"
)

// LogLine 一条日志。
type LogLine struct {
	Time  time.Time `json:"t"`
	Level string    `json:"level"` // info / warn / error / chat
	Text  string    `json:"text"`
}

// Ring 定长环形缓冲，并发安全。
type Ring struct {
	mu   sync.RWMutex
	buf  []LogLine
	next int
	full bool
	max  int
}

// NewRing 构造容量为 max 的环形缓冲。
func NewRing(max int) *Ring {
	if max <= 0 {
		max = 500
	}
	return &Ring{buf: make([]LogLine, max), max: max}
}

// Add 写入一条。
func (r *Ring) Add(level, text string) {
	if r == nil {
		return
	}
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	r.mu.Lock()
	r.buf[r.next] = LogLine{Time: time.Now(), Level: level, Text: text}
	r.next = (r.next + 1) % r.max
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Write 实现 io.Writer（供 log.SetOutput 多路复用）。按行拆，剥掉 log 自带的时间戳前缀。
func (r *Ring) Write(p []byte) (int, error) {
	n := len(p)
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lvl := "info"
		low := strings.ToLower(line)
		switch {
		case strings.Contains(low, "error"), strings.Contains(low, "fatal"), strings.Contains(low, "failed"):
			lvl = "error"
		case strings.Contains(low, "warn"):
			lvl = "warn"
		}
		r.Add(lvl, stripLogTimestamp(line))
	}
	return n, nil
}

// stripLogTimestamp 去掉标准 log 的 "2006/01/02 15:04:05 " 前缀，只留正文。
func stripLogTimestamp(s string) string {
	// 形如 2026/09/15 10:45:08 msg —— 长度 20 且第 5、8 位为 '/'，第 11 位空格
	if len(s) > 20 && s[4] == '/' && s[7] == '/' && s[10] == ' ' && s[13] == ':' {
		return s[20:]
	}
	return s
}

// Tail 返回最近 n 条（按时间正序，最旧的在前）。
func (r *Ring) Tail(n int) []LogLine {
	if r == nil {
		return []LogLine{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	size := r.next
	if r.full {
		size = r.max
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]LogLine, 0, n)
	// 定位有效区间的起始下标
	start := 0
	if r.full {
		start = r.next
	}
	total := size
	skip := total - n
	for i := 0; i < total; i++ {
		if i < skip {
			continue
		}
		out = append(out, r.buf[(start+i)%r.max])
	}
	return out
}
