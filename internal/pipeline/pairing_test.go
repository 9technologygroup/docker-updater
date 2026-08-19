package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/compose"
	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
)

// pairingSink fails the test the moment the pipeline opens a step it never
// closes, closes one it never opened, or closes one under a different name.
type pairingSink struct {
	t     *testing.T
	calls []string
	open  string
	live  bool
	adds  int
}

func (p *pairingSink) StartStep(name string) {
	p.t.Helper()
	if p.live {
		p.t.Errorf("StartStep(%q) while %q is still running", name, p.open)
	}
	p.open, p.live = name, true
	p.calls = append(p.calls, "start:"+name)
}

func (p *pairingSink) AddStep(s job.Step) {
	p.t.Helper()
	switch {
	case !p.live:
		p.t.Errorf("AddStep(%q) with no StartStep before it", s.Name)
	case p.open != s.Name:
		p.t.Errorf("AddStep(%q) closes StartStep(%q); the two names must match byte for byte", s.Name, p.open)
	}
	p.live = false
	p.adds++
	p.calls = append(p.calls, "add:"+s.Name)
}

func (p *pairingSink) SetBefore([]job.ServiceState) {}
func (p *pairingSink) SetAfter([]job.ServiceState)  {}
func (p *pairingSink) SetChanged([]string)          {}

type teeSink struct {
	a, b job.Sink
}

func (t teeSink) StartStep(name string) { t.a.StartStep(name); t.b.StartStep(name) }
func (t teeSink) AddStep(s job.Step)    { t.a.AddStep(s); t.b.AddStep(s) }
func (t teeSink) SetBefore(s []job.ServiceState) {
	t.a.SetBefore(s)
	t.b.SetBefore(s)
}
func (t teeSink) SetAfter(s []job.ServiceState) { t.a.SetAfter(s); t.b.SetAfter(s) }
func (t teeSink) SetChanged(s []string)         { t.a.SetChanged(s); t.b.SetChanged(s) }

const fakeDockerPreamble = `#!/bin/sh
all="$*"
case "$all" in
  *"config --quiet"*) exit 0 ;;
  *"config --format json"*) echo '{"services":{"web":{"image":"acme/web:1"}}}'; exit 0 ;;
  *"ps --all"*) echo '[{"Name":"demo-web-1","Service":"web","State":"running","Health":"healthy","Image":"acme/web:1"}]'; exit 0 ;;
  "inspect --type container"*) printf '/demo-web-1\tsha256:old\tacme/web:1\n'; exit 0 ;;
  "image inspect"*) echo "sha256:new"; exit 0 ;;
  tag*) exit 0 ;;
  *pull*) exit 0 ;;
`

const fakeDockerCoda = `esac
echo "the fake docker was asked for something the test did not plan: $all" >&2
exit 1
`

func fakeDocker(t *testing.T, upCases string) *compose.Runner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a /bin/sh script")
	}

	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(fakeDockerPreamble+upCases+fakeDockerCoda), 0o700); err != nil {
		t.Fatal(err)
	}
	return compose.New(path)
}

func newTarget(t *testing.T) *config.Target {
	t.Helper()
	return &config.Target{
		Name:          "demo",
		Dir:           t.TempDir(),
		ComposeFile:   "docker-compose.yml",
		PullTimeout:   time.Minute,
		HealthTimeout: 10 * time.Second,
		JobTimeout:    time.Minute,
		PreUpdate:     &config.Hook{Command: "/bin/sh", Args: []string{"-c", "exit 0"}, Timeout: 10 * time.Second},
	}
}

func TestPipelinePairsEveryStartStepWithItsCompletedStep(t *testing.T) {
	cases := []struct {
		name     string
		upCases  string
		want     job.State
		wantStep string
	}{
		{
			name:     "update succeeds",
			upCases:  `  *"up -d"*) exit 0 ;;` + "\n",
			want:     job.StateSucceeded,
			wantStep: "health",
		},
		{
			name: "update fails and rolls back",
			upCases: `  *"--force-recreate"*) exit 0 ;;` + "\n" +
				`  *"up -d"*) echo "no such image"; exit 1 ;;` + "\n",
			want:     job.StateRolledBack,
			wantStep: "rollback-up",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := New(fakeDocker(t, tc.upCases))
			target := newTarget(t)

			guard := &pairingSink{t: t}
			j, err := job.NewStore().Begin(target.Name, job.Request{})
			if err != nil {
				t.Fatal(err)
			}

			state, message := e.pipeline(context.Background(), target, teeSink{a: guard, b: j}, job.Request{})
			if state != tc.want {
				t.Fatalf("state = %q (%s), want %q; calls = %v", state, message, tc.want, guard.calls)
			}

			if guard.live {
				t.Errorf("the pipeline left %q running and never completed it", guard.open)
			}
			if !slices.Contains(guard.calls, "start:"+tc.wantStep) {
				t.Errorf("calls = %v, want them to include a start for %q", guard.calls, tc.wantStep)
			}

			// The real job replays the same calls, so a placeholder the pipeline
			// opened and never closed surfaces here as a step it never reported.
			steps := j.Snapshot().Steps
			if len(steps) != guard.adds {
				t.Fatalf("the job holds %d steps for %d completed steps: %v", len(steps), guard.adds, steps)
			}
			for _, s := range steps {
				if s.Running {
					t.Errorf("step %q is still marked running: %+v", s.Name, s)
				}
			}
		})
	}
}

func TestPipelineAnnouncesThePreUpdateHookBeforeItRuns(t *testing.T) {
	e := New(fakeDocker(t, `  *"up -d"*) exit 0 ;;`+"\n"))
	target := newTarget(t)

	marker := filepath.Join(t.TempDir(), "hook-ran")
	target.PreUpdate = &config.Hook{
		Command: "/bin/sh",
		Args:    []string{"-c", "touch " + marker},
		Timeout: 10 * time.Second,
	}

	guard := &pairingSink{t: t}
	msg, ok := e.runPreUpdate(context.Background(), target, guard, job.Request{}, []string{"web"})
	if !ok {
		t.Fatalf("runPreUpdate: %s", msg)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}

	want := []string{"start:pre-update", "add:pre-update"}
	if len(guard.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", guard.calls, want)
	}
	for i := range want {
		if guard.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", guard.calls, want)
		}
	}
}

func TestWaitHealthyPairsItsStepWhenItTimesOut(t *testing.T) {
	e := New(fakeDocker(t, `  *"up -d"*) exit 0 ;;`+"\n"))
	target := newTarget(t)
	target.HealthTimeout = time.Millisecond
	target.StabilityWindow = time.Hour

	guard := &pairingSink{t: t}
	if err := e.waitHealthy(context.Background(), target, guard, nil, []string{"web"}, "health"); err == nil {
		t.Fatal("waitHealthy should have timed out")
	}
	if guard.live {
		t.Errorf("waitHealthy left %q running", guard.open)
	}
	if len(guard.calls) != 2 || guard.calls[0] != "start:health" || guard.calls[1] != "add:health" {
		t.Errorf("calls = %v, want a start and a completed health step", guard.calls)
	}
}

func TestCheckToleratesTheDiscardSink(t *testing.T) {
	e := New(fakeDocker(t, `  *"up -d"*) exit 0 ;;`+"\n"))

	res, err := e.Check(context.Background(), newTarget(t))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Available {
		t.Errorf("result = %+v, want the new image reported as available", res)
	}
}
