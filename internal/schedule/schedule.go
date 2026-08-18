package schedule

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

const startupJitter = 90 * time.Second

type Checker interface {
	Check(ctx context.Context, target string) (wire.CheckResult, error)
}

type Starter interface {
	Start(t *config.Target, req job.Request) (*job.Job, error)
}

type Pending struct {
	Since    time.Time
	Services []string
}

func (p Pending) ReadyAt(soak time.Duration) time.Time { return p.Since.Add(soak) }

type Scheduler struct {
	cfg     *config.Config
	checker Checker
	starter Starter
	log     *slog.Logger

	// jitter spreads the first check across hosts. A field rather than a
	// constant so tests can exercise the loop without waiting it out.
	jitter time.Duration

	mu        sync.Mutex
	pending   map[string]Pending
	nextCheck map[string]time.Time
	lastCheck map[string]time.Time
}

func New(cfg *config.Config, checker Checker, starter Starter, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:       cfg,
		checker:   checker,
		starter:   starter,
		log:       log,
		jitter:    startupJitter,
		pending:   make(map[string]Pending),
		nextCheck: make(map[string]time.Time),
		lastCheck: make(map[string]time.Time),
	}
}

func (s *Scheduler) Managed() []string {
	var names []string
	for _, t := range s.cfg.AutoUpdateTargets() {
		names = append(names, t.Name)
	}
	return names
}

func (s *Scheduler) PendingFor(target string) (Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[target]
	return p, ok
}

// Timing reports when this target was last checked for a new image and when it
// will be checked next. Without it there is no way to answer "how long until it
// looks again", because the first check is jittered and the ticker keeps that
// offset for the life of the process.
func (s *Scheduler) Timing(target string) (last, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCheck[target], s.nextCheck[target]
}

func (s *Scheduler) setNext(target string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextCheck[target] = at
}

func (s *Scheduler) setLast(target string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCheck[target] = at
}

func (s *Scheduler) Run(ctx context.Context) {
	targets := s.cfg.AutoUpdateTargets()
	if len(targets) == 0 {
		return
	}

	s.log.Info("auto update scheduler started", "targets", s.Managed())

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t *config.Target) {
			defer wg.Done()
			s.watch(ctx, t)
		}(t)
	}
	wg.Wait()
}

func (s *Scheduler) watch(ctx context.Context, t *config.Target) {
	var delay time.Duration
	if s.jitter > 0 {
		delay = time.Duration(rand.Int64N(int64(s.jitter)))
	}
	s.setNext(t.Name, time.Now().Add(delay))
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	ticker := time.NewTicker(t.CheckInterval)
	defer ticker.Stop()

	for {
		s.setLast(t.Name, time.Now())
		s.tick(ctx, t)
		s.setNext(t.Name, time.Now().Add(t.CheckInterval))

		// A soak expiring before the next check needs its own wake-up. Waiting
		// only on the ticker means an update that finished soaking sits until
		// the next interval, so a 10m soak under a 1h check_interval applies up
		// to 50 minutes late.
		var (
			soakC     <-chan time.Time
			soakTimer *time.Timer
		)
		if p, ok := s.PendingFor(t.Name); ok {
			if d := time.Until(p.ReadyAt(t.SoakWindow())); d > 0 {
				soakTimer = time.NewTimer(d)
				soakC = soakTimer.C
			}
		}

		select {
		case <-ctx.Done():
			if soakTimer != nil {
				soakTimer.Stop()
			}
			return
		case <-ticker.C:
		case <-soakC:
		}
		if soakTimer != nil {
			soakTimer.Stop()
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, t *config.Target) {
	checkCtx, cancel := context.WithTimeout(ctx, t.PullTimeout+time.Minute)
	defer cancel()

	result, err := s.checker.Check(checkCtx, t.Name)
	if err != nil {
		s.log.Error("auto update check failed", "target", t.Name, "error", err)
		return
	}

	if !result.Available {
		if s.clear(t.Name) {
			s.log.Info("auto update no longer pending", "target", t.Name, "reason", result.Message)
		}
		return
	}

	pending, existed := s.mark(t.Name, result.Changed)
	ready := pending.ReadyAt(t.SoakWindow())

	if !existed {
		s.log.Info("auto update pending",
			"target", t.Name, "changed", result.Changed,
			"soak", t.SoakWindow().String(), "applies_at", ready.UTC().Format(time.RFC3339))
		if t.SoakWindow() > 0 {
			return
		}
	}

	if time.Now().Before(ready) {
		return
	}

	s.apply(t, result.Changed)
}

func (s *Scheduler) apply(t *config.Target, changed []string) {
	j, err := s.starter.Start(t, job.Request{
		Trigger: "auto",
		Reason:  "auto update after " + t.SoakWindow().String() + " soak",
	})
	if err != nil {
		s.log.Warn("auto update could not start", "target", t.Name, "error", err)
		return
	}

	s.clear(t.Name)
	s.log.Info("auto update started", "target", t.Name, "job", j.ID(), "changed", changed)
}

func (s *Scheduler) mark(target string, services []string) (Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.pending[target]; ok {
		return existing, true
	}
	p := Pending{Since: time.Now(), Services: services}
	s.pending[target] = p
	return p, false
}

func (s *Scheduler) clear(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, existed := s.pending[target]
	delete(s.pending, target)
	return existed
}
