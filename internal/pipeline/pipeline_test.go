package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/compose"
	"github.com/9technologygroup/docker-updater/internal/config"
)

func TestSettled(t *testing.T) {
	containers := []compose.Container{
		{Service: "web", State: "running", Health: "healthy"},
		{Service: "db", State: "running"},
		{Service: "worker", State: "exited"},
	}

	if ok, _ := settled(containers, []string{"web", "db"}); !ok {
		t.Error("web and db should be settled")
	}
	if ok, reason := settled(containers, []string{"web", "worker"}); ok {
		t.Error("an exited service is not settled")
	} else if !strings.Contains(reason, "worker") {
		t.Errorf("reason = %q, want it to name worker", reason)
	}
	if ok, reason := settled(containers, []string{"missing"}); ok {
		t.Errorf("a missing service is not settled, reason = %q", reason)
	}
}

func TestCrashed(t *testing.T) {
	containers := []compose.Container{
		{Service: "web", State: "running", Health: "starting"},
		{Service: "db", State: "exited"},
	}

	if svc, ok := crashed(containers, []string{"web"}); ok {
		t.Errorf("a starting container is not crashed, got %q", svc)
	}
	if svc, ok := crashed(containers, []string{"db"}); !ok || svc != "db" {
		t.Errorf("crashed = %q, %v, want db, true", svc, ok)
	}
	if svc, ok := crashed(containers, []string{"gone"}); !ok || svc != "gone" {
		t.Errorf("a missing container counts as crashed, got %q, %v", svc, ok)
	}
}

func TestRefTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/acme/web:2.0.3":  "2.0.3",
		"web:latest":              "latest",
		"registry:5000/web:2.0.3": "2.0.3",
		"web":                     "",
		"web@sha256:abc":          "",
		"registry:5000/web":       "",
	}
	for ref, want := range cases {
		if got := refTag(ref); got != want {
			t.Errorf("refTag(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestTargetEnv(t *testing.T) {
	withEnv := &config.Target{ImageTagEnv: "APP_VERSION"}
	if got := targetEnv(withEnv, "2.0.4"); len(got) != 1 || got[0] != "APP_VERSION=2.0.4" {
		t.Errorf("targetEnv = %v", got)
	}
	if got := targetEnv(withEnv, ""); got != nil {
		t.Errorf("targetEnv with no tag = %v, want nil", got)
	}
	if got := targetEnv(&config.Target{}, "2.0.4"); got != nil {
		t.Errorf("targetEnv with no image_tag_env = %v, want nil", got)
	}
}

// A host that has never run `dup auth` must see exactly the environment it saw
// before the credential store existed.
func TestTargetEnvIsUnchangedWithoutACredentialStore(t *testing.T) {
	target := loadTarget(t, false)

	if got := targetEnv(target, ""); got != nil {
		t.Fatalf("targetEnv = %v, want nil so docker inherits root's own config", got)
	}
	if got := targetEnv(target, "2.0.4"); len(got) != 1 || got[0] != "APP_VERSION=2.0.4" {
		t.Fatalf("targetEnv = %v, want only the tag", got)
	}
}

func TestTargetEnvPointsAtTheStackStoreWhenItExists(t *testing.T) {
	target := loadTarget(t, true)

	got := targetEnv(target, "2.0.4")
	if len(got) != 2 {
		t.Fatalf("targetEnv = %v, want DOCKER_CONFIG and the tag", got)
	}
	if got[0] != "DOCKER_CONFIG="+target.DockerConfigDir() {
		t.Errorf("targetEnv[0] = %q, want the stack's own store", got[0])
	}
	if got[1] != "APP_VERSION=2.0.4" {
		t.Errorf("targetEnv[1] = %q, want the tag", got[1])
	}
}

func loadTarget(t *testing.T, withStore bool) *config.Target {
	t.Helper()

	stack := t.TempDir()
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()

	body := "docker_config_dir: " + store + "\n" +
		"auth:\n  bearer_token: 0123456789abcdef0123456789abcdef\n" +
		"targets:\n  - name: pmon\n    dir: " + stack + "\n    image_tag_env: APP_VERSION\n"

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	target, ok := cfg.Target("pmon")
	if !ok {
		t.Fatal("target pmon not found")
	}

	if withStore {
		if err := os.MkdirAll(target.DockerConfigDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"auths":{"harbor.example.com":{"auth":"cm9ib3Q6cHc="}}}`
		if err := os.WriteFile(filepath.Join(target.DockerConfigDir(), "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"harbor.example.com/library/app:1.0": "harbor.example.com",
		"registry.example.com:5000/app":      "registry.example.com:5000",
		"localhost:5000/app":                 "localhost:5000",
		"localhost/app":                      "localhost",
		"library/nginx:1.27":                 "",
		"nginx:1.27":                         "",
		"":                                   "",
	}
	for ref, want := range cases {
		if got := registryHost(ref); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestRegistryHostsSkipsServicesOutOfScope(t *testing.T) {
	target := &config.Target{Name: "pmon", Services: []string{"app"}}
	desired := map[string]string{
		"app":   "harbor.example.com/library/app:1.0",
		"cache": "other.example.com/redis:7",
	}
	got := registryHosts(target, desired)
	if len(got) != 1 || got[0] != "harbor.example.com" {
		t.Fatalf("registryHosts = %v, want only the registry of the services in scope", got)
	}
}

func TestWantedRespectsServiceAllowlist(t *testing.T) {
	all := &config.Target{}
	if !wanted(all, "anything") {
		t.Error("an empty services list means every service is in scope")
	}

	scoped := &config.Target{Services: []string{"web"}}
	if !wanted(scoped, "web") {
		t.Error("web should be in scope")
	}
	if wanted(scoped, "database") {
		t.Error("database is outside the configured scope")
	}
}

func TestRepository(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/acme/web:2.0.3":  "ghcr.io/acme/web",
		"ghcr.io/acme/web:":       "ghcr.io/acme/web",
		"web:latest":              "web",
		"web":                     "web",
		"registry:5000/web:2.0.3": "registry:5000/web",
		"registry:5000/web":       "registry:5000/web",
		"alpine":                  "alpine",
		"repo@sha256:abc":         "repo",
	}
	for ref, want := range cases {
		if got := repository(ref); got != want {
			t.Errorf("repository(%q) = %q, want %q", ref, got, want)
		}
	}
}

// realPullOutput is a verbatim capture from a production host: one private
// image 401s and every other line is progress for images that were cancelled.
const realPullOutput = `Image postgres:17-alpine Pulling 
 Image ghcr.io/patchmon/patchmon-server:latest Pulling 
 Image cr.dev.patchmon.cloud/multi-tenant/patchmon-provisioner:latest Pulling 
 Image guacamole/guacd:1.6.0 Pulling 
 Image redis:7-alpine Pulling 
 Image cr.dev.patchmon.cloud/multi-tenant/patchmon-provisioner:latest Error unknown: failed to resolve reference "cr.dev.patchmon.cloud/multi-tenant/patchmon-provisioner:latest": unexpected status from HEAD request to https://cr.dev.patchmon.cloud/v2/multi-tenant/patchmon-provisioner/manifests/latest: 401 Unauthorized
 Image ghcr.io/patchmon/patchmon-server:latest Interrupted 
 Image redis:7-alpine Interrupted 
 Image guacamole/guacd:1.6.0 Interrupted 
 Image postgres:17-alpine Interrupted 
Error response from daemon: unknown: failed to resolve reference "cr.dev.patchmon.cloud/multi-tenant/patchmon-provisioner:latest": unexpected status from HEAD request to https://cr.dev.patchmon.cloud/v2/multi-tenant/patchmon-provisioner/manifests/latest: 401 Unauthorized`

func TestParsePullNamesOnlyTheImageThatActuallyFailed(t *testing.T) {
	f := parsePull(realPullOutput)

	if len(f.refs) != 1 || f.refs[0] != "cr.dev.patchmon.cloud/multi-tenant/patchmon-provisioner:latest" {
		t.Fatalf("refs = %v, want only the provisioner image", f.refs)
	}
	if f.reason != "401 Unauthorized" {
		t.Errorf("reason = %q, want %q", f.reason, "401 Unauthorized")
	}
	if f.hosts == nil || f.hosts[0] != "cr.dev.patchmon.cloud" {
		t.Errorf("hosts = %v, want only the private registry, not ghcr.io or docker hub", f.hosts)
	}
	if !f.auth {
		t.Error("a 401 must be recognised as a credentials failure")
	}
	if f.cancelled != 9 {
		t.Errorf("cancelled = %d, want the 9 progress lines that are not failures", f.cancelled)
	}
}

func TestPullDetailDropsTheProgressNoise(t *testing.T) {
	target := &config.Target{Name: "patchmon"}
	out := parsePull(realPullOutput).detail(target, realPullOutput)

	for _, unwanted := range []string{"Pulling", "Interrupted", "failed to resolve reference", "HEAD request"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("detail still carries %q:\n%s", unwanted, out)
		}
	}
	for _, wanted := range []string{
		"cr.dev.patchmon.cloud/multi-tenant/patchmon-provisioner:latest",
		"401 Unauthorized",
		"sudo dup auth patchmon",
		"runs docker as root",
		"The other 9 images",
	} {
		if !strings.Contains(out, wanted) {
			t.Errorf("detail is missing %q:\n%s", wanted, out)
		}
	}
	if strings.Contains(out, "ghcr.io") || strings.Contains(out, "redis") {
		t.Errorf("detail blames an image that pulled fine:\n%s", out)
	}
}

func TestPullSummaryStaysOneLine(t *testing.T) {
	got := parsePull(realPullOutput).summary(&config.Target{Name: "patchmon"})
	if strings.Contains(got, "\n") {
		t.Fatalf("the job message must stay one line: %q", got)
	}
	for _, want := range []string{"patchmon-provisioner:latest", "401 Unauthorized", "sudo dup auth patchmon"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, missing %q", got, want)
		}
	}
}

func TestParsePullIgnoresFailuresThatAreNotAboutCredentials(t *testing.T) {
	f := parsePull(" Image nginx:1.27 Error manifest unknown")
	if f.auth {
		t.Error("manifest unknown is not a credentials failure")
	}
	if len(f.refs) != 1 || f.refs[0] != "nginx:1.27" {
		t.Fatalf("refs = %v", f.refs)
	}
	if got := f.detail(&config.Target{Name: "x"}, ""); strings.Contains(got, "dup auth") {
		t.Errorf("must not advise dup auth for a non-credentials failure:\n%s", got)
	}
}

func TestParsePullFallsBackWhenItCannotParse(t *testing.T) {
	raw := "something compose has never printed before"
	f := parsePull(raw)
	if f.parsed {
		t.Fatal("nothing parseable should not report parsed")
	}
	if got := f.detail(&config.Target{Name: "x"}, raw); !strings.Contains(got, raw) {
		t.Errorf("the raw output must survive when parsing fails: %s", got)
	}
	if got := f.summary(&config.Target{Name: "x"}); got != "docker compose pull failed" {
		t.Errorf("summary = %q", got)
	}
}
