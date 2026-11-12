package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/qoder"
)

func newQoderPanel(t *testing.T) (*Panel, string, *int, *bool) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "qoder_auths")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	enabledCalls := false
	p := New(Deps{
		QoderEnabled: true,
		QoderAuthDir: dir,
		QoderLogin:   qoder.NewLoginManager(qoder.New(qoder.Config{}), dir),
		ReloadQoderAuths: func() (int, error) {
			reloads++
			auths, err := loadQoderCount(dir)
			return auths, err
		},
		SetQoderAccountEnabled: func(uid string, enabled bool) error {
			enabledCalls = true
			return nil
		},
	})
	return p, dir, &reloads, &enabledCalls
}

func loadQoderCount(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n, nil
}

func doPanel(t *testing.T, p *Panel, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestQoderImportThenDeleteAndEnable(t *testing.T) {
	p, dir, reloads, enabledCalls := newQoderPanel(t)

	body := `{"accounts":[
		{"uid":"q-u1","nickname":"one","accessToken":"at1","refreshToken":"drt-1","realm":"cn"},
		{"uid":"q-u2","nickname":"two","accessToken":"at2","realm":"intl"}
	]}`
	code, out := doPanel(t, p, http.MethodPost, "/panel/api/qoder/import", body)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("import: code=%d out=%v", code, out)
	}
	if n := len(out["imported"].([]any)); n != 2 {
		t.Fatalf("imported = %v", out["imported"])
	}
	for _, name := range []string{"qoder-q-u1.json", "qoder-q-u2.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	if *reloads != 1 {
		t.Errorf("reloads = %d, want 1", *reloads)
	}

	// 删除 qoder 账号：走 Qoder 目录 + backup。
	code, out = doPanel(t, p, http.MethodPost, "/panel/api/accounts/delete",
		`{"uid":"q-u1","provider":"qoder"}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("delete: code=%d out=%v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "qoder-q-u1.json")); !os.IsNotExist(err) {
		t.Errorf("qoder file should be removed, stat err = %v", err)
	}
	baks, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), "backups", "qoder-q-u1.json.*.bak"))
	if len(baks) != 1 {
		t.Errorf("backup files = %v", baks)
	}

	// 启用/禁用按 provider 路由。
	code, out = doPanel(t, p, http.MethodPost, "/panel/api/accounts/enable",
		`{"uid":"q-u2","enabled":false,"provider":"qoder"}`)
	if code != http.StatusOK || out["ok"] != true || !*enabledCalls {
		t.Fatalf("enable: code=%d out=%v called=%v", code, out, *enabledCalls)
	}

	// 非法 uid 必须被拒（防路径穿越）。
	code, _ = doPanel(t, p, http.MethodPost, "/panel/api/accounts/delete",
		`{"uid":"../evil","provider":"qoder"}`)
	if code != http.StatusBadRequest {
		t.Errorf("traversal uid code = %d, want 400", code)
	}
}

func TestQoderDisabledRejected(t *testing.T) {
	p := New(Deps{QoderEnabled: false})
	code, out := doPanel(t, p, http.MethodPost, "/panel/api/qoder/import", `{}`)
	if code != http.StatusNotImplemented {
		t.Errorf("import while disabled code = %d out=%v", code, out)
	}
	code, _ = doPanel(t, p, http.MethodPost, "/panel/api/accounts/delete",
		`{"uid":"x","provider":"qoder"}`)
	if code != http.StatusBadRequest {
		t.Errorf("delete while disabled code = %d, want 400", code)
	}
}
