package schedule

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/job"
	"github.com/PatchMon/docker-updater/internal/wire"
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

	mu      sync.Mutex
	pending map[string]Pending
}

func New(cfg *config.Config, checker Checker, starter Starter, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		checker: checker,
		starter: starter,
		log:     log,
		pending: make(map[string]Pending),
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
	delay := time.Duration(rand.Int64N(int64(startupJitter)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	ticker := time.NewTicker(t.CheckInterval)
	defer ticker.Stop()

	for {
		s.tick(ctx, t)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
