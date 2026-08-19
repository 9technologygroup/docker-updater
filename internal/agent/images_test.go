package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/wire"
)

func serveSocket(t *testing.T, handler http.Handler) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "dup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "a.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sock
}

func TestClientImagesReadsTheAgentResult(t *testing.T) {
	var got wire.ImagesRequest
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != wire.ImagesPath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(wire.ImagesResult{
			Images:     map[string]string{"app": "harbor.example.com/library/app:1.0"},
			Registries: []string{"harbor.example.com"},
		})
	}))

	result, err := NewClient(sock).Images(context.Background(), "pmon")
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if got.Target != "pmon" {
		t.Errorf("the agent was asked about %q, want pmon", got.Target)
	}
	if result.Images["app"] != "harbor.example.com/library/app:1.0" {
		t.Errorf("images = %v", result.Images)
	}
	if len(result.Registries) != 1 || result.Registries[0] != "harbor.example.com" {
		t.Errorf("registries = %v", result.Registries)
	}
}

func TestClientImagesSurfacesTheAgentsRefusal(t *testing.T) {
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": `unknown target "pmon"`})
	}))

	_, err := NewClient(sock).Images(context.Background(), "pmon")
	if err == nil {
		t.Fatal("a 404 from the agent must be an error")
	}
	if !strings.Contains(err.Error(), `unknown target "pmon"`) {
		t.Fatalf("err = %v, want the agent's own reason", err)
	}
}
