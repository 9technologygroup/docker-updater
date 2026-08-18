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
)

type authThrottle struct {
	mu          sync.Mutex
	failures    int
	windowStart time.Time
}

func (a *authThrottle) allow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	if now.Sub(a.windowStart) > authWindow {
		a.windowStart = now
		a.failures = 0
	}
	return a.failures < maxAuthFailures
}

func (a *authThrottle) fail() {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	if now.Sub(a.windowStart) > authWindow {
		a.windowStart = now
		a.failures = 0
	}
	a.failures++
}

func (s *Server) authenticate(r *http.Request, body []byte) (string, bool) {
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		secret := s.cfg.GitHubSecret()
		if len(secret) == 0 || !verifySignature(secret, body, sig) {
			s.throttle.fail()
			return "", false
		}
		return "github", true
	}

	token := s.cfg.BearerToken()
	presented := bearerToken(r.Header.Get("Authorization"))
	if len(token) == 0 || presented == "" || !secretsEqual(token, []byte(presented)) {
		s.throttle.fail()
		return "", false
	}
	return "api", true
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
