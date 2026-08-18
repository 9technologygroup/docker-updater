package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	authWindow      = time.Minute
	maxAuthFailures = 20
	maxThrottleKeys = 4096
)

type throttleEntry struct {
	failures    int
	windowStart time.Time
}

// authThrottle counts failures per source address. A single global counter would
// let one bad client lock every legitimate caller out for the rest of the window.
type authThrottle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
}

func (a *authThrottle) entry(key string, now time.Time) *throttleEntry {
	if a.entries == nil {
		a.entries = make(map[string]*throttleEntry)
	}
	e, ok := a.entries[key]
	if !ok {
		// Bounded so a flood of spoofed sources cannot grow the map without limit.
		if len(a.entries) >= maxThrottleKeys {
			a.sweep(now)
		}
		if len(a.entries) >= maxThrottleKeys {
			a.entries = make(map[string]*throttleEntry)
		}
		e = &throttleEntry{windowStart: now}
		a.entries[key] = e
	}
	if now.Sub(e.windowStart) > authWindow {
		e.windowStart = now
		e.failures = 0
	}
	return e
}

func (a *authThrottle) sweep(now time.Time) {
	for k, e := range a.entries {
		if now.Sub(e.windowStart) > authWindow {
			delete(a.entries, k)
		}
	}
}

func (a *authThrottle) allow(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entry(key, time.Now()).failures < maxAuthFailures
}

func (a *authThrottle) fail(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entry(key, time.Now()).failures++
}

func (s *Server) authenticate(r *http.Request, body []byte) (string, bool) {
	key := s.throttleKey(r)

	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		secret := s.cfg.GitHubSecret()
		if len(secret) == 0 || !verifySignature(secret, body, sig) {
			s.throttle.fail(key)
			return "", false
		}
		return "github", true
	}

	token := s.cfg.BearerToken()
	presented := bearerToken(r.Header.Get("Authorization"))
	if len(token) == 0 || presented == "" || !secretsEqual(token, []byte(presented)) {
		s.throttle.fail(key)
		return "", false
	}
	return "api", true
}

func (s *Server) throttleKey(r *http.Request) string {
	if addr, ok := s.clientIP(r); ok {
		return addr.String()
	}
	return "unknown"
}

func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func secretsEqual(a, b []byte) bool {
	ah := sha256.Sum256(a)
	bh := sha256.Sum256(b)
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func verifySignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil || len(want) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}
