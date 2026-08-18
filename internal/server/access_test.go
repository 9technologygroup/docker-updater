package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serverWith(t *testing.T, extraConfig string) *Server {
	t.Helper()
	return newTestServerWithConfig(t, "", extraConfig)
}

func requestFrom(t *testing.T, s *Server, remote string, headers map[string]string) int {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/targets", nil)
	req.RemoteAddr = remote
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestNoAllowListMeansEverySourceIsAccepted(t *testing.T) {
	s := serverWith(t, "")
	if code := requestFrom(t, s, "203.0.113.9:5000", nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when allow_from is unset", code)
	}
}

func TestAllowListRejectsOtherSources(t *testing.T) {
	s := serverWith(t, "allow_from: [\"10.0.0.0/8\", \"192.168.1.5\"]\n")

	cases := map[string]int{
		"10.4.5.6:5000":    http.StatusOK,
		"192.168.1.5:5000": http.StatusOK,
		"192.168.1.6:5000": http.StatusForbidden,
		"203.0.113.9:5000": http.StatusForbidden,
	}
	for remote, want := range cases {
		if code := requestFrom(t, s, remote, nil); code != want {
			t.Errorf("%s: status = %d, want %d", remote, code, want)
		}
	}
}

func TestForwardedForIsIgnoredFromAnUntrustedPeer(t *testing.T) {
	s := serverWith(t, "allow_from: [\"10.0.0.0/8\"]\n")

	code := requestFrom(t, s, "203.0.113.9:5000", map[string]string{"X-Forwarded-For": "10.1.2.3"})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; an untrusted peer must not be able to spoof its address with a header", code)
	}
}

func TestForwardedForIsHonouredFromATrustedProxy(t *testing.T) {
	s := serverWith(t, "allow_from: [\"10.0.0.0/8\"]\ntrusted_proxies: [\"127.0.0.1\"]\n")

	if code := requestFrom(t, s, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "10.1.2.3"}); code != http.StatusOK {
		t.Errorf("a trusted proxy forwarding an allowed client should be accepted, got %d", code)
	}
	if code := requestFrom(t, s, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.9"}); code != http.StatusForbidden {
		t.Errorf("a trusted proxy forwarding a disallowed client should be rejected, got %d", code)
	}
}

func TestForwardedForPicksTheClientNotTheProxyChain(t *testing.T) {
	s := serverWith(t, "allow_from: [\"10.0.0.0/8\"]\ntrusted_proxies: [\"127.0.0.1\", \"172.18.0.0/16\"]\n")

	code := requestFrom(t, s, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "10.1.2.3, 172.18.0.4"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the rightmost non-proxy hop is the real client", code)
	}

	code = requestFrom(t, s, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "10.1.2.3, 203.0.113.9"})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; a client may not prepend a fake hop to look allowed", code)
	}
}

func TestCORSIsOffByDefault(t *testing.T) {
	s := serverWith(t, "")

	req := httptest.NewRequest(http.MethodOptions, "/v1/targets/pmon/update", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty; a browser must not be able to use this API by default", got)
	}
	if rec.Code == http.StatusNoContent || rec.Code == http.StatusOK {
		t.Errorf("preflight returned %d; it must not succeed when no origins are configured", rec.Code)
	}
}

func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	s := serverWith(t, "cors:\n  allowed_origins: [\"https://dash.example.com\"]\n")

	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/v1/targets/pmon/update", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	ok := preflight("https://dash.example.com")
	if ok.Code != http.StatusNoContent {
		t.Errorf("allowed origin preflight = %d, want 204", ok.Code)
	}
	if got := ok.Header().Get("Access-Control-Allow-Origin"); got != "https://dash.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if !strings.Contains(ok.Header().Get("Vary"), "Origin") {
		t.Error("responses that depend on Origin must set Vary: Origin")
	}

	denied := preflight("https://evil.example.com")
	if denied.Code != http.StatusForbidden {
		t.Errorf("unlisted origin preflight = %d, want 403", denied.Code)
	}
	if got := denied.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin received Access-Control-Allow-Origin = %q", got)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"10.0.0.1:5000": "10.0.0.1",
		"[::1]:5000":    "::1",
		"10.0.0.1":      "10.0.0.1",
		"fe80::1%eth0":  "fe80::1%eth0",
		"[2001:db8::1]": "2001:db8::1",
		"2001:db8::1":   "2001:db8::1",
		"  10.0.0.2  ":  "10.0.0.2",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIPv6ForwardedForIsParsed(t *testing.T) {
	s := serverWith(t, "allow_from: [\"2001:db8::/32\"]\ntrusted_proxies: [\"127.0.0.1\"]\n")

	for _, forwarded := range []string{"2001:db8::5", "[2001:db8::5]"} {
		if code := requestFrom(t, s, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": forwarded}); code != http.StatusOK {
			t.Errorf("X-Forwarded-For %q: status = %d, want 200", forwarded, code)
		}
	}
	if code := requestFrom(t, s, "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "2001:dead::5"}); code != http.StatusForbidden {
		t.Errorf("an address outside the allowed v6 range should be rejected, got %d", code)
	}
}
