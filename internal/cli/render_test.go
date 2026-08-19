package cli

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/job"
)

func running(name string, ago time.Duration) job.Step {
	return job.Step{Name: name, StartedAt: time.Now().UTC().Add(-ago), Running: true}
}

func done(name string, ms int64, ok bool, errText string) job.Step {
	return job.Step{Name: name, StartedAt: time.Now().UTC(), DurationMS: ms, OK: ok, Error: errText}
}

func snapshot(state job.State, steps ...job.Step) job.Snapshot {
	return job.Snapshot{
		ID: "18c4fe391aeb4dfd", Target: "quackback", State: state,
		StartedAt: time.Now().UTC(), Steps: steps,
	}
}

func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// The non-TTY path is what lands in a log file, so a step appearing twice or out
// of order is the failure mode that matters.
func TestNonTTYRendererEmitsEachStepOnceInOrder(t *testing.T) {
	var buf bytes.Buffer
	r := newJobRenderer(&buf, false)

	r.update(snapshot(job.StateRunning))
	r.update(snapshot(job.StateRunning, running("validate", time.Second)))
	r.update(snapshot(job.StateRunning, done("validate", 300, true, ""), running("pull", 2*time.Second)))
	r.update(snapshot(job.StateRunning, done("validate", 300, true, ""), done("pull", 58200, true, ""), running("up", time.Second)))

	final := snapshot(job.StateSucceeded, done("validate", 300, true, ""), done("pull", 58200, true, ""), done("up", 6000, true, ""))
	final.Message = "updated app"
	final.Changed = []string{"app"}
	final.DurationMS = 70008
	r.finish(final)

	got := lines(buf.String())
	want := []string{
		"quackback  job 18c4fe391aeb4dfd started",
		"    validate                 ok     300ms",
		"    pull                     ok     58.2s",
		"    up                       ok     6.0s",
		"quackback  succeeded",
		"  updated app",
		"  changed: app",
		"  job 18c4fe391aeb4dfd in 1m10.008s",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNonTTYRendererHoldsBackARunningStep(t *testing.T) {
	var buf bytes.Buffer
	r := newJobRenderer(&buf, false)

	r.update(snapshot(job.StateRunning, done("validate", 300, true, ""), running("pull", 3*time.Second)))
	if strings.Contains(buf.String(), "pull") {
		t.Fatalf("a step still running was printed as finished:\n%s", buf.String())
	}

	r.update(snapshot(job.StateRunning, done("validate", 300, true, ""), done("pull", 3100, true, "")))
	if n := strings.Count(buf.String(), "validate"); n != 1 {
		t.Errorf("validate printed %d times, want 1:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "pull"); n != 1 {
		t.Errorf("pull printed %d times, want 1:\n%s", n, buf.String())
	}
}

func TestNonTTYRendererPrintsAFailedStepAndItsError(t *testing.T) {
	var buf bytes.Buffer
	r := newJobRenderer(&buf, false)

	final := snapshot(job.StateFailed, done("pull", 2100, false, "manifest unknown"))
	final.Message = "pull failed"
	final.DurationMS = 2400
	r.finish(final)

	got := lines(buf.String())
	want := []string{
		"quackback  job 18c4fe391aeb4dfd started",
		"    pull                     FAILED 2.1s",
		"      manifest unknown",
		"quackback  failed",
		"  pull failed",
		"  job 18c4fe391aeb4dfd in 2.4s",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStepLineFlipsFromPendingToItsOutcome(t *testing.T) {
	now := time.Now()
	pending := stepLine(job.Step{Name: "pull", StartedAt: now.Add(-38 * time.Second), Running: true}, now)
	if !strings.Contains(pending, "...") || !strings.Contains(pending, "38.0s") {
		t.Errorf("running step rendered as %q, want a pending marker and a live elapsed time", pending)
	}

	ok := stepLine(job.Step{Name: "pull", DurationMS: 38400, OK: true}, now)
	if !strings.Contains(ok, " ok ") || !strings.Contains(ok, "38.4s") {
		t.Errorf("completed step rendered as %q, want ok and its final duration", ok)
	}

	failed := stepLine(job.Step{Name: "pull", DurationMS: 900}, now)
	if !strings.Contains(failed, "FAILED") || !strings.Contains(failed, "900ms") {
		t.Errorf("failed step rendered as %q, want FAILED and its final duration", failed)
	}
}

func TestJobSummaryKeepsItsShape(t *testing.T) {
	snap := snapshot(job.StateSucceeded)
	snap.Message = "updated app"
	snap.Changed = []string{"app", "worker"}
	snap.DurationMS = 70008

	want := []string{
		"quackback  succeeded",
		"  updated app",
		"  changed: app, worker",
		"  job 18c4fe391aeb4dfd in 1m10.008s",
	}
	got := jobSummaryLines(snap, false)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	bare := jobSummaryLines(snapshot(job.StateRunning), false)
	if len(bare) != 2 {
		t.Errorf("a job with no message and nothing changed rendered %d lines, want 2: %q", len(bare), bare)
	}
}

func TestTTYRendererRedrawsOneBlockInPlace(t *testing.T) {
	var buf bytes.Buffer
	r := newJobRenderer(&buf, true)

	r.update(snapshot(job.StateRunning, running("validate", time.Second)))
	first := buf.String()
	if strings.Contains(first, "\x1b[") && strings.Contains(first, "A") && strings.Contains(first, "\x1b[0A") {
		t.Error("the first draw moved the cursor up, but there is nothing above it yet")
	}
	if r.lines != 3 {
		t.Fatalf("tracked %d lines after the first draw, want 3", r.lines)
	}

	buf.Reset()
	r.update(snapshot(job.StateRunning, done("validate", 300, true, ""), running("pull", time.Second)))
	second := buf.String()
	if !strings.HasPrefix(second, "\x1b[3A") {
		t.Errorf("the redraw did not move up over the 3 lines it drew before: %q", second)
	}
	if strings.Count(second, "\x1b[2K") != 4 {
		t.Errorf("the redraw cleared %d lines, want 4: %q", strings.Count(second, "\x1b[2K"), second)
	}

	buf.Reset()
	final := snapshot(job.StateSucceeded, done("validate", 300, true, ""), done("pull", 3100, true, ""))
	final.DurationMS = 3400
	r.finish(final)
	if !strings.HasPrefix(buf.String(), "\x1b[4A") {
		t.Errorf("the final block did not overwrite the live one: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "quackback  succeeded") {
		t.Errorf("the final block lost its summary line: %q", buf.String())
	}
	if r.lines != 0 {
		t.Errorf("the renderer is still tracking %d lines after finishing", r.lines)
	}
}

// A long or multi-line value in the block would wrap and desynchronise the
// cursor maths, so live lines are folded onto one screen line each.
func TestLiveLinesAreFoldedToOneScreenLineEach(t *testing.T) {
	snap := snapshot(job.StateRunning, job.Step{
		Name: "pull", DurationMS: 100, Error: "first line\nsecond line" + strings.Repeat("x", 200),
	})
	for _, l := range jobStepLines(snap, time.Now(), true) {
		if strings.ContainsAny(l, "\n\t") {
			t.Errorf("live line still contains a newline or tab: %q", l)
		}
		if len(l) > liveWidth {
			t.Errorf("live line is %d chars, want at most %d: %q", len(l), liveWidth, l)
		}
	}
	if got := jobStepLines(snap, time.Now(), false); !strings.Contains(got[1], "\n") {
		t.Error("the final block should keep the full error text intact")
	}
}

func TestBriefDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                        "0ms",
		300 * time.Millisecond:   "300ms",
		time.Second:              "1.0s",
		38400 * time.Millisecond: "38.4s",
		70 * time.Second:         "1m10s",
		90 * time.Minute:         "1h30m",
	}
	for d, want := range cases {
		if got := briefDuration(d); got != want {
			t.Errorf("briefDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestClassifyScan(t *testing.T) {
	cases := []struct {
		name        string
		out         scanResult
		err         error
		wantOutcome scanOutcome
		wantResult  string
	}{
		{"up to date", scanResult{Message: "app is current"}, nil, scanUpToDate, "up to date"},
		{"available", scanResult{Available: true, Changed: []string{"app"}}, nil, scanAvailable, "update available"},
		{"busy on 409", scanResult{}, &apiStatusError{status: http.StatusConflict}, scanBusy, "busy"},
		{"busy on 503", scanResult{}, &apiStatusError{status: http.StatusServiceUnavailable}, scanBusy, "busy"},
		{"failed on 500", scanResult{}, &apiStatusError{status: http.StatusInternalServerError}, scanFailed, "check FAILED"},
		{"failed on transport", scanResult{}, errors.New("connection refused"), scanFailed, "check FAILED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, result, services, _ := classifyScan(c.out, c.err)
			if outcome != c.wantOutcome {
				t.Errorf("outcome = %d, want %d", outcome, c.wantOutcome)
			}
			if result != c.wantResult {
				t.Errorf("result = %q, want %q", result, c.wantResult)
			}
			if c.wantOutcome == scanAvailable && services != "app" {
				t.Errorf("services = %q, want the changed service", services)
			}
		})
	}
}

func TestScanTableNonTTYPrintsOneRowPerTarget(t *testing.T) {
	var buf bytes.Buffer
	table := newScanTable(&buf, false, []string{"quackback", "web"})
	table.header()

	table.checking("quackback")
	table.result("quackback", "update available", "app", "app: new image")
	table.checking("web")
	table.result("web", "up to date", "-", "")

	got := lines(buf.String())
	want := []string{
		"STACK      RESULT            SERVICES                  DETAIL",
		"quackback  update available  app                       app: new image",
		"web        up to date        -",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Error("the non-TTY table emitted an ANSI escape")
	}
	if strings.Contains(buf.String(), "checking") {
		t.Error("the non-TTY table printed a placeholder row")
	}
}

func TestScanTableTTYRewritesThePlaceholderRow(t *testing.T) {
	var buf bytes.Buffer
	table := newScanTable(&buf, true, []string{"quackback"})

	table.checking("quackback")
	if !strings.Contains(buf.String(), "checking...") {
		t.Fatalf("no placeholder row was drawn: %q", buf.String())
	}

	buf.Reset()
	table.result("quackback", "up to date", "-", "")
	if !strings.HasPrefix(buf.String(), "\x1b[1A\r\x1b[2K") {
		t.Errorf("the result did not overwrite the placeholder row: %q", buf.String())
	}
	if strings.Contains(buf.String(), "checking") {
		t.Errorf("the placeholder survived into the final row: %q", buf.String())
	}
}
