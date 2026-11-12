package agentcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxEditBytes = 8 << 20
	backupKeep   = 10
)

// FileRecord 记录一次 apply 写过的文件，供 restore 精确回退。
type FileRecord struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Backup  string `json:"backup,omitempty"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
}

// State 每个客户端的接入记录（不存明文 Key）。
type State struct {
	AppliedAt string       `json:"applied_at"`
	BaseURL   string       `json:"base_url"`
	Model     string       `json:"model"`
	Files     []FileRecord `json:"files"`
}

type stateFile struct {
	Clients map[string]State `json:"clients"`
}

func readText(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", nil // 缺失 = 空文档：首次从零建配置是合法场景。
	}
	raw, err := readBytes(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func readBytes(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if st.Size() > maxEditBytes {
		return nil, fmt.Errorf("refusing to edit %s (%d bytes > %d limit)", path, st.Size(), maxEditBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return raw, nil
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	tmp := path + ".qagent.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Service) statePath() string { return filepath.Join(s.StateDir, "agent-state.json") }
func (s *Service) backupDir(client string) string {
	return filepath.Join(s.StateDir, "agent-backups", client)
}

func (s *Service) loadState() stateFile {
	out := stateFile{Clients: map[string]State{}}
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out.Clients == nil {
		out.Clients = map[string]State{}
	}
	return out
}

func (s *Service) saveState(st stateFile) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.statePath(), raw)
}

func (s *Service) backupFile(client, path string, original []byte) (string, error) {
	dir := s.backupDir(client)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Base(path) + "." + time.Now().Format("20060102-150405.000000000") + ".bak"
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		return "", err
	}
	pruneBackups(dir)
	return target, nil
}

// pruneBackups 每个原始文件只留最新 backupKeep 份。文件名前缀是时间戳，
// 字典序即时间序。
func pruneBackups(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	groups := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".bak") {
			continue
		}
		parts := strings.Split(name, ".")
		if len(parts) < 4 {
			continue
		}
		base := strings.Join(parts[:len(parts)-3], ".")
		groups[base] = append(groups[base], name)
	}
	for _, names := range groups {
		if len(names) <= backupKeep {
			continue
		}
		sortStrings(names)
		for _, name := range names[:len(names)-backupKeep] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// pruneEmptyParents 删除 restore 后残留的空父目录链（不越过 home）。
func pruneEmptyParents(path, home string) {
	dir := filepath.Dir(path)
	stop := filepath.Clean(home)
	for dir != stop && len(dir) > len(stop) && strings.HasPrefix(dir, stop) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func loadJSON(path, what string) (map[string]any, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%s): %w", what, path, err)
	}
	if data == nil {
		return nil, fmt.Errorf("%s must be a JSON object: %s", what, path)
	}
	return data, nil
}

func dumpJSON(v any) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}
