package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/job"
)

func snap(id, target string, state job.State) job.Snapshot {
	fin := time.Now()
	return job.Snapshot{
		ID: id, Target: target, State: state,
		Message: "did the thing", Trigger: "api",
		Changed:    []string{"web"},
		Steps:      []job.Step{{Name: "pull", OK: true}, {Name: "up", OK: true}},
		StartedAt:  fin.Add(-time.Minute),
		FinishedAt: &fin,
		DurationMS: 60000,
	}
}

func openAt(t *testing.T, path string, maxBytes int64, keep int) *Writer {
	t.Helper()
	w, err := Open(Config{Path: path, MaxBytes: maxBytes, Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestAppendAndReadNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	w := openAt(t, path, 1<<20, 3)

	for i := range 5 {
		if err := w.Append(snap(fmt.Sprintf("job-%d", i), "web", job.StateSucceeded)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Read(path, Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d jobs, want 5", len(got))
	}
	if got[0].ID != "job-4" {
		t.Errorf("first result is %s, want the newest job-4", got[0].ID)
	}
	if got[0].Steps[0].Name != "pull" {
		t.Error("the step log did not survive the round trip")
	}
}

// The whole point: a restart must not lose what dup did.
func TestSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")

	w1 := openAt(t, path, 1<<20, 3)
	_ = w1.Append(snap("before", "web", job.StateSucceeded))
	_ = w1.Close()

	w2 := openAt(t, path, 1<<20, 3)
	_ = w2.Append(snap("after", "web", job.StateSucceeded))

	got, _ := Read(path, Query{Limit: 10})
	if len(got) != 2 {
		t.Fatalf("got %d jobs across a reopen, want 2", len(got))
	}
}

func TestFilterByTargetAndJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	w := openAt(t, path, 1<<20, 3)
	_ = w.Append(snap("a1", "web", job.StateSucceeded))
	_ = w.Append(snap("b1", "db", job.StateFailed))
	_ = w.Append(snap("a2", "web", job.StateRolledBack))

	got, _ := Read(path, Query{Target: "web", Limit: 10})
	if len(got) != 2 {
		t.Fatalf("target filter returned %d, want 2", len(got))
	}
	for _, s := range got {
		if s.Target != "web" {
			t.Errorf("target filter leaked %s", s.Target)
		}
	}

	one, _ := Read(path, Query{JobID: "b1", Limit: 10})
	if len(one) != 1 || one[0].Target != "db" {
		t.Errorf("job id filter returned %#v", one)
	}
}

func TestLimitStopsEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	w := openAt(t, path, 1<<20, 3)
	for i := range 20 {
		_ = w.Append(snap(fmt.Sprintf("j%02d", i), "web", job.StateSucceeded))
	}
	got, _ := Read(path, Query{Limit: 3})
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if got[0].ID != "j19" {
		t.Errorf("limit did not take the newest, got %s", got[0].ID)
	}
}

// Rotation must not hide history: a read has to walk the archives too, newest
// file first, and still come back in newest-first order overall.
func TestReadsAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	// A record is ~400 bytes, so 1200 holds three per file and Keep 4 gives a
	// live file plus four archives: room for 15, comfortably more than the 12
	// written, so nothing should be pruned.
	w := openAt(t, path, 1200, 4)

	for i := range 12 {
		if err := w.Append(snap(fmt.Sprintf("j%02d", i), "web", job.StateSucceeded)); err != nil {
			t.Fatal(err)
		}
	}

	if matches, _ := filepath.Glob(path + ".*.gz"); len(matches) == 0 {
		t.Fatal("expected rotation to have happened")
	}

	got, err := Read(path, Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 {
		t.Fatalf("got %d jobs across rotation, want all 12", len(got))
	}
	if got[0].ID != "j11" {
		t.Errorf("newest overall is %s, want j11", got[0].ID)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].StartedAt.Before(got[i].StartedAt) {
			t.Errorf("results are not newest first at %d", i)
		}
	}
}

// A running job is not an outcome, so it must not be recorded.
func TestRunningJobIsNotRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	w := openAt(t, path, 1<<20, 3)
	if err := w.Append(snap("live", "web", job.StateRunning)); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(path, Query{Limit: 10}); len(got) != 0 {
		t.Errorf("a running job was recorded: %#v", got)
	}
}

// A hard kill can leave a partial final line. It must not poison the read.
func TestTruncatedTailIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	w := openAt(t, path, 1<<20, 3)
	_ = w.Append(snap("good", "web", job.StateSucceeded))
	_ = w.Close()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"trunc","target":"we`)
	_ = f.Close()

	got, err := Read(path, Query{Limit: 10})
	if err != nil {
		t.Fatalf("a truncated tail broke the read: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Errorf("got %#v, want just the good record", got)
	}
}

func TestReadMissingFileIsNotAnError(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "nothing.jsonl"), Query{Limit: 5})
	if err != nil {
		t.Fatalf("reading a missing history errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records from nothing", len(got))
	}
}

func TestNilWriterIsSafe(t *testing.T) {
	var w *Writer
	if err := w.Append(snap("x", "web", job.StateSucceeded)); err != nil {
		t.Errorf("Append on a nil writer errored: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close on a nil writer errored: %v", err)
	}
}

func TestArchiveOrdering(t *testing.T) {
	path := "/var/lib/dup/history.jsonl"
	got := files(path)
	if got[0] != path {
		t.Errorf("live file should be read first, got %s", got[0])
	}
	if idx := archiveIndex(path+".10.gz", path); idx != 10 {
		t.Errorf("archiveIndex = %d, want 10 (string sort would place 10 before 2)", idx)
	}
	if idx := archiveIndex(path+".notanumber.gz", path); idx != 1<<30 {
		t.Errorf("a non-numeric archive should sort last, got %d", idx)
	}
	if !strings.HasSuffix(path, ".jsonl") {
		t.Fatal("unreachable")
	}
}
