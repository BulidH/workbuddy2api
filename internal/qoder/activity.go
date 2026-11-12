// activity.go 本地活动流水（JSONL）：签到 / 领取 / 兑换码结果的落盘记录。
//
// 面板「Qoder 活动」页读它展示历史，不依赖上游；文件按天追加即可，无需轮转。
package qoder

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ActivityEntry 一条活动流水。
type ActivityEntry struct {
	Time     string `json:"time"` // RFC3339（本地时区）
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Kind     string `json:"kind"` // checkin / claim / code
	Campaign string `json:"campaign,omitempty"`
	Credits  int64  `json:"credits,omitempty"` // 本轮获得的 Credits
	Status   string `json:"status"`            // ok / already / blocked / pending / locked / fail / skipped
	Message  string `json:"message,omitempty"`
}

// ActivityLog JSONL 追加写入 + 尾部读取。
type ActivityLog struct {
	path string
	mu   sync.Mutex
}

// NewActivityLog 构造；path 为空时返回 nil（调用方据此跳过记录）。
func NewActivityLog(path string) *ActivityLog {
	if path == "" {
		return nil
	}
	return &ActivityLog{path: path}
}

// Log 追加一条流水；失败静默（活动记录不应影响签到结论），返回是否写入成功。
func (l *ActivityLog) Log(e ActivityEntry) bool {
	if l == nil {
		return false
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return false
	}
	return true
}

// Tail 返回最近 n 条流水（按写入顺序，旧→新）。读取失败返回空切片。
func (l *ActivityLog) Tail(n int) []ActivityEntry {
	if l == nil || n <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	// 文件很大时也只保留尾部 n 行（环形覆盖）。
	ring := make([]ActivityEntry, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e ActivityEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if len(ring) == n {
			copy(ring, ring[1:])
			ring[n-1] = e
		} else {
			ring = append(ring, e)
		}
	}
	return ring
}
