package agentd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PatchMon/docker-updater/internal/compose"
	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/job"
	"github.com/PatchMon/docker-updater/internal/pipeline"
	"github.com/PatchMon/docker-updater/internal/wire"
)

func newTestConfig(t *testing.T, sock string) *config.Config {
	t.Helper()

	stack := t.TempDir()
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := "agent_socket: " + sock + "\n" +
		"auth:\n  bearer_token: 0123456789abcdef0123456789abcdef\n" +
		"targets:\n  - name: smoke\n    dir: " + stack + "\n    compose_file: docker-compose.yml\n"

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pmu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

func startAgent(t *testing.T, cfg *config.Config) string {
	t.Helper()

	listener, err := Listen(cfg.AgentSocket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, pipeline.New(compose.New("definitely-not-a-real-docker-binary")), log)

	httpServer := &http.Server{Handler: srv.Handler(), ConnContext: ConnContext}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})
	return cfg.AgentSocket
}

func post(t *testing.T, sock, body string) (int, string) {
	t.Helper()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 20 * time.Second,
	}
	resp, err := client.Post("http://agent"+wire.ExecPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestListenCreatesRestrictedSocket(t *testing.T) {
	sock := shortSocket(t)
	listener, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != socketMode {
		t.Errorf("socket mode = %04o, want %04o", perm, socketMode)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("socket mode %04o allows world access", perm)
	}
}

func TestListenRefusesToClobberARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notasocket")
	if err := os.WriteFile(path, []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Listen(path); err == nil {
		t.Fatal("Listen should refuse to replace a regular file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Listen deleted a file it should have left alone")
	}
}

func TestAgentRejectsUntrustedInput(t *testing.T) {
	cfg := newTestConfig(t, shortSocket(t))
	sock := startAgent(t, cfg)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown target", `{"target":"nope"}`, http.StatusNotFound},
		{"traversal as target", `{"target":"../../etc/passwd"}`, http.StatusNotFound},
		{"empty target", `{}`, http.StatusNotFound},
		{"tag without image_tag_env", `{"target":"smoke","tag":"2.0.4"}`, http.StatusBadRequest},
		{"unknown field", `{"target":"smoke","cmd":"rm -rf /"}`, http.StatusBadRequest},
		{"not json", `not json at all`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, sock, tc.body)
			if status != tc.want {
				t.Fatalf("status = %d body = %s, want %d", status, body, tc.want)
			}
		})
	}
}

func TestAgentSerialisesPerTarget(t *testing.T) {
	cfg := newTestConfig(t, shortSocket(t))
	srv := NewServer(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !srv.acquire("smoke") {
		t.Fatal("first acquire should succeed")
	}
	if srv.acquire("smoke") {
		t.Fatal("second acquire for the same target must fail")
	}
	if !srv.acquire("other") {
		t.Fatal("a different target must not be blocked")
	}
	srv.release("smoke")
	if !srv.acquire("smoke") {
		t.Fatal("acquire should succeed after release")
	}
}

func TestEventRoundTrip(t *testing.T) {
	original := wire.Event{Type: wire.EventResult, State: job.StateRolledBack, Message: "rolled back"}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	var decoded wire.Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != job.StateRolledBack || decoded.Message != "rolled back" {
		t.Errorf("decoded = %+v", decoded)
	}
}

type fakeBackend struct {
	started  chan struct{}
	finished chan error
}

func (f *fakeBackend) Update(ctx context.Context, _ *config.Target, _ job.Request, _ job.Sink) (job.State, string, error) {
	close(f.started)
	select {
	case <-ctx.Done():
		f.finished <- ctx.Err()
	case <-time.After(3 * time.Second):
		f.finished <- nil
	}
	return job.StateSucceeded, "done", nil
}

func TestUpdateSurvivesTheCallerDisconnecting(t *testing.T) {
	cfg := newTestConfig(t, shortSocket(t))
	backend := &fakeBackend{started: make(chan struct{}), finished: make(chan error, 1)}

	listener, err := Listen(cfg.AgentSocket)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpServer := &http.Server{Handler: srv.Handler(), ConnContext: ConnContext}
	go func() { _ = httpServer.Serve(listener) }()
	defer func() { _ = httpServer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", cfg.AgentSocket)
		},
	}}

	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+wire.ExecPath,
			strings.NewReader(`{"target":"smoke"}`))
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		t.Fatal("update never started")
	}

	cancel()

	select {
	case err := <-backend.finished:
		if err != nil {
			t.Fatalf("the update was cancelled when the caller went away (%v); a restart of the API service would roll back a healthy deploy", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("update never finished")
	}
}
