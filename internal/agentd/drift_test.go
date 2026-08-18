package agentd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

func writeCfg(t *testing.T, dir string, targets ...string) string {
	t.Helper()
	body := "agent_peer_user: root\ntargets:\n"
	for _, name := range targets {
		stack := filepath.Join(dir, name)
		if err := os.MkdirAll(stack, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		body += "  - name: " + name + "\n    dir: " + stack + "\n"
	}
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Ibrahim's case: a target added to the config, only the API service restarted,
// so the agent 404s with something that reads like a typo.
func TestUnknownTargetExplainsAStaleAgentConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, "old-stack")

	loaded, err := config.LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(loaded, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithConfigPath(path)

	// The operator adds a stack and does not restart the agent.
	writeCfg(t, dir, "old-stack", "patchmon-pmon")

	msg := s.unknownTarget("patchmon-pmon")
	for _, want := range []string{"after this agent started", "restart dup-agent"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}

func TestUnknownTargetWhenTheConfigReallyLacksIt(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, "only-this")

	loaded, _ := config.LoadAgent(path)
	s := NewServer(loaded, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithConfigPath(path)

	msg := s.unknownTarget("typo")
	if strings.Contains(msg, "restart") {
		t.Errorf("an unchanged config should not blame staleness: %s", msg)
	}
	if !strings.Contains(msg, "not in") {
		t.Errorf("message should say it is not in the config: %s", msg)
	}
}

func TestHealthReportsTheConfigItIsRunning(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, "web")
	loaded, _ := config.LoadAgent(path)
	s := NewServer(loaded, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithConfigPath(path)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var out wire.HealthResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ConfigFingerprint != loaded.Fingerprint() || out.ConfigFingerprint == "" {
		t.Errorf("fingerprint = %q, want %q", out.ConfigFingerprint, loaded.Fingerprint())
	}
	if len(out.Targets) != 1 || out.Targets[0] != "web" {
		t.Errorf("targets = %v", out.Targets)
	}
	if out.ConfigLoadedAt.IsZero() {
		t.Error("load time not reported")
	}
	_ = context.Background()
}

// Two different configs must not fingerprint the same, or drift is undetectable.
func TestFingerprintChangesWithTheFile(t *testing.T) {
	dir := t.TempDir()
	a, _ := config.LoadAgent(writeCfg(t, dir, "one"))
	b, _ := config.LoadAgent(writeCfg(t, dir, "one", "two"))
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("fingerprint did not change when the config did")
	}
	if a.Fingerprint() == "" {
		t.Error("fingerprint is empty")
	}
}
