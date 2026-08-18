package cli

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/config"
)

func TestAgentUnreachableSaysTheAgentIsNotRunning(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "agent.sock")

	got := agentUnreachable(socket, errors.New("dial unix: no such file or directory"))

	if !strings.Contains(got, "not running") {
		t.Errorf("message should say the agent is not running, got:\n%s", got)
	}
	if !strings.Contains(got, "systemctl enable --now dup-agent") {
		t.Errorf("message must give the command to start it, got:\n%s", got)
	}
	if strings.Contains(got, "Run this as root") {
		t.Error("must not blame permissions when the socket simply does not exist")
	}
}

func TestAgentUnreachableDetectsAStaleSocket(t *testing.T) {
	// t.TempDir() paths blow the ~104 byte sun_path limit, so use a short one.
	dir, err := os.MkdirTemp("", "dup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("cannot create a unix socket: %v", err)
	}
	// Closing the listener normally unlinks it, so keep the file in place.
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	_ = listener.Close()

	got := agentUnreachable(socket, syscall.ECONNREFUSED)

	if !strings.Contains(got, "stale") {
		t.Errorf("a socket file with nothing listening is stale, got:\n%s", got)
	}
	if !strings.Contains(got, "systemctl restart dup-agent") {
		t.Errorf("message must give the restart command, got:\n%s", got)
	}
}

func TestAgentUnreachableDetectsAWrongFileType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := agentUnreachable(path, errors.New("connection refused"))

	if !strings.Contains(got, "not a socket") {
		t.Errorf("should point out the path is not a socket, got:\n%s", got)
	}
	if !strings.Contains(got, "rm "+path) {
		t.Errorf("should say how to clear it, got:\n%s", got)
	}
}

func TestAPIUnreachableExplainsHowToStartIt(t *testing.T) {
	err := apiUnreachable("http://127.0.0.1:7788", syscall.ECONNREFUSED)

	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("got: %v", err)
	}
	if !strings.Contains(err.Error(), "systemctl enable --now dup-agent dup") {
		t.Errorf("must give the start command, got: %v", err)
	}
}

func TestAPIUnreachablePassesThroughOtherErrors(t *testing.T) {
	err := apiUnreachable("http://127.0.0.1:7788", errors.New("tls handshake timeout"))

	if !strings.Contains(err.Error(), "tls handshake timeout") {
		t.Errorf("an unrelated error must not be rewritten into a start-the-service message, got: %v", err)
	}
	if strings.Contains(err.Error(), "systemctl") {
		t.Errorf("got: %v", err)
	}
}

func TestAPIURLReflectsTLS(t *testing.T) {
	plain := &config.Config{Listen: "127.0.0.1:7788"}
	if got := apiURL(plain); got != "http://127.0.0.1:7788" {
		t.Errorf("apiURL = %q, want http", got)
	}

	secure := &config.Config{Listen: "127.0.0.1:7788", TLS: config.TLS{SelfSigned: true}}
	if got := apiURL(secure); got != "https://127.0.0.1:7788" {
		t.Errorf("apiURL = %q, want https; dup list showing http while dup check showed https is what caused the confusion", got)
	}

	byoCert := &config.Config{Listen: "0.0.0.0:443", TLS: config.TLS{CertFile: "/a.crt", KeyFile: "/a.key"}}
	if got := apiURL(byoCert); got != "https://0.0.0.0:443" {
		t.Errorf("apiURL = %q, want https for a supplied certificate", got)
	}
}
