package job

import (
	"errors"
	"strings"
	"testing"
)

func TestStoreRejectsConcurrentUpdatesForSameTarget(t *testing.T) {
	s := NewStore()

	first, err := s.Begin("pmon", Request{})
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}

	second, err := s.Begin("pmon", Request{})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second Begin error = %v, want ErrBusy", err)
	}
	if second.ID() != first.ID() {
		t.Error("ErrBusy should hand back the job that is already running")
	}

	if _, err := s.Begin("website", Request{}); err != nil {
		t.Fatalf("a different target must not be blocked: %v", err)
	}

	s.Complete(first, StateSucceeded, "done")
	if _, err := s.Begin("pmon", Request{}); err != nil {
		t.Fatalf("Begin after completion: %v", err)
	}
}

func TestCompleteClosesDoneOnce(t *testing.T) {
	s := NewStore()
	j, err := s.Begin("pmon", Request{})
	if err != nil {
		t.Fatal(err)
	}

	s.Complete(j, StateSucceeded, "first")
	s.Complete(j, StateFailed, "second")

	<-j.Done()
	if got := j.State(); got != StateSucceeded {
		t.Errorf("state = %q, want the first terminal state to win", got)
	}
	if snap := j.Snapshot(); snap.Message != "first" {
		t.Errorf("message = %q, want %q", snap.Message, "first")
	}
}

func TestStateOK(t *testing.T) {
	ok := []State{StateSucceeded, StateNoChange, StateDryRun}
	for _, s := range ok {
		if !s.OK() {
			t.Errorf("%q should report OK", s)
		}
	}
	notOK := []State{StateFailed, StateRolledBack, StateRollbackFailed, StateRunning}
	for _, s := range notOK {
		if s.OK() {
			t.Errorf("%q should not report OK", s)
		}
	}
}

func TestJobLogIsCapped(t *testing.T) {
	s := NewStore()
	j, _ := s.Begin("pmon", Request{})

	for range 20 {
		j.AddStep(Step{Name: "noisy", Output: strings.Repeat("x", 20<<10)})
	}

	total := 0
	for _, step := range j.Snapshot().Steps {
		total += len(step.Output)
	}
	if total > maxJobLogBytes+1024 {
		t.Fatalf("job log grew to %d bytes, want it capped near %d", total, maxJobLogBytes)
	}
}

func TestSanitiseStripsControlCharacters(t *testing.T) {
	got := sanitise("release\r\n\x1b[31mfake log line\x00", 0)
	if strings.ContainsAny(got, "\r\x00\x1b") {
		t.Errorf("sanitise left control characters in %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Error("sanitise should keep newlines")
	}
}

func TestSanitiseTruncates(t *testing.T) {
	if got := sanitise(strings.Repeat("a", 500), 10); len(got) > 20 {
		t.Errorf("length = %d, want it truncated to about 10", len(got))
	}
}
