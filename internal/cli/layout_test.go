package cli

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDupDoesNotLinkAnythingThatCanExecDocker(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/PatchMon/docker-updater/cmd/dup").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}

	for _, pkg := range strings.Fields(string(out)) {
		switch pkg {
		case "github.com/PatchMon/docker-updater/internal/compose",
			"github.com/PatchMon/docker-updater/internal/pipeline",
			"github.com/PatchMon/docker-updater/internal/discover",
			"github.com/PatchMon/docker-updater/internal/agentd":
			t.Errorf("dup links %s; the unprivileged binary must not contain code that can exec docker", pkg)
		}
	}
}

func TestDupAgentLinksTheDockerLayer(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/PatchMon/docker-updater/cmd/dup-agent").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	if !strings.Contains(string(out), "github.com/PatchMon/docker-updater/internal/compose") {
		t.Error("dup-agent should link internal/compose; it is the half that talks to docker")
	}
}

func TestShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Minute:           "30m",
		6 * time.Hour:              "6h",
		90 * time.Minute:           "1h30m",
		45 * time.Second:           "45s",
		0:                          "0",
		24 * time.Hour:             "24h",
		time.Hour + 30*time.Second: "1h0m30s",
		10 * time.Minute:           "10m",
		100 * time.Minute:          "1h40m",
	}
	for d, want := range cases {
		if got := short(d); got != want {
			t.Errorf("short(%s) = %q, want %q", d, got, want)
		}
	}
}
