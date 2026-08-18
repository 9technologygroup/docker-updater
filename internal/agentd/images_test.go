package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

type stubBackend struct {
	result wire.ImagesResult
	err    error
	seen   string
}

func (*stubBackend) Update(context.Context, *config.Target, job.Request, job.Sink) (job.State, string, error) {
	return job.StateNoChange, "", nil
}

func (s *stubBackend) Images(_ context.Context, t *config.Target) (wire.ImagesResult, error) {
	s.seen = t.Name
	return s.result, s.err
}

func imagesServer(t *testing.T, backend job.Backend) *httptest.Server {
	t.Helper()

	cfg := newTestConfig(t, shortSocket(t))
	srv := NewServer(cfg, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

func postImages(t *testing.T, base, body string) (int, string) {
	t.Helper()

	resp, err := http.Post(base+wire.ImagesPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestImagesReturnsTheResolvedReferences(t *testing.T) {
	backend := &stubBackend{result: wire.ImagesResult{
		Images:     map[string]string{"app": "harbor.example.com/library/app:1.0"},
		Registries: []string{"harbor.example.com"},
	}}
	srv := imagesServer(t, backend)

	status, body := postImages(t, srv.URL, `{"target":"smoke"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, body)
	}

	var result wire.ImagesResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Images["app"] != "harbor.example.com/library/app:1.0" {
		t.Errorf("images = %v", result.Images)
	}
	if len(result.Registries) != 1 || result.Registries[0] != "harbor.example.com" {
		t.Errorf("registries = %v", result.Registries)
	}
	if backend.seen != "smoke" {
		t.Errorf("the handler resolved %q, want the target from its own config", backend.seen)
	}
}

func TestImagesRejectsUntrustedInput(t *testing.T) {
	srv := imagesServer(t, &stubBackend{})

	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown target", `{"target":"nope"}`, http.StatusNotFound},
		{"traversal as target", `{"target":"../../etc/passwd"}`, http.StatusNotFound},
		{"empty target", `{}`, http.StatusNotFound},
		{"unknown field", `{"target":"smoke","dir":"/etc"}`, http.StatusBadRequest},
		{"not json", `not json at all`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postImages(t, srv.URL, tc.body)
			if status != tc.want {
				t.Fatalf("status = %d body = %s, want %d", status, body, tc.want)
			}
		})
	}
}

func TestImagesReportsABackendFailure(t *testing.T) {
	srv := imagesServer(t, &stubBackend{err: errors.New("compose is not installed")})

	status, body := postImages(t, srv.URL, `{"target":"smoke"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s, want 500", status, body)
	}
	if !strings.Contains(body, "compose is not installed") {
		t.Errorf("body = %s, want the backend's own reason", body)
	}
}

func TestImagesIsNotImplementedWithoutAnImager(t *testing.T) {
	srv := imagesServer(t, backendWithoutImages{})

	status, body := postImages(t, srv.URL, `{"target":"smoke"}`)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d body = %s, want 501", status, body)
	}
}

type backendWithoutImages struct{}

func (backendWithoutImages) Update(context.Context, *config.Target, job.Request, job.Sink) (job.State, string, error) {
	return job.StateNoChange, "", nil
}
