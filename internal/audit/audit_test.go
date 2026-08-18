package audit

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/config"
)

func currentIdentity(t *testing.T) *Identity {
	t.Helper()

	u, err := user.Current()
	if err != nil {
		t.Skip("cannot resolve current user")
	}
	id, err := LookupIdentity(u.Username)
	if err != nil {
		t.Skipf("cannot look up %q: %v", u.Username, err)
	}
	return id
}

func configWithTarget(t *testing.T, dir string) *config.Config {
	t.Helper()

	body := "auth:\n  bearer_token: 0123456789abcdef0123456789abcdef\n" +
		"targets:\n  - name: a\n    dir: " + dir + "\n    compose_file: docker-compose.yml\n"

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestAuditFlagsAComposeFileTheServiceUserCanRewrite(t *testing.T) {
	id := currentIdentity(t)

	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := filepath.EvalSymlinks(compose)
	if err != nil {
		t.Fatal(err)
	}

	findings := Run(configWithTarget(t, dir), id)
	if len(findings) == 0 {
		t.Fatal("a compose file owned and writable by the running user must be flagged")
	}

	var sawCompose bool
	for _, f := range findings {
		if f.Path == compose || f.Path == resolved {
			sawCompose = true
			if !strings.Contains(f.Reason, "writable") {
				t.Errorf("reason = %q, want it to explain writability", f.Reason)
			}
		}
	}
	if !sawCompose {
		t.Errorf("compose file was not among the findings: %+v", findings)
	}
}

func TestAuditFlagsWorldWritablePaths(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(compose, 0o666); err != nil {
		t.Fatal(err)
	}

	nobody := &Identity{Name: "nobody", UID: 999999, GIDs: map[uint32]bool{}}
	findings := Run(configWithTarget(t, dir), nobody)

	var sawCompose bool
	for _, f := range findings {
		if strings.HasSuffix(f.Path, "docker-compose.yml") && strings.Contains(f.Reason, "world writable") {
			sawCompose = true
		}
	}
	if !sawCompose {
		t.Errorf("a world writable compose file must be flagged even for an unrelated user: %+v", findings)
	}
}

func TestAuditPassesForRootOwnedTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, every path is writable by definition")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	nobody := &Identity{Name: "nobody", UID: 999999, GIDs: map[uint32]bool{}}
	if findings := Run(configWithTarget(t, dir), nobody); len(findings) != 0 {
		t.Errorf("a tree the user does not own should pass, got %+v", findings)
	}
}

func TestWithAncestorsReachesRoot(t *testing.T) {
	got := withAncestors("/opt/acme/web/docker-compose.yml")
	want := []string{
		"/opt/acme/web/docker-compose.yml",
		"/opt/acme/web",
		"/opt/acme",
		"/opt",
		"/",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLookupIdentityRejectsUnknownUser(t *testing.T) {
	if _, err := LookupIdentity("definitely-not-a-real-user-" + strconv.Itoa(os.Getpid())); err == nil {
		t.Fatal("expected an error for an unknown user")
	}
}

func TestAuditChecksTheImplicitDotEnvEvenWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dotenv := filepath.Join(dir, ".env")
	if err := os.WriteFile(dotenv, []byte("TAG=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dotenv, 0o666); err != nil {
		t.Fatal(err)
	}

	nobody := &Identity{Name: "nobody", UID: 999999, GIDs: map[uint32]bool{}}
	findings := Run(configWithTarget(t, dir), nobody)

	for _, f := range findings {
		if strings.HasSuffix(f.Path, "/.env") {
			return
		}
	}
	t.Errorf("a writable .env must be flagged: compose reads it unconditionally and it can set COMPOSE_FILE, got %+v", findings)
}

func TestAuditCoversThePreUpdateHookCommand(t *testing.T) {
	id := currentIdentity(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(t.TempDir(), "backup.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	body := "auth:\n  bearer_token: 0123456789abcdef0123456789abcdef\n" +
		"targets:\n  - name: a\n    dir: " + dir + "\n    compose_file: docker-compose.yml\n" +
		"    pre_update:\n      command: " + hook + "\n"
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	resolved, _ := filepath.EvalSymlinks(hook)
	for _, f := range Run(cfg, id) {
		if f.Path == hook || f.Path == resolved {
			return
		}
	}
	t.Error("a pre-update hook the service account can rewrite must be flagged: it runs as root")
}

func TestStickyDirectoriesAreNotTreatedAsWritable(t *testing.T) {
	dir := t.TempDir()
	sticky := filepath.Join(dir, "sticky")
	if err := os.Mkdir(sticky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}

	nobody := &Identity{Name: "nobody", UID: 999999, GIDs: map[uint32]bool{}}
	if reason, bad := writableBy(sticky, nobody); bad {
		t.Errorf("a sticky world-writable directory must not be flagged (%s); only the owner of an entry can replace it", reason)
	}

	plain := filepath.Join(dir, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, bad := writableBy(plain, nobody); !bad {
		t.Error("a world-writable directory without the sticky bit must still be flagged")
	}
}

func TestStickyDoesNotExcuseAnOwnedDirectory(t *testing.T) {
	id := currentIdentity(t)

	dir := t.TempDir()
	owned := filepath.Join(dir, "owned")
	if err := os.Mkdir(owned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(owned, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}

	if _, bad := writableBy(owned, id); !bad {
		t.Error("the owner can clear the sticky bit, so a directory the service account owns is still writable")
	}
}
