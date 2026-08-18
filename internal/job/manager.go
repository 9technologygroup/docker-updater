package job

import (
	"context"
	"log/slog"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
)

type Notifier interface {
	Notify(ctx context.Context, snap Snapshot)
}

type Backend interface {
	Update(ctx context.Context, t *config.Target, req Request, sink Sink) (State, string, error)
}

type Manager struct {
	store    *Store
	backend  Backend
	notifier Notifier
	log      *slog.Logger
}

func NewManager(backend Backend, store *Store, notifier Notifier, log *slog.Logger) *Manager {
	return &Manager{store: store, backend: backend, notifier: notifier, log: log}
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) Start(t *config.Target, req Request) (*Job, error) {
	j, err := m.store.Begin(t.Name, req)
	if err != nil {
		return j, err
	}
	go m.run(t, j, req)
	return j, nil
}

func (m *Manager) run(t *config.Target, j *Job, req Request) {
	ctx, cancel := context.WithTimeout(context.Background(), t.JobTimeout)
	defer cancel()

	state, message, err := m.backend.Update(ctx, t, req, j)
	if err != nil {
		state = StateFailed
		message = "lost contact with the update agent, the stack may be mid-update: " + err.Error()
	}
	m.store.Complete(j, state, message)

	snap := j.Snapshot()
	level := slog.LevelInfo
	if !state.OK() {
		level = slog.LevelError
	}
	m.log.Log(ctx, level, "update finished",
		"job", snap.ID, "target", snap.Target, "state", string(snap.State),
		"trigger", snap.Trigger, "changed", snap.Changed,
		"duration_ms", snap.DurationMS, "message", snap.Message)

	if m.notifier != nil {
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), time.Minute)
		defer notifyCancel()
		m.notifier.Notify(notifyCtx, snap)
	}
}
