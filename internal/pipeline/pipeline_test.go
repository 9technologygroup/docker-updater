package pipeline

import (
	"strings"
	"testing"

	"github.com/PatchMon/docker-updater/internal/compose"
	"github.com/PatchMon/docker-updater/internal/config"
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

func TestTagEnv(t *testing.T) {
	withEnv := &config.Target{ImageTagEnv: "APP_VERSION"}
	if got := tagEnv(withEnv, "2.0.4"); len(got) != 1 || got[0] != "APP_VERSION=2.0.4" {
		t.Errorf("tagEnv = %v", got)
	}
	if got := tagEnv(withEnv, ""); got != nil {
		t.Errorf("tagEnv with no tag = %v, want nil", got)
	}
	if got := tagEnv(&config.Target{}, "2.0.4"); got != nil {
		t.Errorf("tagEnv with no image_tag_env = %v, want nil", got)
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
