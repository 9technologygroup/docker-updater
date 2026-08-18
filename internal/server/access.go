package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func (s *Server) clientIP(r *http.Request) (netip.Addr, bool) {
	peer, err := netip.ParseAddr(hostOnly(r.RemoteAddr))
	if err != nil {
		return netip.Addr{}, false
	}
	peer = normalise(peer)

	trusted := s.cfg.TrustedProxyPrefixes()
	if len(trusted) == 0 || !inAny(peer, trusted) {
		return peer, true
	}

	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer, true
	}

	var chain []string
	for _, header := range forwarded {
		for _, part := range strings.Split(header, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}

	for i := len(chain) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hostOnly(chain[i]))
		if err != nil {
			continue
		}
		addr = normalise(addr)
		if inAny(addr, trusted) {
			continue
		}
		return addr, true
	}
	return peer, true
}

func (s *Server) allowedSource(r *http.Request) (netip.Addr, bool) {
	allow := s.cfg.AllowFromPrefixes()
	addr, ok := s.clientIP(r)
	if len(allow) == 0 {
		return addr, true
	}
	if !ok {
		return addr, false
	}
	return addr, inAny(addr, allow)
}

func (s *Server) restrictSource(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, ok := s.allowedSource(r)
		if !ok {
			s.log.Warn("rejected request from a source that is not in allow_from",
				"source", addr.String(), "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "this source address is not permitted")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	allowed := s.cfg.CORS.AllowedOrigins

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Add("Vary", "Origin")
		if !originAllowed(origin, allowed) {
			if r.Method == http.MethodOptions {
				writeError(w, http.StatusForbidden, "cross-origin requests are not enabled for this origin")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

func inAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Addr().Is4() != addr.Is4() {
			continue
		}
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func hostOnly(v string) string {
	v = strings.TrimSpace(v)
	if host, _, err := net.SplitHostPort(v); err == nil {
		return host
	}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		return v[1 : len(v)-1]
	}
	return v
}

func normalise(addr netip.Addr) netip.Addr { return addr.Unmap().WithZone("") }
