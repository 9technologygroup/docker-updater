package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/job"
)

const (
	fallbackWidth = 78
	minWidth      = 40
	stepNameCol   = 24
	stepMarkCol   = 6
	scanResultCol = 16

	maxStepOutputLines = 12
	maxProgressLines   = 12
)

func stdoutIsTTY() bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// os.ModeCharDevice is not enough here: /dev/null is a character device too,
// so it would report a redirected stdin as a terminal and prompt for a password.
func stdinIsTTY() bool {
	_, err := getTermState(int(os.Stdin.Fd()))
	return err == nil
}

// jobRenderer draws a running job as it goes: one block redrawn in place on a
// terminal, and each finished step emitted once anywhere else.
type jobRenderer struct {
	w       io.Writer
	tty     bool
	lines   int
	emitted int
	started bool
}

func newJobRenderer(w io.Writer, tty bool) *jobRenderer {
	return &jobRenderer{w: w, tty: tty}
}

func (r *jobRenderer) update(snap job.Snapshot) {
	if r.tty {
		r.draw(append(jobSummaryLines(snap, true), jobStepLines(snap, time.Now(), true)...))
		return
	}
	r.begin(snap)
	r.emitSteps(snap)
}

func (r *jobRenderer) finish(snap job.Snapshot) {
	if r.tty {
		// Printed rather than drawn: the final block is unclipped so a long
		// message survives whole, and nothing is redrawn over it afterwards.
		r.erase()
		for _, l := range append(jobSummaryLines(snap, false), jobStepLines(snap, time.Now(), false)...) {
			_, _ = fmt.Fprintln(r.w, l)
		}
		return
	}
	r.begin(snap)
	r.emitSteps(snap)
	for _, l := range jobSummaryLines(snap, false) {
		_, _ = fmt.Fprintln(r.w, l)
	}
}

func (r *jobRenderer) begin(snap job.Snapshot) {
	if r.started {
		return
	}
	r.started = true
	_, _ = fmt.Fprintf(r.w, "%s  job %s started\n", snap.Target, snap.ID)
}

func (r *jobRenderer) emitSteps(snap job.Snapshot) {
	now := time.Now()
	for i := r.emitted; i < len(snap.Steps); i++ {
		s := snap.Steps[i]
		if s.Running {
			return
		}
		_, _ = fmt.Fprintln(r.w, stepLine(s, now))
		for _, l := range stepDetail(s) {
			_, _ = fmt.Fprintln(r.w, l)
		}
		r.emitted = i + 1
	}
}

func (r *jobRenderer) draw(lines []string) {
	r.erase()
	for _, l := range lines {
		_, _ = fmt.Fprintf(r.w, "%s\n", l)
	}
	r.lines = len(lines)
}

// erase clears the whole previous block rather than one row per logical line,
// so anything that did wrap is still removed.
func (r *jobRenderer) erase() {
	if r.lines == 0 {
		return
	}
	_, _ = fmt.Fprintf(r.w, "\x1b[%dA\r\x1b[0J", r.lines)
	r.lines = 0
}

func jobSummaryLines(snap job.Snapshot, live bool) []string {
	lines := []string{fmt.Sprintf("%s  %s", snap.Target, snap.State)}
	if snap.Message != "" {
		lines = append(lines, "  "+snap.Message)
	}
	if len(snap.Changed) > 0 {
		lines = append(lines, "  changed: "+strings.Join(snap.Changed, ", "))
	}
	lines = append(lines, fmt.Sprintf("  job %s in %s", snap.ID, time.Duration(snap.DurationMS)*time.Millisecond))
	return fitLines(lines, live)
}

func jobStepLines(snap job.Snapshot, now time.Time, live bool) []string {
	var lines []string
	for _, s := range snap.Steps {
		lines = append(lines, stepLine(s, now))
		if s.Running {
			lines = append(lines, progressLines(snap.Progress)...)
		}
		if live {
			if s.Error != "" {
				lines = append(lines, "      "+s.Error)
			}
			continue
		}
		lines = append(lines, stepDetail(s)...)
	}
	return fitLines(lines, live)
}

