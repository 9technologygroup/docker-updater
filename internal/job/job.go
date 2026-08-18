package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	maxJobLogBytes = 96 << 10
	defaultHistory = 200
)

var ErrBusy = errors.New("an update is already running for this target")

type State string

const (
	StateRunning        State = "running"
	StateSucceeded      State = "succeeded"
	StateNoChange       State = "no_change"
	StateDryRun         State = "dry_run"
	StateFailed         State = "failed"
	StateRolledBack     State = "rolled_back"
	StateRollbackFailed State = "rollback_failed"
)

func (s State) Terminal() bool { return s != StateRunning }

func (s State) OK() bool {
	return s == StateSucceeded || s == StateNoChange || s == StateDryRun
}

type Sink interface {
	AddStep(Step)
	SetBefore([]ServiceState)
	SetAfter([]ServiceState)
	SetChanged([]string)
}

type Request struct {
	Tag     string
	Reason  string
	Trigger string
	DryRun  bool
	Force   bool
}

type Step struct {
	Name       string    `json:"name"`
	Command    string    `json:"command,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Output     string    `json:"output,omitempty"`
}

type ServiceState struct {
	Service   string `json:"service"`
	Container string `json:"container,omitempty"`
	Image     string `json:"image,omitempty"`
	ImageID   string `json:"image_id,omitempty"`
	State     string `json:"state,omitempty"`
	Health    string `json:"health,omitempty"`
}

type Snapshot struct {
	ID           string         `json:"id"`
	Target       string         `json:"target"`
	State        State          `json:"state"`
	Message      string         `json:"message,omitempty"`
	Trigger      string         `json:"trigger,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	RequestedTag string         `json:"requested_tag,omitempty"`
	DryRun       bool           `json:"dry_run,omitempty"`
	Changed      []string       `json:"changed_services,omitempty"`
	Before       []ServiceState `json:"before,omitempty"`
	After        []ServiceState `json:"after,omitempty"`
	Steps        []Step         `json:"steps,omitempty"`
	StartedAt    time.Time      `json:"started_at"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
	DurationMS   int64          `json:"duration_ms"`
}

type Job struct {
	mu sync.Mutex

	id           string
	target       string
	state        State
	message      string
	trigger      string
	reason       string
	requestedTag string
	dryRun       bool
	changed      []string
	before       []ServiceState
	after        []ServiceState
	steps        []Step
	logBytes     int
	startedAt    time.Time
	finishedAt   *time.Time

	done chan struct{}
}

func newJob(target string, req Request) *Job {
	return &Job{
		id:           newID(),
		target:       target,
		state:        StateRunning,
		trigger:      req.Trigger,
		reason:       sanitise(req.Reason, 200),
		requestedTag: req.Tag,
		dryRun:       req.DryRun,
		startedAt:    time.Now().UTC(),
		done:         make(chan struct{}),
	}
}

func (j *Job) ID() string     { return j.id }
func (j *Job) Target() string { return j.target }
func (j *Job) Done() <-chan struct{} {
	return j.done
}

func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

func (j *Job) AddStep(s Step) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s.Output = sanitise(s.Output, maxJobLogBytes)
	if j.logBytes+len(s.Output) > maxJobLogBytes {
		remaining := maxJobLogBytes - j.logBytes
		if remaining < 0 {
			remaining = 0
		}
		if len(s.Output) > remaining {
			s.Output = s.Output[:remaining] + "\n…[job log truncated]…"
		}
	}
	j.logBytes += len(s.Output)
	j.steps = append(j.steps, s)
}

func (j *Job) SetBefore(states []ServiceState) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.before = states
}

func (j *Job) SetAfter(states []ServiceState) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.after = states
}

func (j *Job) SetChanged(services []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.changed = services
}

func (j *Job) finish(state State, message string) {
	j.mu.Lock()
	if j.state.Terminal() {
		j.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	j.state = state
	j.message = sanitise(message, 1000)
	j.finishedAt = &now
	j.mu.Unlock()
	close(j.done)
}

func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()

	s := Snapshot{
		ID:           j.id,
		Target:       j.target,
		State:        j.state,
		Message:      j.message,
		Trigger:      j.trigger,
		Reason:       j.reason,
		RequestedTag: j.requestedTag,
		DryRun:       j.dryRun,
		Changed:      append([]string(nil), j.changed...),
		Before:       append([]ServiceState(nil), j.before...),
		After:        append([]ServiceState(nil), j.after...),
		Steps:        append([]Step(nil), j.steps...),
		StartedAt:    j.startedAt,
		FinishedAt:   j.finishedAt,
	}
	end := time.Now().UTC()
	if j.finishedAt != nil {
		end = *j.finishedAt
	}
	s.DurationMS = end.Sub(j.startedAt).Milliseconds()
	return s
}

type Store struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	running map[string]*Job
	history int
}

func NewStore() *Store {
	return &Store{
		jobs:    make(map[string]*Job),
		running: make(map[string]*Job),
		history: defaultHistory,
	}
}

func (s *Store) Begin(target string, req Request) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, busy := s.running[target]; busy {
		return existing, ErrBusy
	}

	j := newJob(target, req)
	s.running[target] = j
	s.jobs[j.id] = j
	s.order = append(s.order, j.id)

	for len(s.order) > s.history {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.jobs, oldest)
	}
	return j, nil
}

func (s *Store) Complete(j *Job, state State, message string) {
	j.finish(state, message)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.running[j.target]; ok && cur.id == j.id {
		delete(s.running, j.target)
	}
}

func (s *Store) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

func (s *Store) Running(target string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.running[target]
	return j, ok
}

func (s *Store) List(target string, limit int) []Snapshot {
	s.mu.Lock()
	jobs := make([]*Job, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		j, ok := s.jobs[s.order[i]]
		if !ok {
			continue
		}
		if target != "" && j.target != target {
			continue
		}
		jobs = append(jobs, j)
		if limit > 0 && len(jobs) >= limit {
			break
		}
	}
	s.mu.Unlock()

	out := make([]Snapshot, 0, len(jobs))
	for _, j := range jobs {
		snap := j.Snapshot()
		snap.Steps = nil
		snap.Before = nil
		snap.After = nil
		out = append(out, snap)
	}
	return out
}

func (s *Store) DrainRunning(ctx context.Context) {
	for {
		s.mu.Lock()
		var pending *Job
		for _, j := range s.running {
			pending = j
			break
		}
		s.mu.Unlock()

		if pending == nil {
			return
		}
		select {
		case <-pending.Done():
		case <-ctx.Done():
			return
		}
	}
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

func sanitise(s string, max int) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
		case r < 0x20 || r == 0x7f:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if max > 0 && len(out) > max {
		out = out[:max] + "…"
	}
	return out
}
