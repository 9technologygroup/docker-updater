package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/dockercfg"
)

func testAuthConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	body := "listen: \"127.0.0.1:0\"\n" +
		"agent_socket: /tmp/dup-cli-test-agent.sock\n" +
		"docker_config_dir: " + filepath.Join(dir, "docker") + "\n" +
		"auth:\n  bearer_token: " + strings.Repeat("x", 32) + "\n" +
		"targets:\n  - name: app\n    dir: " + filepath.Join(dir, "app") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadBasic(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// This is the one check.go/root.go cannot enforce on their own: even run as
// root, dup auth must not try to read a password from a pipe or a script.
func TestRunAuthAddRefusesNonTTYStdin(t *testing.T) {
	cfg := testAuthConfig(t)
	err := runAuthAdd(cfg, "app", false)
	if err == nil || !strings.Contains(err.Error(), "not a TTY") {
		t.Fatalf("runAuthAdd = %v, want a not-a-TTY refusal", err)
	}
}

func TestRunAuthRemoveNeedsAStack(t *testing.T) {
	cfg := testAuthConfig(t)
	if err := runAuthRemove(cfg, "", "harbor.example.com"); err == nil {
		t.Fatal("runAuthRemove with no stack was accepted")
	}
}

func TestRunAuthRemoveUnknownStack(t *testing.T) {
	cfg := testAuthConfig(t)
	if err := runAuthRemove(cfg, "nosuch", "harbor.example.com"); err == nil {
		t.Fatal("runAuthRemove for an unknown stack was accepted")
	}
}

func TestRunAuthListEmptyStoreIsNotAnError(t *testing.T) {
	cfg := testAuthConfig(t)
	if err := runAuthList(cfg, ""); err != nil {
		t.Fatalf("runAuthList on an empty store = %v, want nil", err)
	}
}

func TestRunAuthSetReadRemoveRoundTrip(t *testing.T) {
	cfg := testAuthConfig(t)
	tgt, ok := cfg.Target("app")
	if !ok {
		t.Fatal("test config has no app target")
	}

	store, err := dockercfg.Read(tgt.DockerConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("harbor.example.com", "robot$app", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(tgt.DockerConfigDir()); err != nil {
		t.Fatal(err)
	}
	if !tgt.HasDockerConfig() {
		t.Error("HasDockerConfig is false right after writing a store")
	}

	if err := runAuthList(cfg, "app"); err != nil {
		t.Fatalf("runAuthList after seeding a credential = %v, want nil", err)
	}
	if err := runAuthRemove(cfg, "app", "harbor.example.com"); err != nil {
		t.Fatalf("runAuthRemove = %v, want nil", err)
	}
	if dockercfg.Exists(tgt.DockerConfigDir()) {
		t.Error("the credential store file survived removing its only host")
	}
}

func TestPromptLineTrimsInput(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("  robot$app-pull  \n"))
	got, err := promptLine(r, "username: ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "robot$app-pull" {
		t.Errorf("promptLine = %q, want %q", got, "robot$app-pull")
	}
}

func TestAuthArgsSuffixPreservesWhatWasTyped(t *testing.T) {
	cases := []struct {
		target string
		list   bool
		remove string
		force  bool
		want   string
	}{
		{want: ""},
		{target: "app", want: " app"},
		{target: "app", force: true, want: " app --force"},
		{list: true, want: " --list"},
		{target: "app", remove: "harbor.example.com", want: " app --remove harbor.example.com"},
	}
	for _, c := range cases {
		if got := authArgsSuffix(c.target, c.list, c.remove, c.force); got != c.want {
			t.Errorf("authArgsSuffix(%q,%v,%q,%v) = %q, want %q", c.target, c.list, c.remove, c.force, got, c.want)
		}
	}
}

func authTestTarget(t *testing.T) (*config.Config, *config.Target) {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, "config.yml")
	body := "listen: \"127.0.0.1:7788\"\nagent_peer_user: dup\nlog_file: none\nhistory_file: none\n" +
		"docker_config_dir: " + filepath.Join(base, "docker") + "\n" +
		"targets:\n  - name: patchmon\n    dir: " + base + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadBasic(path)
	if err != nil {
		t.Fatal(err)
	}
	tgt, ok := cfg.Target("patchmon")
	if !ok {
		t.Fatal("target patchmon missing from the loaded config")
	}
	return cfg, tgt
}

// A public registry needs no credentials, so pressing enter past it is a choice
// rather than a failure, and must not make the command exit non-zero.
func TestAuthTreatsABlankUsernameAsLeavingTheRegistryAlone(t *testing.T) {
	cfg, tgt := authTestTarget(t)

	tally, err := authTarget(cfg, tgt, false, bufio.NewReader(strings.NewReader("ghcr.io\n\n\n")))
	if err != nil {
		t.Fatalf("authTarget: %v", err)
	}
	if tally.failed != 0 {
		t.Errorf("failed = %d, want a blank username to count as skipped: %+v", tally.failed, tally)
	}
	if tally.skipped != 1 || tally.saved != 0 {
		t.Errorf("tally = %+v, want exactly one skip", tally)
	}
}

func TestAuthLeavesAHostThatAlreadyHasCredentialsAlone(t *testing.T) {
	cfg, tgt := authTestTarget(t)

	store := dockercfg.New()
	if err := store.Set("cr.dev.patchmon.cloud", "robot$patchmon", strings.Repeat("s", 32)); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(tgt.DockerConfigDir()); err != nil {
		t.Fatal(err)
	}

	tally, err := authTarget(cfg, tgt, false, bufio.NewReader(strings.NewReader("cr.dev.patchmon.cloud\n")))
	if err != nil {
		t.Fatalf("authTarget: %v", err)
	}
	if tally.failed != 0 || tally.skipped != 1 || tally.saved != 0 {
		t.Errorf("tally = %+v, want one skip and no failure", tally)
	}
}

func TestAuthTallySummaryReadsAsEnglish(t *testing.T) {
	cases := []struct {
		in   authTally
		want string
	}{
		{authTally{}, "nothing to do"},
		{authTally{saved: 1}, "stored 1"},
		{authTally{saved: 1, skipped: 2}, "stored 1, left 2 alone"},
		{authTally{saved: 0, skipped: 1, failed: 1}, "stored 0, left 1 alone, could not store 1"},
	}
	for _, c := range cases {
		if got := c.in.summary(); got != c.want {
			t.Errorf("summary(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}
