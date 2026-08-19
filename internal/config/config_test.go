package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stackDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const goodToken = "0123456789abcdef0123456789abcdef"

func TestLoadAcceptsValidConfig(t *testing.T) {
	dir := stackDir(t)
	path := writeConfig(t, `
auth:
  bearer_token: `+goodToken+`
targets:
  - name: pmon
    dir: `+dir+`
    compose_file: docker-compose.yml
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	target, ok := cfg.Target("pmon")
	if !ok {
		t.Fatal("target pmon not found")
	}
	if !target.RollbackEnabled() {
		t.Error("rollback should default to enabled")
	}
	if target.HealthTimeout == 0 || target.PullTimeout == 0 || target.JobTimeout == 0 {
		t.Error("timeouts should be defaulted")
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	dir := stackDir(t)
	auth := "auth:\n  bearer_token: " + goodToken + "\n"

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "traversal in compose_file",
			body: auth + "targets:\n  - name: a\n    dir: " + dir + "\n    compose_file: ../../etc/passwd\n",
			want: "bare filename",
		},
		{
			name: "absolute compose_file",
			body: auth + "targets:\n  - name: a\n    dir: " + dir + "\n    compose_file: /etc/passwd\n",
			want: "bare filename",
		},
		{
			name: "relative dir",
			body: auth + "targets:\n  - name: a\n    dir: ./relative\n",
			want: "absolute path",
		},
		{
			name: "target name with shell metacharacters",
			body: auth + "targets:\n  - name: \"a; rm -rf /\"\n    dir: " + dir + "\n",
			want: "must match",
		},
		{
			name: "service name that looks like a flag",
			body: auth + "targets:\n  - name: a\n    dir: " + dir + "\n    services: [\"--volumes\"]\n",
			want: "must match",
		},
		{
			name: "duplicate target names",
			body: auth + "targets:\n  - name: a\n    dir: " + dir + "\n  - name: a\n    dir: " + dir + "\n",
			want: "duplicate",
		},
		{
			name: "no auth configured",
			body: "targets:\n  - name: a\n    dir: " + dir + "\n",
			want: "at least one of bearer token",
		},
		{
			name: "short bearer token",
			body: "auth:\n  bearer_token: short\ntargets:\n  - name: a\n    dir: " + dir + "\n",
			want: "at least 32 characters",
		},
		{
			name: "unknown field",
			body: auth + "wat: true\ntargets:\n  - name: a\n    dir: " + dir + "\n",
			want: "field wat not found",
		},
		{
			name: "notify url with bad scheme",
			body: auth + "notify:\n  url: \"file:///etc/passwd\"\ntargets:\n  - name: a\n    dir: " + dir + "\n",
			want: "scheme must be http",
		},
		{
			name: "bad image_tag_env",
			body: auth + "targets:\n  - name: a\n    dir: " + dir + "\n    image_tag_env: \"lower case\"\n",
			want: "must match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestStabilityWindowDefaultsAndBounds(t *testing.T) {
	dir := stackDir(t)
	auth := "auth:\n  bearer_token: " + goodToken + "\n"

	cfg, err := Load(writeConfig(t, auth+"targets:\n  - name: a\n    dir: "+dir+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := cfg.Target("a")
	if target.StabilityWindow <= 0 {
		t.Fatal("stability_window should be defaulted")
	}
	if target.StabilityWindow >= target.HealthTimeout {
		t.Fatalf("stability_window %s should be shorter than health_timeout %s", target.StabilityWindow, target.HealthTimeout)
	}

	_, err = Load(writeConfig(t, auth+"targets:\n  - name: a\n    dir: "+dir+"\n    health_timeout: 10s\n    stability_window: 30s\n"))
	if err == nil || !strings.Contains(err.Error(), "stability_window") {
		t.Fatalf("error = %v, want a stability_window complaint", err)
	}
}

func TestLoadRejectsWorldReadableSecretFile(t *testing.T) {
	dir := stackDir(t)
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte(goodToken), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgFor := func() string {
		return "auth:\n  bearer_token_file: " + secret + "\ntargets:\n  - name: a\n    dir: " + dir + "\n"
	}

	rejected := []os.FileMode{0o644, 0o666, 0o604, 0o660, 0o662}
	for _, mode := range rejected {
		if err := os.Chmod(secret, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Load(writeConfig(t, cfgFor()))
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Errorf("mode %04o: error = %v, want a permissions complaint", mode, err)
		}
	}

	accepted := []os.FileMode{0o600, 0o640, 0o440}
	for _, mode := range accepted {
		if err := os.Chmod(secret, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(writeConfig(t, cfgFor())); err != nil {
			t.Errorf("mode %04o should be accepted, got %v", mode, err)
		}
	}
}

func TestAgentSocketPathIsBounded(t *testing.T) {
	dir := stackDir(t)
	auth := "auth:\n  bearer_token: " + goodToken + "\n"
	long := "/" + strings.Repeat("a", MaxSocketPathLen) + "/agent.sock"

	_, err := Load(writeConfig(t, auth+"agent_socket: "+long+"\ntargets:\n  - name: a\n    dir: "+dir+"\n"))
	if err == nil || !strings.Contains(err.Error(), "unix sockets") {
		t.Fatalf("error = %v, want a socket path length complaint", err)
	}

	_, err = Load(writeConfig(t, auth+"agent_socket: relative.sock\ntargets:\n  - name: a\n    dir: "+dir+"\n"))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want an absolute path complaint", err)
	}
}

func TestLoadRejectsMissingComposeFile(t *testing.T) {
	empty := t.TempDir()
	_, err := Load(writeConfig(t, "auth:\n  bearer_token: "+goodToken+"\ntargets:\n  - name: a\n    dir: "+empty+"\n"))
	if err == nil || !strings.Contains(err.Error(), "no compose file") {
		t.Fatalf("error = %v, want a missing compose file complaint", err)
	}
}

func TestValidImageTag(t *testing.T) {
	valid := []string{"2.0.4", "latest", "v2.0.4", "2.0.4-rc.1", "sha-abc123", "1.0.0_build"}
	for _, tag := range valid {
		if !ValidImageTag(tag) {
			t.Errorf("ValidImageTag(%q) = false, want true", tag)
		}
	}

	invalid := []string{"", "-flag", "--rm", "2.0.4; rm -rf /", "tag with space", "a/b", "tag$(id)", "tag\nnewline", strings.Repeat("a", 200)}
	for _, tag := range invalid {
		if ValidImageTag(tag) {
			t.Errorf("ValidImageTag(%q) = true, want false", tag)
		}
	}
}

func TestTargetsSharingADirectoryBasenameAreRejected(t *testing.T) {
	base := t.TempDir()
	one := filepath.Join(base, "a", "pmon")
	two := filepath.Join(base, "b", "pmon")
	for _, d := range []string{one, two} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Load(writeConfig(t, "auth:\n  bearer_token: "+goodToken+
		"\ntargets:\n  - name: one\n    dir: "+one+"\n  - name: two\n    dir: "+two+"\n"))
	if err == nil || !strings.Contains(err.Error(), "same project") {
		t.Fatalf("error = %v, want a compose project collision complaint", err)
	}
}

func TestSymlinkedComposeFileIsRejected(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.yml")
	if err := os.WriteFile(outside, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "docker-compose.yml")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := Load(writeConfig(t, "auth:\n  bearer_token: "+goodToken+
		"\ntargets:\n  - name: a\n    dir: "+dir+"\n    compose_file: docker-compose.yml\n"))
	if err == nil || !strings.Contains(err.Error(), "outside its own directory") {
		t.Fatalf("error = %v, want a symlink escape complaint", err)
	}
}

func TestSecretFileMustBeARegularFile(t *testing.T) {
	dir := stackDir(t)
	secretDir := t.TempDir()
	real := filepath.Join(secretDir, "real.token")
	if err := os.WriteFile(real, []byte(goodToken), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(secretDir, "link.token")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := Load(writeConfig(t, "auth:\n  bearer_token_file: "+link+"\ntargets:\n  - name: a\n    dir: "+dir+"\n")); err != nil {
		t.Fatalf("a symlink to a well-permissioned regular file is fine: %v", err)
	}

	fifo := filepath.Join(secretDir, "fifo.token")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	_, err := Load(writeConfig(t, "auth:\n  bearer_token_file: "+fifo+"\ntargets:\n  - name: a\n    dir: "+dir+"\n"))
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %v, want a regular-file complaint", err)
	}
}

func TestSoakZeroIsHonouredAndDistinctFromUnset(t *testing.T) {
	dir := stackDir(t)
	auth := "auth:\n  bearer_token: " + goodToken + "\n"

	cfg, err := Load(writeConfig(t, auth+"defaults:\n  soak: 45m\ntargets:\n"+
		"  - name: explicit\n    dir: "+dir+"\n    soak: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := cfg.Target("explicit")
	if got := target.SoakWindow(); got != 0 {
		t.Errorf("soak = %s, want 0; an explicit 0s must not fall back to the default", got)
	}

	base := t.TempDir()
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err = Load(writeConfig(t, auth+"defaults:\n  soak: 45m\ntargets:\n  - name: inherit\n    dir: "+other+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ = cfg.Target("inherit")
	if got := target.SoakWindow(); got != 45*time.Minute {
		t.Errorf("soak = %s, want 45m inherited from defaults", got)
	}
}

func TestNetworkAndTLSValidation(t *testing.T) {
	dir := stackDir(t)
	auth := "auth:\n  bearer_token: " + goodToken + "\n"
	targets := "targets:\n  - name: a\n    dir: " + dir + "\n"

	bad := []struct{ name, body, want string }{
		{"bad cidr", auth + "allow_from: [\"10.0.0.0/64\"]\n" + targets, "not a valid CIDR"},
		{"bad ip", auth + "allow_from: [\"not-an-ip\"]\n" + targets, "not a valid IP"},
		{"bad proxy", auth + "trusted_proxies: [\"999.1.1.1\"]\n" + targets, "not a valid IP"},
		{"cors wildcard", auth + "cors:\n  allowed_origins: [\"*\"]\n" + targets, "not allowed"},
		{"cors not an origin", auth + "cors:\n  allowed_origins: [\"example.com\"]\n" + targets, "full origin"},
		{"tls half configured", auth + "tls:\n  cert_file: /tmp/a.crt\n" + targets, "must be set together"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	cfg, err := Load(writeConfig(t, auth+"allow_from: [\"10.0.0.0/8\", \"192.168.1.5\"]\ntrusted_proxies: [\"127.0.0.1\"]\n"+targets))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowFromPrefixes()) != 2 || len(cfg.TrustedProxyPrefixes()) != 1 {
		t.Errorf("prefixes = %v / %v", cfg.AllowFromPrefixes(), cfg.TrustedProxyPrefixes())
	}
}

func TestSelfSignedTLSDefaultsItsPaths(t *testing.T) {
	dir := stackDir(t)
	cfg, err := Load(writeConfig(t, "auth:\n  bearer_token: "+goodToken+
		"\ntls:\n  self_signed: true\ntargets:\n  - name: a\n    dir: "+dir+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLS.IsEnabled() {
		t.Fatal("self_signed should enable tls")
	}
	if cfg.TLS.CertFile != DefaultCertFile || cfg.TLS.KeyFile != DefaultKeyFile {
		t.Errorf("cert=%q key=%q, want the defaults", cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
}

func TestPreUpdateHookValidation(t *testing.T) {
	dir := stackDir(t)
	auth := "auth:\n  bearer_token: " + goodToken + "\n"

	script := filepath.Join(t.TempDir(), "backup.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(writeConfig(t, auth+"targets:\n  - name: a\n    dir: "+dir+
		"\n    pre_update:\n      command: "+script+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := cfg.Target("a")
	if !target.PreUpdate.Configured() {
		t.Fatal("hook should be configured")
	}
	if !target.PreUpdate.IsRequired() {
		t.Error("a hook must default to required, so a failed backup blocks the update")
	}
	if target.PreUpdate.Timeout <= 0 {
		t.Error("hook timeout should be defaulted")
	}

	notExec := filepath.Join(t.TempDir(), "plain.sh")
	if err := os.WriteFile(notExec, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(writeConfig(t, auth+"targets:\n  - name: a\n    dir: "+dir+
		"\n    pre_update:\n      command: "+notExec+"\n"))
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("error = %v, want an executability complaint", err)
	}

	_, err = Load(writeConfig(t, auth+"targets:\n  - name: a\n    dir: "+dir+
		"\n    pre_update:\n      command: backup.sh\n"))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want an absolute path complaint", err)
	}
}

func TestSecretGroupIsCheckedAgainstTheServiceAccountNotTheCaller(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte(goodToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o640); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no POSIX stat available")
	}
	fileGID := int64(stat.Gid)

	// Production is root:dup 0640, read by root commands such as the agent
	// unit's ExecStartPre. Keying this on the caller's gid rejected exactly
	// that, so the agent could never start.
	if err := checkSecretFile(secret, info, fileGID, true); err != nil {
		t.Errorf("a secret group-owned by the service account must be accepted: %v", err)
	}

	// A group that is neither the caller's nor the service account's must fail,
	// which is what stops root:docker 0640 handing the token to the docker group.
	unrelated := fileGID + 1
	if unrelated == int64(os.Getgid()) {
		unrelated++
	}
	if fileGID != int64(os.Getgid()) {
		if err := checkSecretFile(secret, info, unrelated, true); err == nil {
			t.Error("a secret readable by an unrelated group must be rejected")
		}
	}
}

// An unmounted volume must not be fatal: dup check gates ExecStartPre for both
// units, so failing here would take the whole service down over one absent path.
func TestMissingTargetDirIsAWarningNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\n" +
		"targets:\n" +
		"  - name: here\n    dir: " + dir + "\n" +
		"  - name: gone\n    dir: " + filepath.Join(dir, "not-mounted") + "\n"

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warnings := cfg.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "gone") {
		t.Errorf("warning does not name the stack: %q", warnings[0])
	}
}

func TestNonDirectoryTargetDirIsStillAnError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\n" +
		"targets:\n  - name: a\n    dir: " + file + "\n"

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a target dir that is a regular file was accepted")
	}
}

func TestTLSEnabledPrecedence(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		tls  TLS
		want bool
	}{
		{"explicit off beats paths", TLS{Enabled: &no, CertFile: "/c", KeyFile: "/k", SelfSigned: true}, false},
		{"explicit off beats self_signed", TLS{Enabled: &no, SelfSigned: true}, false},
		{"explicit on", TLS{Enabled: &yes, CertFile: "/c", KeyFile: "/k"}, true},
		{"inferred from self_signed", TLS{SelfSigned: true}, true},
		{"inferred from cert_file", TLS{CertFile: "/c"}, true},
		{"inferred off", TLS{}, false},
	}
	for _, tc := range cases {
		if got := tc.tls.IsEnabled(); got != tc.want {
			t.Errorf("%s: IsEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The reference config carries the certificate paths so they are visible, so
// enabled: false has to switch TLS off despite them being set.
func TestTLSDisabledWithPathsStillSet(t *testing.T) {
	body := "auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\n" +
		"tls:\n  enabled: false\n  self_signed: true\n" +
		"  cert_file: /etc/dup/self-signed.crt\n  key_file: /etc/dup/self-signed.key\n" +
		"targets:\n  - name: a\n    dir: " + t.TempDir() + "\n"

	cfg, err := LoadService(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadService: %v", err)
	}
	if cfg.TLS.IsEnabled() {
		t.Error("enabled: false did not switch TLS off")
	}
}

// A config written before enabled: existed must behave exactly as it did.
func TestTLSWithoutEnabledKeyIsUnchanged(t *testing.T) {
	body := "auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\n" +
		"tls:\n  self_signed: true\n" +
		"targets:\n  - name: a\n    dir: " + t.TempDir() + "\n"

	cfg, err := LoadService(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadService: %v", err)
	}
	if !cfg.TLS.IsEnabled() {
		t.Error("self_signed: true alone should still enable TLS")
	}
	if cfg.TLS.CertFile != DefaultCertFile || cfg.TLS.KeyFile != DefaultKeyFile {
		t.Errorf("paths = %q %q, want the defaults", cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
}

func TestTLSEnabledWithNothingToServe(t *testing.T) {
	body := "auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\n" +
		"tls:\n  enabled: true\n" +
		"targets:\n  - name: a\n    dir: " + t.TempDir() + "\n"

	if _, err := LoadService(writeConfig(t, body)); err == nil {
		t.Fatal("tls.enabled with no certificate and no self_signed was accepted")
	}
}

// A host that has just installed dup has nothing under its control yet. It must
// still start, so dup list can show what is on the host and worth adding.
func TestNoTargetsIsValid(t *testing.T) {
	for _, body := range []string{
		"auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\ntargets: []\n",
		"auth:\n  bearer_token: " + strings.Repeat("a", 32) + "\n",
	} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load with no targets: %v", err)
		}
		if len(cfg.Targets) != 0 {
			t.Errorf("got %d targets, want 0", len(cfg.Targets))
		}
		if len(cfg.TargetNames()) != 0 || len(cfg.AutoUpdateTargets()) != 0 {
			t.Error("empty config reported targets")
		}
		if _, ok := cfg.Target("anything"); ok {
			t.Error("lookup succeeded against an empty config")
		}
	}
}

func TestDockerConfigDirDefaultsAndResolvesPerStack(t *testing.T) {
	dir := stackDir(t)
	path := writeConfig(t, `
auth:
  bearer_token: `+goodToken+`
targets:
  - name: pmon
    dir: `+dir+`
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DockerConfigDir != DefaultDockerCfg {
		t.Fatalf("docker_config_dir = %q, want %q", cfg.DockerConfigDir, DefaultDockerCfg)
	}

	target, ok := cfg.Target("pmon")
	if !ok {
		t.Fatal("target pmon not found")
	}
	if got, want := target.DockerConfigDir(), filepath.Join(DefaultDockerCfg, "pmon"); got != want {
		t.Errorf("store = %q, want %q", got, want)
	}
	if target.HasDockerConfig() {
		t.Error("a stack with no store on disk must not claim to have one")
	}
}

func TestDockerConfigDirIsOverridable(t *testing.T) {
	stack := stackDir(t)
	store := t.TempDir()
	path := writeConfig(t, `
docker_config_dir: `+store+`
auth:
  bearer_token: `+goodToken+`
targets:
  - name: pmon
    dir: `+stack+`
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	target, _ := cfg.Target("pmon")
	if got, want := target.DockerConfigDir(), filepath.Join(store, "pmon"); got != want {
		t.Fatalf("store = %q, want %q", got, want)
	}

	if err := os.MkdirAll(target.DockerConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.DockerConfigDir(), "config.json"), []byte(`{"auths":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !target.HasDockerConfig() {
		t.Error("a stack with a config.json in its store must report it")
	}
}

func TestDockerConfigDirMustBeAbsolute(t *testing.T) {
	path := writeConfig(t, `
docker_config_dir: etc/dup/docker
auth:
  bearer_token: `+goodToken+`
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want a complaint about an absolute path", err)
	}
}

func TestDiscordWebhookDetection(t *testing.T) {
	yes := []string{
		"https://discord.com/api/webhooks/123/abc",
		"https://discordapp.com/api/webhooks/123/abc",
		"https://ptb.discord.com/api/webhooks/123/abc",
		"https://DISCORD.com/api/webhooks/123/abc",
	}
	no := []string{
		"https://n8n.example.com/webhook/dup",
		"https://discord.com/channels/123",
		"https://notdiscord.com/api/webhooks/123/abc",
		"https://example.com/discord.com/webhooks/1",
	}
	for _, u := range yes {
		if !IsDiscordWebhook(u) {
			t.Errorf("IsDiscordWebhook(%q) = false, want true", u)
		}
	}
	for _, u := range no {
		if IsDiscordWebhook(u) {
			t.Errorf("IsDiscordWebhook(%q) = true, want false", u)
		}
	}
}

// dup check gates ExecStartPre for both units, so a webhook nobody reads must
// never be the reason a host will not start dup.
func TestADiscordURLWithoutTheDiscordFormatWarnsRatherThanFailing(t *testing.T) {
	c := &Config{Notify: Notify{URL: "https://discord.com/api/webhooks/123/abc"}}
	if err := c.validateNotify(); err != nil {
		t.Fatalf("validateNotify returned an error, want a warning: %v", err)
	}
	if len(c.Warnings()) != 1 || !strings.Contains(c.Warnings()[0], "format: discord") {
		t.Errorf("warnings = %v, want one naming the fix", c.Warnings())
	}

	ok := &Config{Notify: Notify{URL: "https://discord.com/api/webhooks/123/abc", Format: "discord"}}
	if err := ok.validateNotify(); err != nil {
		t.Fatalf("validateNotify: %v", err)
	}
	if len(ok.Warnings()) != 0 {
		t.Errorf("warnings = %v, want none once the format matches", ok.Warnings())
	}
}

func TestNotifyFormatIsValidated(t *testing.T) {
	c := &Config{Notify: Notify{URL: "https://example.com/hook", Format: "slack"}}
	if err := c.validateNotify(); err == nil {
		t.Fatal("an unknown notify format must be refused")
	}
}
