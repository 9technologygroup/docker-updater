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

func TestCompletedStepReplacesItsRunningPlaceholder(t *testing.T) {
	s := NewStore()
	j, _ := s.Begin("pmon", Request{})

	j.StartStep("pull")
	if steps := j.Snapshot().Steps; len(steps) != 1 || !steps[0].Running {
		t.Fatalf("steps after StartStep = %+v, want one running step", steps)
	}

	j.AddStep(Step{Name: "pull", OK: true, Output: "pulled acme/web:2"})
	j.StartStep("up")
	j.AddStep(Step{Name: "up", OK: true})

	steps := j.Snapshot().Steps
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want exactly two", steps)
	}
	if steps[0].Running || !steps[0].OK || steps[0].Output != "pulled acme/web:2" {
		t.Errorf("step[0] = %+v, want the completed pull, not the placeholder", steps[0])
	}
	if steps[1].Name != "up" || steps[1].Running {
		t.Errorf("step[1] = %+v", steps[1])
	}
}

func TestUnfinishedStepIsResolvedWhenTheJobEnds(t *testing.T) {
	s := NewStore()
	j, _ := s.Begin("pmon", Request{})

	j.AddStep(Step{Name: "pull", OK: true})
	j.StartStep("up")
	s.Complete(j, StateFailed, "the agent went away")

	steps := j.Snapshot().Steps
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want exactly two", steps)
	}
	last := steps[1]
	if last.Running {
		t.Error("a step must not still report as running once the job is terminal")
	}
	if last.OK {
		t.Error("a step that never reported back is not a success")
	}
	if last.Error != "did not complete" {
		t.Errorf("error = %q, want %q", last.Error, "did not complete")
	}
}
