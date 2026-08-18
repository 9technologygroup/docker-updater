package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/selfupdate"
	"github.com/9technologygroup/docker-updater/internal/version"
)

func testChecker(t *testing.T, handler http.HandlerFunc) *selfupdate.Checker {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &selfupdate.Checker{
		Repo:      "owner/name",
		BaseURL:   srv.URL,
		CachePath: filepath.Join(t.TempDir(), "update-check.json"),
		TTL:       time.Hour,
		Client:    srv.Client(),
		Now:       time.Now,
	}
}

func serveRelease(tag string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false,"published_at":"2026-08-17T12:04:11Z"}`, tag)
	}
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// install.sh captures `dup version` into a shell variable. Anything beyond one
// line on stdout corrupts it, so the advisory has to go to stderr.
func TestVersionKeepsStdoutToOneLine(t *testing.T) {
	withVersion(t, "v1.3.0")
	var stdout, stderr strings.Builder

	if err := runVersionTo(nil, &stdout, &stderr, testChecker(t, serveRelease("v1.4.0"))); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}

	if n := strings.Count(stdout.String(), "\n"); n != 1 {
		t.Errorf("stdout has %d newlines, want 1:\n%s", n, stdout.String())
	}
	if !strings.Contains(stdout.String(), "v1.3.0") {
		t.Errorf("stdout does not name the running version: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "v1.4.0") {
		t.Errorf("stderr does not carry the advisory: %q", stderr.String())
	}
}

func TestVersionSaysNothingWhenUpToDate(t *testing.T) {
	withVersion(t, "v1.4.0")
	var stdout, stderr strings.Builder

	if err := runVersionTo(nil, &stdout, &stderr, testChecker(t, serveRelease("v1.4.0"))); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}
	if stderr.String() != "" {
		t.Errorf("stderr should be empty when up to date, got %q", stderr.String())
	}
}

func TestVersionIsSilentWhenTheCheckFails(t *testing.T) {
	withVersion(t, "v1.3.0")
	var stdout, stderr strings.Builder

	checker := testChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := runVersionTo(nil, &stdout, &stderr, checker); err != nil {
		t.Fatalf("a failed check must not be an error: %v", err)
	}
	if stderr.String() != "" {
		t.Errorf("a failed check should be silent by default, got %q", stderr.String())
	}
}

func TestVersionCheckReportsWhyItFailed(t *testing.T) {
	withVersion(t, "v1.3.0")
	var stdout, stderr strings.Builder

	checker := testChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := runVersionTo([]string{"--check"}, &stdout, &stderr, checker); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}
	if !strings.Contains(stderr.String(), "could not check") {
		t.Errorf("--check should explain the failure, got %q", stderr.String())
	}
}

func TestVersionNoCheckMakesNoRequest(t *testing.T) {
	withVersion(t, "v1.3.0")
	var hits atomic.Int32
	var stdout, stderr strings.Builder

	checker := testChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		serveRelease("v1.4.0")(w, nil)
	})
	if err := runVersionTo([]string{"--no-check"}, &stdout, &stderr, checker); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("--no-check still called the API")
	}
	if stderr.String() != "" {
		t.Errorf("--no-check should say nothing, got %q", stderr.String())
	}
}

func TestVersionDisabledByEnv(t *testing.T) {
	t.Setenv(selfupdate.DisableEnv, "1")
	withVersion(t, "v1.3.0")
	var hits atomic.Int32
	var stdout, stderr strings.Builder

	checker := testChecker(t, func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) })
	// Explicit --check must not override a fleet-wide opt-out.
	if err := runVersionTo([]string{"--check"}, &stdout, &stderr, checker); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("the disable environment variable was overridden by --check")
	}
	if !strings.Contains(stderr.String(), selfupdate.DisableEnv) {
		t.Errorf("stderr should name the variable that disabled it, got %q", stderr.String())
	}
}

func TestVersionFullAddsALatestLine(t *testing.T) {
	withVersion(t, "v1.3.0")
	var stdout, stderr strings.Builder

	if err := runVersionTo([]string{"--full"}, &stdout, &stderr, testChecker(t, serveRelease("v1.4.0"))); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"commit:", "licence:", "source:", "latest:", "v1.4.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("--full output is missing %q:\n%s", want, out)
		}
	}
}

// dup --version --full used to drop the flag, because the alias branch called
// runVersion(nil) and discarded every argument after the first.
func TestVersionAliasesForwardTheirFlags(t *testing.T) {
	withVersion(t, "v1.3.0")
	for _, alias := range []string{"version", "ver", "-ver", "--version", "-v", "--ver"} {
		// An undefined flag can only be reported if the arguments reached the
		// command, so this fails loudly if they are dropped again.
		if err := Run([]string{alias, "--definitely-not-a-flag"}); err == nil {
			t.Errorf("dup %s --definitely-not-a-flag was accepted, so its flags were discarded", alias)
		}
	}
}

func TestVersionDevBuildDoesNotCheck(t *testing.T) {
	withVersion(t, "dev")
	var hits atomic.Int32
	var stdout, stderr strings.Builder

	checker := testChecker(t, func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) })
	if err := runVersionTo(nil, &stdout, &stderr, checker); err != nil {
		t.Fatalf("runVersionTo: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("a dev build called the API")
	}
}

// TestMain clears dup's own environment variables. An operator may well have
// DUP_NO_UPDATE_CHECK exported, the docs suggest it, and inheriting it made ten
// tests fail for a reason that had nothing to do with the code.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"DUP_NO_UPDATE_CHECK", "DUP_GITHUB_TOKEN", "DUP_GITHUB_REPO", "DUP_CACHE_FILE",
		"UPDATER_BEARER_TOKEN", "UPDATER_GITHUB_SECRET",
	} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
