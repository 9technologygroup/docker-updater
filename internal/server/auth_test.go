package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func httptestRequest(method, path, body string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func recorderFor(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// One bad client must not lock everyone else out. A single global counter did
// exactly that: twenty bad requests a minute from anywhere blocked the lot.
func TestThrottleIsPerSource(t *testing.T) {
	s := newTestServer(t, "")
	bad := map[string]string{"Authorization": "Bearer wrongwrongwrongwrongwrongwrong00"}

	for range maxAuthFailures + 5 {
		req := httptestRequest(http.MethodGet, "/v1/targets", "", bad)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := recorderFor(s, req)
		_ = rec
	}

	req := httptestRequest(http.MethodGet, "/v1/targets", "", bearer())
	req.RemoteAddr = "198.51.100.4:6666"
	if got := recorderFor(s, req).Code; got != http.StatusOK {
		t.Fatalf("a different source got %d, want 200; the throttle is not per source", got)
	}

	req = httptestRequest(http.MethodGet, "/v1/targets", "", bearer())
	req.RemoteAddr = "203.0.113.9:5555"
	if got := recorderFor(s, req).Code; got != http.StatusTooManyRequests {
		t.Errorf("the offending source got %d, want 429", got)
	}
}
