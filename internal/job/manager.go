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

// Recorder persists a finished job. The in-memory store is bounded and dies with
// the process, so this is what makes history survive a restart.
type Recorder interface {
	Append(snap Snapshot) error
}

type Backend interface {
	Update(ctx context.Context, t *config.Target, req Request, sink Sink) (State, string, error)
}

type Manager struct {
	store    *Store
	backend  Backend
	notifier Notifier
	recorder Recorder
	onDone   func(target string, snap Snapshot)
	log      *slog.Logger
}

// OnComplete fires when any update finishes, whoever asked for it. The scheduler
// uses it to drop a pending soak that somebody has already applied by hand.
func (m *Manager) OnComplete(fn func(target string, snap Snapshot)) *Manager {
	m.onDone = fn
	return m
}

func (m *Manager) WithRecorder(r Recorder) *Manager {
	m.recorder = r
	return m
}

func NewManager(backend Backend, store *Store, notifier Notifier, log *slog.Logger) *Manager {
	return &Manager{store: store, backend: backend, notifier: notifier, log: log}
}

func (m *Manager) Store() *Store { return m.store }

// Busy reports whether an update is already running for this target, so a
// caller can avoid asking the agent something it will refuse.
func (m *Manager) Busy(target string) bool {
	_, busy := m.store.Running(target)
	return busy
}

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

	if m.onDone != nil {
		m.onDone(snap.Target, snap)
	}

	if m.recorder != nil {
		if err := m.recorder.Append(snap); err != nil {
			m.log.Warn("could not record job history", "job", snap.ID, "error", err)
		}
	}

	if m.notifier != nil {
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), time.Minute)
		defer notifyCancel()
		m.notifier.Notify(notifyCtx, snap)
	}
}
