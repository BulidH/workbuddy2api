package panel

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/agentcfg"
)

func agentsPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	home := t.TempDir()
	p := New(Deps{
		ConfigPath: "config.json",
		Agents:     agentcfg.New(filepath.Join(home, "state"), home),
	})
	return p, home
}

func TestAgentsOverviewEndpoint(t *testing.T) {
	p, _ := agentsPanel(t)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/agents", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "claude-code") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAgentsPlanAndApplyEndpoints(t *testing.T) {
	p, home := agentsPanel(t)
	body := `{"client":"crush","base_url":"http://127.0.0.1:8787","api_key":"sk-x","model":"m"}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/agents/plan", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "crush.json") {
		t.Fatalf("plan code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/agents/apply", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("apply code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/agents/restore",
		strings.NewReader(`{"client":"crush"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "deleted") {
		t.Fatalf("restore code=%d body=%s", rec.Code, rec.Body.String())
	}
	_ = home
}

func TestAgentsValidation(t *testing.T) {
	p, _ := agentsPanel(t)
	cases := []string{
		`{"client":"crush"}`,
		`{"client":"crush","base_url":"http://x"}`,
		`{"client":"nope","base_url":"http://x","api_key":"k"}`,
	}
	for _, body := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/agents/apply", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s -> code=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestAgentsDisabled(t *testing.T) {
	p := New(Deps{ConfigPath: "config.json"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/agents", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d", rec.Code)
	}
}
