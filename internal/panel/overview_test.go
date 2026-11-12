package panel

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// writeCredFiles 按面板/loader 的文件名约定造凭证文件。内容无效（{"broken"）的
// 文件仍算「候选文件」但不会被 LoadDir/LoadQoderDir 解析入池，用于模拟加载失败。
func writeCredFiles(t *testing.T, dir, prefix string, names ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		body := `{"uid":"` + name + `","accessToken":"at"}`
		if name == "broken" {
			body = `{not-json`
		}
		fp := filepath.Join(dir, prefix+"-"+name+".json")
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// providerStatus 复刻 /status 的 providers 形状；panel.statusMap 会做一次 JSON
// 往返把 map[string]map[string]int 规整成 map[string]any。
func providerStatus(t *testing.T, wb, qd int) func() map[string]any {
	t.Helper()
	return func() map[string]any {
		providers := map[string]map[string]int{
			auth.ProviderWorkBuddy: {"total": wb},
		}
		if qd >= 0 {
			providers[auth.ProviderQoder] = map[string]int{"total": qd}
		}
		return map[string]any{
			"total":     wb + max(qd, 0),
			"providers": providers,
		}
	}
}

// TestOverviewCountsBothProviders 回归本次线上问题：11 个 WorkBuddy + 2 个 Qoder，
// 两个目录都存在且与各自池对齐时，不应再报「凭证文件数与池内账号数不一致」。
func TestOverviewCountsBothProviders(t *testing.T) {
	root := t.TempDir()
	wbDir := writeCredFiles(t, filepath.Join(root, "auths"), "workbuddy",
		"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9", "u10", "u11")
	qDir := writeCredFiles(t, filepath.Join(root, "qoder_auths"), "qoder", "q1", "q2")
	// 混放目录下的无关 .json / 备份不该被算成凭证。
	if err := os.WriteFile(filepath.Join(wbDir, "notes.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wbDir, "workbuddy-u1.json.bak"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(Deps{
		AuthDir:      wbDir,
		QoderEnabled: true,
		QoderAuthDir: qDir,
		StatusJSON:   providerStatus(t, 11, 2),
	})
	code, out := doPanel(t, p, http.MethodGet, "/panel/api/overview", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d out=%v", code, out)
	}
	c := out["counts"].(map[string]any)
	want := map[string]any{
		"total":                float64(13),
		"auth_files":           float64(13),
		"auth_files_workbuddy": float64(11),
		"auth_files_qoder":     float64(2),
		"pool_workbuddy":       float64(11),
		"pool_qoder":           float64(2),
		"credential_mismatch":  false,
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("counts.%s = %#v, want %#v", k, c[k], v)
		}
	}
}

// TestOverviewQoderDisabled Qoder 未启用时，Qoder 目录里的文件完全不参与比较。
func TestOverviewQoderDisabled(t *testing.T) {
	root := t.TempDir()
	wbDir := writeCredFiles(t, filepath.Join(root, "auths"), "workbuddy", "u1")
	qDir := writeCredFiles(t, filepath.Join(root, "qoder_auths"), "qoder", "q1")

	p := New(Deps{
		AuthDir:      wbDir,
		QoderEnabled: false,
		QoderAuthDir: qDir,
		StatusJSON:   providerStatus(t, 1, -1),
	})
	_, out := doPanel(t, p, http.MethodGet, "/panel/api/overview", "")
	c := out["counts"].(map[string]any)
	if c["auth_files"] != float64(1) || c["auth_files_qoder"] != float64(0) {
		t.Errorf("禁用 Qoder 时不应数 Qoder 文件: %v", c)
	}
	if c["credential_mismatch"] != false {
		t.Errorf("1 文件 / 1 账号不该告警: %v", c)
	}
}

// TestOverviewDetectsUnparseableFile 解析失败的候选文件会保留告警，且按 provider
// 分别比较：只 WorkBuddy 坏了一个文件，即使 Qoder 那边一致也要报。
func TestOverviewDetectsUnparseableFile(t *testing.T) {
	root := t.TempDir()
	wbDir := writeCredFiles(t, filepath.Join(root, "auths"), "workbuddy", "u1", "broken")
	qDir := writeCredFiles(t, filepath.Join(root, "qoder_auths"), "qoder", "q1")

	p := New(Deps{
		AuthDir:      wbDir,
		QoderEnabled: true,
		QoderAuthDir: qDir,
		StatusJSON:   providerStatus(t, 1, 1),
	})
	_, out := doPanel(t, p, http.MethodGet, "/panel/api/overview", "")
	c := out["counts"].(map[string]any)
	if c["credential_mismatch"] != true {
		t.Errorf("坏凭证文件应触发告警: %v", c)
	}
	if c["auth_files"] != float64(3) || c["auth_files_workbuddy"] != float64(2) {
		t.Errorf("文件数应包含解析失败的候选文件: %v", c)
	}
}