// progressLines name the services a running step is waiting on, so a health
// check that sits there for minutes says which service is holding it up.
func progressLines(states []job.ServiceState) []string {
	if len(states) == 0 {
		return nil
	}
	width := 0
	for _, st := range states {
		if len(st.Service) > width {
			width = len(st.Service)
		}
	}
	shown := states
	var extra int
	if len(shown) > maxProgressLines {
		extra = len(shown) - maxProgressLines
		shown = shown[:maxProgressLines]
	}
	lines := make([]string, 0, len(shown)+1)
	for _, st := range shown {
		health := st.Health
		if health == "" {
			health = "no healthcheck"
		}
		lines = append(lines, fmt.Sprintf("      %-*s  %-10s %s", width, st.Service, orDash(st.State), health))
	}
	if extra > 0 {
		lines = append(lines, fmt.Sprintf("      and %d more", extra))
	}
	return lines
}

func stepLine(s job.Step, now time.Time) string {
	mark := "ok"
	d := time.Duration(s.DurationMS) * time.Millisecond
	switch {
	case s.Running:
		mark, d = "...", now.Sub(s.StartedAt)
	case !s.OK:
		mark = "FAILED"
	}
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("    %-*s %-*s %s", stepNameCol, s.Name, stepMarkCol, mark, briefDuration(d))
}

// progressOnly matches compose's per-image progress lines. They are not
// failures, and printing them buries the one line that is.
var progressOnly = regexp.MustCompile(`^\s*(?:Image\s+)?\S+\s+(?:Pulling|Pulled|Interrupted|Waiting|Downloading|Extracting|Verifying|Skipped)\s*$`)

func meaningfulLines(output string) []string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimRight(output, "\n"), "\n") {
		if strings.TrimSpace(line) == "" || progressOnly.MatchString(line) {
			continue
		}
		out = append(out, strings.TrimRight(line, "\r"))
	}
	return out
}

// stepDetail carries the command output of a failed step. exec reports only
// "exit status 1", so without this the reason a pull was refused is never shown.
func stepDetail(s job.Step) []string {
	var lines []string
	if s.Error != "" {
		lines = append(lines, "      "+s.Error)
	}
	if s.OK || s.Running || strings.TrimSpace(s.Output) == "" {
		return lines
	}
	out := meaningfulLines(s.Output)
	if len(out) == 0 {
		return lines
	}
	if len(out) > maxStepOutputLines {
		lines = append(lines, fmt.Sprintf("      ...%d earlier lines", len(out)-maxStepOutputLines))
		out = out[len(out)-maxStepOutputLines:]
	}
	for _, l := range out {
		lines = append(lines, "      "+strings.TrimRight(l, "\r"))
	}
	return lines
}

// liveWidth keeps a redrawn line inside one screen row. The cursor maths counts
// logical lines, so a line that wraps desynchronises every frame after it.
func liveWidth() int {
	if w := terminalWidth(int(os.Stdout.Fd())); w >= minWidth {
		return w - 1
	}
	return fallbackWidth
}

// fitLines keeps a redrawn block one screen line per logical line. The cursor
// maths counts logical lines, so an embedded newline or a wrap desynchronises it.
func fitLines(lines []string, live bool) []string {
	width := liveWidth()
	if !live {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		l = strings.ReplaceAll(l, "\n", " ")
		out[i] = clip(strings.ReplaceAll(l, "\t", " "), width)
	}
	return out
}

func briefDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return short(d.Round(time.Second))
	}
}

// scanTable prints a row per stack as its check returns. The stack column is
// sized from the names up front, which is what removes the need to buffer.
type scanTable struct {
	w       io.Writer
	tty     bool
	stack   int
	pending bool
}

func newScanTable(w io.Writer, tty bool, targets []string) *scanTable {
	width := len("STACK")
	for _, n := range targets {
		if len(n) > width {
			width = len(n)
		}
	}
	return &scanTable{w: w, tty: tty, stack: width}
}

func (t *scanTable) header() {
	_, _ = fmt.Fprintln(t.w, t.row("STACK", "RESULT", "DETAIL"))
}

func (t *scanTable) checking(name string) {
	if !t.tty {
		return
	}
	_, _ = fmt.Fprintln(t.w, t.row(name, "checking...", ""))
	t.pending = true
}

func (t *scanTable) result(name, result, detail string) {
	if t.pending {
		_, _ = fmt.Fprint(t.w, "\x1b[1A\r\x1b[2K")
		t.pending = false
	}
	_, _ = fmt.Fprintln(t.w, t.row(name, result, detail))
}

func (t *scanTable) row(stack, result, detail string) string {
	return strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s",
		t.stack, stack, scanResultCol, result, detail), " ")
}
