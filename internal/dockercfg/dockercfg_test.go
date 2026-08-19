package dockercfg

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteThenReadRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docker", "pmon")

	cfg := New()
	if err := cfg.Set("harbor.example.com", "robot$pmon", "s3cret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cfg.Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !Exists(dir) {
		t.Fatal("Exists should report the file that was just written")
	}

	read, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if hosts := read.Hosts(); len(hosts) != 1 || hosts[0] != "harbor.example.com" {
		t.Fatalf("hosts = %v, want [harbor.example.com]", hosts)
	}
	user, ok := read.Username("harbor.example.com")
	if !ok || user != "robot$pmon" {
		t.Fatalf("Username = %q, %v; want robot$pmon", user, ok)
	}

	var doc struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the file docker will read is not valid json: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(doc.Auths["harbor.example.com"].Auth)
	if err != nil {
		t.Fatalf("auth is not base64: %v", err)
	}
	if string(decoded) != "robot$pmon:s3cret" {
		t.Fatalf("auth decodes to %q, want robot$pmon:s3cret", decoded)
	}
}

func TestWriteUsesTightPermissions(t *testing.T) {
	base := filepath.Join(t.TempDir(), "docker")
	dir := filepath.Join(base, "pmon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := New()
	if err := cfg.Set("harbor.example.com", "robot", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, path := range []string{base, dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != DirMode {
			t.Errorf("%s has mode %04o, want %04o so the service account cannot read it", path, perm, DirMode)
		}
	}

	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != FileMode {
		t.Errorf("%s has mode %04o, want %04o", Path(dir), perm, FileMode)
	}
}

func TestReadPreservesKeysItDoesNotUnderstand(t *testing.T) {
	dir := t.TempDir()
	body := `{"auths":{"harbor.example.com":{"auth":"YTpi","email":"ops@example.com"}},"credsStore":"pass","psFormat":"table"}`
	if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := cfg.Set("registry.example.com:5000", "u", "p"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"credsStore", "psFormat"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("%s was dropped on the round trip", key)
		}
	}
	if !strings.Contains(string(raw), "ops@example.com") {
		t.Error("the email field on an existing entry was dropped on the round trip")
	}
}

func TestSetClearsAStaleToken(t *testing.T) {
	dir := t.TempDir()
	body := `{"auths":{"harbor.example.com":{"auth":"YTpi","identitytoken":"stale"}}}`
	if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("harbor.example.com", "u", "p"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Write(dir); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "identitytoken") {
		t.Error("a stale identitytoken would win over the credentials that were just stored")
	}
}

func TestRemoveAndDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pmon")

	cfg := New()
	if err := cfg.Set("a.example.com", "u", "p"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("b.example.com", "u", "p"); err != nil {
		t.Fatal(err)
	}
	if !cfg.Remove("a.example.com") {
		t.Error("Remove should report that it removed an entry it had")
	}
	if cfg.Remove("a.example.com") {
		t.Error("Remove should report false for an entry it does not have")
	}
	if cfg.Empty() {
		t.Error("one entry is left, so the store is not empty")
	}
	if !cfg.Remove("b.example.com") || !cfg.Empty() {
		t.Error("removing the last entry should leave an empty store")
	}

	if err := cfg.Write(dir); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if Exists(dir) {
		t.Error("Delete left the file in place")
	}
	if err := Delete(dir); err != nil {
		t.Errorf("Delete on an absent file should be a no-op, got %v", err)
	}
}

func TestReadAbsentFileIsAnEmptyStore(t *testing.T) {
	cfg, err := Read(filepath.Join(t.TempDir(), "nothing-here"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !cfg.Empty() {
		t.Error("an absent file should read as an empty store")
	}
}

func TestStoreDirRejectsAnythingThatEscapesBase(t *testing.T) {
	for _, stack := range []string{"..", ".", "", "../evil", "a/b", "/etc", "."} {
		if dir, err := StoreDir("/etc/dup/docker", stack); err == nil {
			t.Errorf("StoreDir(%q) = %q, want an error", stack, dir)
		}
	}

	dir, err := StoreDir("/etc/dup/docker/", "pmon")
	if err != nil {
		t.Fatalf("StoreDir: %v", err)
	}
	if dir != "/etc/dup/docker/pmon" {
		t.Errorf("StoreDir = %q, want /etc/dup/docker/pmon", dir)
	}
	if _, err := StoreDir("etc/dup/docker", "pmon"); err == nil {
		t.Error("a relative base must be refused")
	}
}

func TestNormaliseHost(t *testing.T) {
	cases := map[string]string{
		"harbor.example.com":          "harbor.example.com",
		"https://harbor.example.com":  "harbor.example.com",
		"http://harbor.example.com/":  "harbor.example.com",
		"  registry.example.com:5000": "registry.example.com:5000",
		"docker.io":                   HubHost,
		"index.docker.io":             HubHost,
		"https://index.docker.io/v1/": HubHost,
	}
	for in, want := range cases {
		got, err := NormaliseHost(in)
		if err != nil {
			t.Errorf("NormaliseHost(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormaliseHost(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"", "   ", "harbor.example.com/library", "not a host", "https://"} {
		if got, err := NormaliseHost(in); err == nil {
			t.Errorf("NormaliseHost(%q) = %q, want an error", in, got)
		}
	}
}

func TestProbeHost(t *testing.T) {
	if got := ProbeHost(HubHost); got != "registry-1.docker.io" {
		t.Errorf("ProbeHost(hub) = %q, want registry-1.docker.io", got)
	}
	if got := ProbeHost("harbor.example.com"); got != "harbor.example.com" {
		t.Errorf("ProbeHost = %q, want it unchanged", got)
	}
}

func TestSetRejectsIncompleteCredentials(t *testing.T) {
	cfg := New()
	if err := cfg.Set("", "u", "p"); err == nil {
		t.Error("an empty host must be refused")
	}
	if err := cfg.Set("h.example.com", "", "p"); err == nil {
		t.Error("an empty username must be refused")
	}
	if err := cfg.Set("h.example.com", "u", ""); err == nil {
		t.Error("an empty password must be refused")
	}
	if err := cfg.Set("h.example.com", "u:v", "p"); err == nil {
		t.Error("a username with a colon cannot be encoded unambiguously")
	}
}

func TestBaseMustNotBeATopLevelDirectory(t *testing.T) {
	for _, base := range []string{"/", "/etc", "/var/"} {
		if _, err := StoreDir(base, "pmon"); err == nil {
			t.Errorf("StoreDir(%q) should refuse a top level directory", base)
		}
		if err := EnsureBase(base); err == nil {
			t.Errorf("EnsureBase(%q) should refuse to tighten a top level directory", base)
		}
	}
}

func TestEnsureBaseTightensAnExistingDirectory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "dup", "docker")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBase(base); err != nil {
		t.Fatalf("EnsureBase: %v", err)
	}

	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != DirMode {
		t.Fatalf("%s has mode %04o, want %04o", base, perm, DirMode)
	}
}
