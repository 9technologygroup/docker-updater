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

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/wire"
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

// The bug this guards: the soak was only ever tested when the ticker fired, so
// a short soak under a long check_interval applied at the next interval instead
// of when it was due. On a live host a 10m soak under a 1h interval sat for
// nearly an hour past its stated apply time.
func TestSoakAppliesWithoutWaitingForTheNextCheck(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, 150*time.Millisecond, checker, starter)

	// An interval far longer than the soak, mirroring check_interval: 1h with
	// soak: 10m. If the update waits for the ticker, this test times out.
	target.CheckInterval = time.Hour
	s.jitter = 0

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watch(ctx, target)
	}()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if starter.count() > 0 {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("the update never applied; it waited for the next check_interval rather than the soak")
}

// The soak must still be honoured: applying early is as wrong as applying late.
func TestSoakIsNotSkipped(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, 30*time.Second, checker, starter)
	target.CheckInterval = time.Hour
	s.jitter = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watch(ctx, target)
	}()

	time.Sleep(300 * time.Millisecond)
	got := starter.count()
	cancel()
	<-done

	if got != 0 {
		t.Errorf("applied %d times while still soaking, want 0", got)
	}
}

type busyStarter struct {
	fakeStarter
	busy bool
}

func (b *busyStarter) Busy(string) bool { return b.busy }

// While an update is in flight there is nothing to learn, and asking would pull
// images for a stack that is mid-deploy.
func TestNoCheckWhileAnUpdateIsRunning(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"app"}}}
	starter := &busyStarter{busy: true}
	s, target := newScheduler(t, time.Minute, checker, starter)

	s.tick(context.Background(), target)

	checker.mu.Lock()
	calls := checker.calls
	checker.mu.Unlock()
	if calls != 0 {
		t.Errorf("asked the agent %d times while an update was running, want 0", calls)
	}
	if starter.count() != 0 {
		t.Error("started an update while one was already running")
	}
}

func TestChecksResumeOnceTheUpdateFinishes(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: false}}
	starter := &busyStarter{busy: false}
	s, target := newScheduler(t, time.Minute, checker, starter)

	s.tick(context.Background(), target)

	checker.mu.Lock()
	calls := checker.calls
	checker.mu.Unlock()
	if calls != 1 {
		t.Errorf("checked %d times when idle, want 1", calls)
	}
}

type refusedChecker struct{ status int }

func (r *refusedChecker) Check(context.Context, string) (wire.CheckResult, error) {
	return wire.CheckResult{}, &fakeStatusErr{status: r.status}
}

type fakeStatusErr struct{ status int }

func (e *fakeStatusErr) StatusCode() int { return e.status }
func (e *fakeStatusErr) Error() string   { return "refused" }

// A 409 from the agent is contention working as designed. Logging it as an
// error sent people hunting for a fault that was not there.
func TestAgentRefusalIsNotTreatedAsAFailure(t *testing.T) {
	for _, status := range []int{409, 503} {
		starter := &fakeStarter{}
		s, target := newScheduler(t, time.Minute, &refusedChecker{status: status}, starter)

		s.tick(context.Background(), target)

		if _, ok := s.PendingFor("web"); ok {
			t.Errorf("status %d left pending state behind", status)
		}
		if starter.count() != 0 {
			t.Errorf("status %d started an update", status)
		}
	}
}

// A manual "dup update" bypasses the scheduler entirely, so nothing used to drop
// the soak it was advertising. On a 12h check_interval that left dup list
// claiming an update was still pending for half a day after it had been applied.
func TestClearPendingDropsASoakSomebodyElseApplied(t *testing.T) {
	checker := &fakeChecker{result: wire.CheckResult{Available: true, Changed: []string{"npmplus"}}}
	starter := &fakeStarter{}
	s, target := newScheduler(t, 12*time.Hour, checker, starter)

	s.tick(context.Background(), target)
	if _, ok := s.PendingFor("web"); !ok {
		t.Fatal("expected a pending soak to start with")
	}

	s.ClearPending("web")

	if _, ok := s.PendingFor("web"); ok {
		t.Error("the soak survived an update being applied elsewhere")
	}
	// Clearing something already gone must not panic or log noise.
	s.ClearPending("web")
	s.ClearPending("never-existed")
}