// TestOverviewDetectsCancelingDrift 两个 provider 的偏差方向相反、总数恰好相等时，
// 只比较总数会漏报；必须按 provider 分别比。
func TestOverviewDetectsCancelingDrift(t *testing.T) {
	root := t.TempDir()
	// WorkBuddy 2 个文件却只有 1 个入池，Qoder 0 个文件却有 1 个在池里：总数都是 2。
	wbDir := writeCredFiles(t, filepath.Join(root, "auths"), "workbuddy", "u1", "broken")
	qDir := writeCredFiles(t, filepath.Join(root, "qoder_auths"), "qoder")

	p := New(Deps{
		AuthDir:      wbDir,
		QoderEnabled: true,
		QoderAuthDir: qDir,
		StatusJSON:   providerStatus(t, 1, 1),
	})
	_, out := doPanel(t, p, http.MethodGet, "/panel/api/overview", "")
	c := out["counts"].(map[string]any)
	if c["total"] != float64(2) || c["auth_files"] != float64(2) {
		t.Fatalf("用例前提：总数应恰好相等: %v", c)
	}
	if c["credential_mismatch"] != true {
		t.Errorf("偏差互相抵消时仍应告警: %v", c)
	}
}

// TestReloadSyncsBothPools 「重载账号」必须同时对齐 WorkBuddy 与 Qoder 池：
// 否则概览黄条提示的补救动作对 Qoder 文件根本无效。
func TestReloadSyncsBothPools(t *testing.T) {
	root := t.TempDir()
	wbDir := writeCredFiles(t, filepath.Join(root, "auths"), "workbuddy", "u1")
	qDir := writeCredFiles(t, filepath.Join(root, "qoder_auths"), "qoder", "q1", "q2")

	wbPool, qPool := pool.New(""), pool.New("")
	p := New(Deps{
		AuthDir:      wbDir,
		QoderEnabled: true,
		QoderAuthDir: qDir,
		StatusJSON:   providerStatus(t, 1, 2),
		ReloadAuths: func() (int, error) {
			a, err := auth.LoadDir(wbDir)
			if err != nil {
				return 0, err
			}
			wbPool.SyncToDir(a)
			return len(a), nil
		},
		ReloadQoderAuths: func() (int, error) {
			a, err := auth.LoadQoderDir(qDir)
			if err != nil {
				return 0, err
			}
			qPool.SyncToDir(a)
			return len(a), nil
		},
	})
	code, out := doPanel(t, p, http.MethodPost, "/panel/api/reload", "")
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("reload: code=%d out=%v", code, out)
	}
	if out["accounts"] != float64(3) || out["workbuddy"] != float64(1) || out["qoder"] != float64(2) {
		t.Errorf("reload 应返回两个池的合计: %v", out)
	}
	wt, _, _, _, _ := wbPool.CountsDetailed()
	qt, _, _, _, _ := qPool.CountsDetailed()
	if wt != 1 || qt != 2 {
		t.Errorf("两个池都应被同步: workbuddy=%d qoder=%d", wt, qt)
	}
}

// TestReloadSkipsQoderWhenDisabled Qoder 未启用时不触碰 Qoder 的重载入口。
func TestReloadSkipsQoderWhenDisabled(t *testing.T) {
	wbCalled, qCalled := false, false
	p := New(Deps{
		AuthDir:      t.TempDir(),
		QoderEnabled: false,
		ReloadAuths: func() (int, error) {
			wbCalled = true
			return 7, nil
		},
		ReloadQoderAuths: func() (int, error) {
			qCalled = true
			return 9, nil
		},
	})
	_, out := doPanel(t, p, http.MethodPost, "/panel/api/reload", "")
	if !wbCalled || qCalled {
		t.Errorf("调用错池: workbuddy=%v qoder=%v", wbCalled, qCalled)
	}
	if out["accounts"] != float64(7) || out["qoder"] != float64(0) {
		t.Errorf("accounts 不该混入 Qoder: %v", out)
	}
}
