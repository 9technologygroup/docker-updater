package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newTestChecker(t *testing.T, handler http.HandlerFunc) (*Checker, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Checker{
		Repo:      "owner/name",
		BaseURL:   srv.URL,
		CachePath: filepath.Join(t.TempDir(), "update-check.json"),
		TTL:       time.Hour,
		Client:    srv.Client(),
		Now:       time.Now,
	}, srv
}

func releaseJSON(tag string) string {
	return fmt.Sprintf(`{"tag_name":%q,"draft":false,"prerelease":false,"published_at":"2026-08-17T12:04:11Z"}`, tag)
}

func TestLatest(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseJSON("v1.4.0")))
	})
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Tag != "v1.4.0" {
		t.Errorf("tag = %q, want v1.4.0", rel.Tag)
	}
	if rel.PublishedAt.IsZero() {
		t.Error("published_at was not parsed")
	}
}

func TestLatestErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{"not found", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, ErrNoRelease},
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "4102444800")
			w.WriteHeader(http.StatusForbidden)
		}, ErrRateLimited},
		{"draft only", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":"v1.4.0","draft":true}`))
		}, ErrNoRelease},
		{"prerelease", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":"v1.4.0-rc.1","prerelease":true}`))
		}, ErrNoRelease},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestChecker(t, tc.handler)
			if _, err := c.Latest(context.Background()); !errors.Is(err, tc.want) {
				t.Errorf("Latest error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLatestRejectsJunkTag(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9; rm -rf /","draft":false,"prerelease":false}`))
	})
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("a tag that is not a version was accepted")
	}
}

func TestLatestRefusesOffHostRedirect(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseJSON("v9.9.9")))
	}))
	defer elsewhere.Close()

	c, _ := newTestChecker(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusMovedPermanently)
	})
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("a redirect to another host was followed")
	}
}

func TestCheckUsesCacheAndSkipsTheNetwork(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(releaseJSON("v1.4.0")))
	})

	if _, err := c.Check(context.Background(), "v1.3.0", false); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("first check made %d requests, want 1", got)
	}

	status, err := c.Check(context.Background(), "v1.3.0", false)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("a fresh cache still made %d requests, want 1", got)
	}
	if !status.FromCache {
		t.Error("second check did not report coming from cache")
	}
	if !status.Newer {
		t.Error("v1.4.0 should be newer than v1.3.0")
	}
}

func TestCheckForceIgnoresTheCache(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(releaseJSON("v1.4.0")))
	})
	for range 2 {
		if _, err := c.Check(context.Background(), "v1.3.0", true); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("forced checks made %d requests, want 2", got)
	}
}

func TestCheckStaleCacheRefetches(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(releaseJSON("v1.4.0")))
	})
	c.TTL = time.Nanosecond

	if _, err := c.Check(context.Background(), "v1.3.0", false); err != nil {
		t.Fatalf("first check: %v", err)
	}
	time.Sleep(2 * time.Nanosecond)
	if _, err := c.Check(context.Background(), "v1.3.0", false); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("stale cache made %d requests, want 2", got)
	}
}

func TestCheckUpToDateAndAhead(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseJSON("v1.4.0")))
	})
	for _, current := range []string{"v1.4.0", "v1.5.0"} {
		status, err := c.Check(context.Background(), current, true)
		if err != nil {
			t.Fatalf("check %s: %v", current, err)
		}
		if status.Newer {
			t.Errorf("current %s was reported as behind v1.4.0", current)
		}
	}
}

func TestCheckDevBuildMakesNoRequest(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	})
	if _, err := c.Check(context.Background(), "dev", true); !errors.Is(err, ErrDevBuild) {
		t.Errorf("error = %v, want ErrDevBuild", err)
	}
	if hits.Load() != 0 {
		t.Error("a dev build still called the API")
	}
}

func TestCheckDisabledMakesNoRequest(t *testing.T) {
	t.Setenv(DisableEnv, "1")
	var hits atomic.Int32
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	})
	if _, err := c.Check(context.Background(), "v1.3.0", true); !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want ErrDisabled", err)
	}
	if hits.Load() != 0 {
		t.Error("a disabled check still called the API")
	}
}

func TestRateLimitSuppressesFurtherRequests(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		w.WriteHeader(http.StatusForbidden)
	})

	if _, err := c.Check(context.Background(), "v1.3.0", false); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	if _, err := c.Check(context.Background(), "v1.3.0", false); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second error = %v, want ErrRateLimited", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("made %d requests while rate limited, want 1", got)
	}
}

func TestCacheRejectsATagThatIsNotAVersion(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {})
	write(t, c.CachePath, `{"release":{"tag":"v9.9.9; rm -rf /"},"checked_at":"2200-01-01T00:00:00Z"}`)
	if _, ok := c.readCache(); ok {
		t.Error("a cache holding a non-version tag was accepted")
	}
}

func TestCacheRejectsAnOversizedFile(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {})
	big := make([]byte, maxCacheFile+1)
	for i := range big {
		big[i] = 'x'
	}
	write(t, c.CachePath, string(big))
	if _, ok := c.readCache(); ok {
		t.Error("an oversized cache file was accepted")
	}
}

func TestCheckStillWorksWithAnUnwritableCache(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseJSON("v1.4.0")))
	})
	c.CachePath = "/proc/nonexistent/update-check.json"

	status, err := c.Check(context.Background(), "v1.3.0", false)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !status.Newer {
		t.Error("an unwritable cache changed the result")
	}
}

func TestNotesURLComesFromTheRepoNotTheWire(t *testing.T) {
	r := Release{Tag: "v1.4.0"}
	if got := r.NotesURL("owner/name"); got != "https://github.com/owner/name/releases/tag/v1.4.0" {
		t.Errorf("NotesURL = %q", got)
	}
	if got := r.NotesURL("not a repo"); got != "" {
		t.Errorf("NotesURL for an invalid repo = %q, want empty", got)
	}
}

func TestLatestRejectsAnInvalidRepo(t *testing.T) {
	c, _ := newTestChecker(t, func(w http.ResponseWriter, _ *http.Request) {})
	c.Repo = "../../etc/passwd"
	if _, err := c.Latest(context.Background()); err == nil {
		t.Error("an invalid repo was accepted")
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
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
