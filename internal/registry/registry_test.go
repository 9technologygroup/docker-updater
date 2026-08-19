package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testPassword = "correct-horse-battery-staple"

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	return strings.TrimPrefix(rawURL, "https://")
}

// bearerRegistry answers the way Harbor does: a 401 with a token realm, then a
// token endpoint that checks the credentials with HTTP basic auth.
func bearerRegistry(t *testing.T, wantUser, wantPass string) *httptest.Server {
	t.Helper()

	var realm string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="harbor-registry",scope="registry:catalog:*"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/service/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != wantUser || pass != wantPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("service") != "harbor-registry" {
			t.Errorf("token request did not carry the service from the challenge: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"issued","expires_in":300}`))
	})

	srv := httptest.NewTLSServer(mux)
	realm = srv.URL + "/service/token"
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyAcceptsBearerCredentials(t *testing.T) {
	srv := bearerRegistry(t, "robot$pmon", testPassword)
	c := &Client{HTTP: srv.Client()}

	if err := c.Verify(context.Background(), hostOf(t, srv.URL), "robot$pmon", testPassword); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsBadBearerCredentials(t *testing.T) {
	srv := bearerRegistry(t, "robot$pmon", testPassword)
	c := &Client{HTTP: srv.Client()}

	err := c.Verify(context.Background(), hostOf(t, srv.URL), "robot$pmon", "wrong")
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("a rejection must not be reported as the registry being unreachable")
	}
}

func TestVerifyHandlesPlainBasicAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if user != "ops" || pass != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	if err := c.Verify(context.Background(), hostOf(t, srv.URL), "ops", testPassword); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := c.Verify(context.Background(), hostOf(t, srv.URL), "ops", "wrong"); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

func TestVerifyAcceptsARegistryThatAsksForNothing(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	if err := c.Verify(context.Background(), hostOf(t, srv.URL), "ops", testPassword); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyReportsAnUnreachableRegistrySeparately(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	host := hostOf(t, srv.URL)
	client := srv.Client()
	srv.Close()

	c := &Client{HTTP: client, Timeout: 2 * time.Second}
	err := c.Verify(context.Background(), host, "ops", testPassword)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if errors.Is(err, ErrRejected) {
		t.Error("failing to reach the registry is not the same as being rejected by it")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("the password reached an error string")
	}
}

func TestVerifyRefusesARedirectToAnotherHost(t *testing.T) {
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v2/", http.StatusFound)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	err := c.Verify(context.Background(), hostOf(t, srv.URL), "ops", testPassword)
	if err == nil {
		t.Fatal("a redirect onto another host must not be followed")
	}
	if !strings.Contains(err.Error(), "refusing a redirect") {
		t.Fatalf("err = %v, want the redirect refusal", err)
	}
}

func TestVerifyRefusesAPlaintextTokenRealm(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://auth.example.com/token",service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	err := c.Verify(context.Background(), hostOf(t, srv.URL), "ops", testPassword)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestNoErrorPathCarriesThePassword(t *testing.T) {
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	c := &Client{HTTP: bad.Client()}
	for _, host := range []string{hostOf(t, bad.URL), "127.0.0.1:1"} {
		err := c.Verify(context.Background(), host, "ops", testPassword)
		if err == nil {
			t.Fatalf("%s: expected an error", host)
		}
		if strings.Contains(err.Error(), testPassword) {
			t.Fatalf("%s: the password reached an error string: %v", host, err)
		}
	}
}

func TestDefaultTransportVerifiesTLS(t *testing.T) {
	c := &Client{}
	transport, ok := c.client("harbor.example.com").Transport.(*http.Transport)
	if !ok {
		t.Fatal("the default client should carry its own transport")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("certificate verification must stay on")
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS 1.2", transport.TLSClientConfig.MinVersion)
	}
	if c.client("harbor.example.com").Timeout != defaultTimeout {
		t.Error("the default client should carry a timeout")
	}
}

func TestVerifyRequiresBothHalvesOfTheCredentials(t *testing.T) {
	c := &Client{}
	ctx := context.Background()
	if err := c.Verify(ctx, "", "ops", testPassword); err == nil {
		t.Error("an empty host must be refused before anything is sent")
	}
	if err := c.Verify(ctx, "harbor.example.com", "", testPassword); err == nil {
		t.Error("an empty username must be refused before anything is sent")
	}
	if err := c.Verify(ctx, "harbor.example.com", "ops", ""); err == nil {
		t.Error("an empty password must be refused before anything is sent")
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.example.com/token",service="harbor,registry",scope="repository:lib/app:pull"`)
	if scheme != "Bearer" {
		t.Errorf("scheme = %q, want Bearer", scheme)
	}
	want := map[string]string{
		"realm":   "https://auth.example.com/token",
		"service": "harbor,registry",
		"scope":   "repository:lib/app:pull",
	}
	for key, value := range want {
		if params[key] != value {
			t.Errorf("params[%q] = %q, want %q", key, params[key], value)
		}
	}

	if scheme, _ := parseChallenge(`Basic realm="registry"`); scheme != "Basic" {
		t.Errorf("scheme = %q, want Basic", scheme)
	}
	if scheme, params := parseChallenge("  "); scheme != "" || params != nil {
		t.Error("an absent challenge should parse to nothing")
	}
}
