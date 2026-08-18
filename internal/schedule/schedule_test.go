package schedule

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/job"
	"github.com/PatchMon/docker-updater/internal/wire"
)

type fakeChecker struct {
	mu     sync.Mutex
	result wire.CheckResult
	err    error
	calls  int
}

func (f *fakeChecker) Check(context.Context, string) (wire.CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, f.err
}

func (f *fakeChecker) set(r wire.CheckResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = r
}

type fakeStarter struct {
	mu      sync.Mutex
	started []string
}

func (f *fakeStarter) Start(t *config.Target, _ job.Request) (*job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, t.Name)

	store := job.NewStore()
	j, err := store.Begin(t.Name, job.Request{})
	return j, err
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func newScheduler(t *testing.T, soak time.Duration, checker Checker, starter Starter) (*Scheduler, *config.Target) {
	t.Helper()

	stack := t.TempDir()
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := "auth:\n  bearer_token: 0123456789abcdef0123456789abcdef\n" +
		"targets:\n  - name: web\n    dir: " + stack + "\n    soak: " + soak.String() + "\n"

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	target := cfg.Targets[0]
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, checker, starter, log), target
}

func TestSoakDelaysTheUpdate(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, time.Hour, checker, starter)

	s.tick(context.Background(), target)
	if starter.count() != 0 {
		t.Fatal("an update must not apply on the tick that first sees it; the soak has not elapsed")
	}

	pending, ok := s.PendingFor("web")
	if !ok {
		t.Fatal("the update should be recorded as pending")
	}
	if len(pending.Services) != 1 || pending.Services[0] != "app" {
		t.Errorf("pending services = %v", pending.Services)
	}

	s.tick(context.Background(), target)
	if starter.count() != 0 {
		t.Fatal("still inside the soak window, so it must not have applied")
	}
}

func TestUpdateAppliesOnceTheSoakHasElapsed(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, 50*time.Millisecond, checker, starter)

	s.tick(context.Background(), target)
	if starter.count() != 0 {
		t.Fatal("first sighting should only start the soak")
	}

	time.Sleep(80 * time.Millisecond)
	s.tick(context.Background(), target)

	if starter.count() != 1 {
		t.Fatalf("started %d updates, want 1 once the soak elapsed", starter.count())
	}
	if _, ok := s.PendingFor("web"); ok {
		t.Error("pending state should be cleared once the update starts")
	}
}

func TestZeroSoakAppliesImmediately(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, 0, checker, starter)

	s.tick(context.Background(), target)
	if starter.count() != 1 {
		t.Fatalf("started %d updates, want 1 when soak is zero", starter.count())
	}
}

func TestPendingClearsWhenTheUpdateDisappears(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, time.Hour, checker, starter)

	s.tick(context.Background(), target)
	if _, ok := s.PendingFor("web"); !ok {
		t.Fatal("should be pending")
	}

	checker.set(wire.CheckResult{Available: false, Message: "already up to date"})
	s.tick(context.Background(), target)

	if _, ok := s.PendingFor("web"); ok {
		t.Error("pending must clear when the new image is no longer offered, so the soak restarts next time")
	}
	if starter.count() != 0 {
		t.Error("nothing should have been applied")
	}
}

func TestCheckFailureDoesNotApplyOrClearPending(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, time.Hour, checker, starter)

	s.tick(context.Background(), target)

	checker.mu.Lock()
	checker.err = context.DeadlineExceeded
	checker.mu.Unlock()
	s.tick(context.Background(), target)

	if starter.count() != 0 {
		t.Error("a failed check must never trigger an update")
	}
	if _, ok := s.PendingFor("web"); !ok {
		t.Error("a failed check should leave the existing pending state alone, not reset the soak")
	}
}

func TestManagedListsOnlyAutoUpdateTargets(t *testing.T) {
	cfg := &config.Config{Targets: []*config.Target{
		{Name: "a", AutoUpdate: true},
		{Name: "b"},
		{Name: "c", AutoUpdate: true},
	}}
	s := New(cfg, &fakeChecker{}, &fakeStarter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got := s.Managed()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("Managed() = %v, want [a c]", got)
	}
}

func TestRunReturnsImmediatelyWithNoAutoTargets(t *testing.T) {
	cfg := &config.Config{Targets: []*config.Target{{Name: "a"}}}
	s := New(cfg, &fakeChecker{}, &fakeStarter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run should return at once when nothing is on auto update")
	}
}
