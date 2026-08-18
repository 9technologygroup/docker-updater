package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/selfupdate"
)

const (
	testToken  = "0123456789abcdef0123456789abcdef"
	testGHSeal = "fedcba9876543210fedcba9876543210"
)

func newTestServer(t *testing.T, extraTarget string) *Server {
	t.Helper()
	return newTestServerWithConfig(t, extraTarget, "")
}

func newTestServerWithConfig(t *testing.T, extraTarget, extraTop string) *Server {
	t.Helper()

	stack := t.TempDir()
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := extraTop +
		"auth:\n  bearer_token: " + testToken + "\n  github_secret: " + testGHSeal + "\n" +
		"targets:\n  - name: pmon\n    dir: " + stack + "\n" + extraTarget

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	exec := job.NewManager(stubBackend{}, job.NewStore(), nil, log)
	return New(cfg, exec, log, "testhost", "test")
}

func do(t *testing.T, s *Server, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}

func TestHealthNeedsNoAuth(t *testing.T) {
	rec := do(t, newTestServer(t, ""), http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuthRejection(t *testing.T) {
	s := newTestServer(t, "")

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no header", nil},
		{"wrong token", map[string]string{"Authorization": "Bearer wrongwrongwrongwrongwrongwrong00"}},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}},
		{"basic auth", map[string]string{"Authorization": "Basic " + testToken}},
		{"token as query", nil},
		{"bad signature", map[string]string{"X-Hub-Signature-256": "sha256=deadbeef"}},
		{"signature not hex", map[string]string{"X-Hub-Signature-256": "sha256=zzzz"}},
		{"signature wrong prefix", map[string]string{"X-Hub-Signature-256": "sha1=" + strings.Repeat("a", 40)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, "/v1/targets", "", tc.headers)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestBearerTokenIsAcceptedAndCaseInsensitiveScheme(t *testing.T) {
	s := newTestServer(t, "")
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		rec := do(t, s, http.MethodGet, "/v1/targets", "", map[string]string{"Authorization": scheme + " " + testToken})
		if rec.Code != http.StatusOK {
			t.Fatalf("scheme %q: status = %d, want 200", scheme, rec.Code)
		}
	}
}

func TestGitHubSignatureIsVerifiedOverBody(t *testing.T) {
	s := newTestServer(t, "")
	body := `{"zen":"hello"}`

	mac := hmac.New(sha256.New, []byte(testGHSeal))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	rec := do(t, s, http.MethodPost, "/v1/targets/pmon/update", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "ping",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("body = %s, want a ping to be ignored", rec.Body.String())
	}

	rec = do(t, s, http.MethodPost, "/v1/targets/pmon/update", body+" ", map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "ping",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: status = %d, want 401", rec.Code)
	}
}

func TestUnknownTargetIs404(t *testing.T) {
	s := newTestServer(t, "")
	for _, path := range []string{"/v1/targets/nope/update", "/v1/targets/..%2F..%2Fetc/update"} {
		rec := do(t, s, http.MethodPost, path, "", bearer())
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestTagIsRejectedWhenTargetHasNoImageTagEnv(t *testing.T) {
	s := newTestServer(t, "")
	rec := do(t, s, http.MethodPost, "/v1/targets/pmon/update", `{"tag":"2.0.4"}`, bearer())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not accept a tag") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestUnknownBodyFieldIsRejected(t *testing.T) {
	s := newTestServer(t, "")
	rec := do(t, s, http.MethodPost, "/v1/targets/pmon/update", `{"command":"rm -rf /"}`, bearer())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestGitHubEventFiltering(t *testing.T) {
	stack := t.TempDir()
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, "    image_tag_env: APP_VERSION\n")
	target, _ := s.cfg.Target("pmon")

	cases := []struct {
		name        string
		event       string
		body        string
		wantIgnored bool
		wantTag     string
	}{
		{"ping", "ping", `{"zen":"x"}`, true, ""},
		{"push", "push", `{}`, true, ""},
		{"release created", "release", `{"action":"created","release":{"tag_name":"v2.0.4"}}`, true, ""},
		{"draft", "release", `{"action":"published","release":{"tag_name":"v2.0.4","draft":true}}`, true, ""},
		{"prerelease", "release", `{"action":"published","release":{"tag_name":"v2.0.4-rc.1","prerelease":true}}`, true, ""},
		{"published", "release", `{"action":"published","release":{"tag_name":"v2.0.4"},"repository":{"full_name":"acme/web"}}`, false, "2.0.4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ignored, err := s.githubRequest(target, tc.event, []byte(tc.body))
			if err != nil {
				t.Fatalf("githubRequest: %v", err)
			}
			if tc.wantIgnored != (ignored != "") {
				t.Fatalf("ignored = %q, wantIgnored = %v", ignored, tc.wantIgnored)
			}
			if req.Tag != tc.wantTag {
				t.Errorf("tag = %q, want %q", req.Tag, tc.wantTag)
			}
		})
	}
}

func TestGitHubPrereleaseAllowedWhenConfigured(t *testing.T) {
	s := newTestServer(t, "    image_tag_env: APP_VERSION\n    allow_prerelease: true\n")
	target, _ := s.cfg.Target("pmon")

	req, ignored, err := s.githubRequest(target, "release",
		[]byte(`{"action":"published","release":{"tag_name":"v2.0.4-rc.1","prerelease":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ignored != "" {
		t.Fatalf("ignored = %q, want it to proceed", ignored)
	}
	if req.Tag != "2.0.4-rc.1" {
		t.Errorf("tag = %q, want 2.0.4-rc.1", req.Tag)
	}
}

func TestGitHubMaliciousTagIsRejected(t *testing.T) {
	s := newTestServer(t, "    image_tag_env: APP_VERSION\n")
	target, _ := s.cfg.Target("pmon")

	_, _, err := s.githubRequest(target, "release",
		[]byte(`{"action":"published","release":{"tag_name":"v2.0.4 --volumes"}}`))
	if err == nil {
		t.Fatal("expected a rejection for a tag containing a flag")
	}
}

func TestListTargetsDoesNotLeakSecrets(t *testing.T) {
	s := newTestServer(t, "")
	rec := do(t, s, http.MethodGet, "/v1/targets", "", bearer())
	body := rec.Body.String()
	if strings.Contains(body, testToken) || strings.Contains(body, testGHSeal) {
		t.Fatalf("target listing leaked a secret: %s", body)
	}

	var parsed struct {
		Targets []struct {
			Name string `json:"name"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Targets) != 1 || parsed.Targets[0].Name != "pmon" {
		t.Errorf("targets = %+v", parsed.Targets)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	s := newTestServer(t, "")
	rec := do(t, s, http.MethodPost, "/v1/targets/pmon/update", strings.Repeat("a", config.MaxBodyBytes+1024), bearer())
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestParseWait(t *testing.T) {
	cases := map[string]time.Duration{
		"":       0,
		"30s":    30 * time.Second,
		"90":     90 * time.Second,
		"2m":     2 * time.Minute,
		"99h":    MaxWait,
		"-5s":    0,
		"banana": 0,
	}
	for in, want := range cases {
		if got := parseWait(in); got != want {
			t.Errorf("parseWait(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSecretsEqual(t *testing.T) {
	if !secretsEqual([]byte(testToken), []byte(testToken)) {
		t.Error("identical secrets should compare equal")
	}
	if secretsEqual([]byte(testToken), []byte(testToken+"x")) {
		t.Error("different-length secrets should not compare equal")
	}
	if secretsEqual([]byte(testToken), []byte("")) {
		t.Error("empty secret should not match")
	}
}

func TestAuthFailuresAreThrottledWithoutBlocking(t *testing.T) {
	s := newTestServer(t, "")
	bad := map[string]string{"Authorization": "Bearer wrongwrongwrongwrongwrongwrong00"}

	start := time.Now()
	var got429 bool
	for i := range maxAuthFailures + 5 {
		rec := do(t, s, http.MethodGet, "/v1/targets", "", bad)
		switch rec.Code {
		case http.StatusUnauthorized:
		case http.StatusTooManyRequests:
			got429 = true
		default:
			t.Fatalf("request %d: status = %d, want 401 or 429", i, rec.Code)
		}
	}
	elapsed := time.Since(start)

	if !got429 {
		t.Errorf("after %d failures the server should start returning 429", maxAuthFailures)
	}
	if elapsed > 2*time.Second {
		t.Errorf("rejecting %d bad requests took %s; auth rejection must not sleep and pin goroutines", maxAuthFailures+5, elapsed)
	}
}

func TestValidTokenDoesNotResetTheFailureBudget(t *testing.T) {
	s := newTestServer(t, "")
	bad := map[string]string{"Authorization": "Bearer wrongwrongwrongwrongwrongwrong00"}

	for range maxAuthFailures - 1 {
		do(t, s, http.MethodGet, "/v1/targets", "", bad)
	}
	if rec := do(t, s, http.MethodGet, "/v1/targets", "", bearer()); rec.Code != http.StatusOK {
		t.Fatalf("a valid token should still work below the limit, got %d", rec.Code)
	}

	for range 5 {
		do(t, s, http.MethodGet, "/v1/targets", "", bad)
	}
	if rec := do(t, s, http.MethodGet, "/v1/targets", "", bad); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429; a successful auth must not clear the failure budget", rec.Code)
	}
}

func TestHealthzDoesNotLeakHostDetails(t *testing.T) {
	s := newTestServer(t, "")
	body := do(t, s, http.MethodGet, "/healthz", "", nil).Body.String()

	for _, leak := range []string{"testhost", "version"} {
		if strings.Contains(body, leak) {
			t.Errorf("unauthenticated /healthz leaked %q: %s", leak, body)
		}
	}
}

type stubBackend struct{}

func (stubBackend) Update(context.Context, *config.Target, job.Request, job.Sink) (job.State, string, error) {
	return job.StateSucceeded, "stub", nil
}

// The version endpoint reads a cache and never calls out, so an authenticated
// caller cannot drive requests to GitHub by polling it.
func TestVersionEndpointMakesNoNetworkCall(t *testing.T) {
	s := newTestServer(t, "").WithBuild("abc1234").
		WithUpdateStatus(func() (selfupdate.Status, bool) {
			return selfupdate.Status{
				Latest:    selfupdate.Release{Tag: "v1.4.0", PublishedAt: time.Unix(1755432251, 0)},
				Newer:     true,
				CheckedAt: time.Unix(1755432251, 0),
			}, true
		})

	rec := do(t, s, http.MethodGet, "/v1/version", "", map[string]string{"Authorization": "Bearer " + testToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"latest":"v1.4.0"`, `"update_available":true`, `"commit":"abc1234"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestVersionEndpointRequiresAuth(t *testing.T) {
	rec := do(t, newTestServer(t, ""), http.MethodGet, "/v1/version", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestVersionEndpointOmitsLatestWhenTheCacheIsEmpty(t *testing.T) {
	s := newTestServer(t, "").WithUpdateStatus(func() (selfupdate.Status, bool) {
		return selfupdate.Status{}, false
	})
	rec := do(t, s, http.MethodGet, "/v1/version", "", map[string]string{"Authorization": "Bearer " + testToken})
	if strings.Contains(rec.Body.String(), "latest") {
		t.Errorf("latest should be omitted with no cache: %s", rec.Body.String())
	}
}

func TestTargetsReportPendingSoak(t *testing.T) {
	since := time.Now().Add(-5 * time.Minute)
	s := newTestServer(t, "").WithPending(func(string) (time.Time, []string, bool) {
		return since, []string{"web"}, true
	})
	rec := do(t, s, http.MethodGet, "/v1/targets", "", map[string]string{"Authorization": "Bearer " + testToken})
	if !strings.Contains(rec.Body.String(), "pending_since") {
		t.Errorf("targets did not report the soak state: %s", rec.Body.String())
	}
}
