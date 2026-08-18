package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/history"
)

func runLogs(args []string) error {
	fs, configPath := newFlagSet("logs")
	limit := fs.Int("limit", 20, "how many jobs to show")
	jobID := fs.String("job", "", "show one job in full, by id")
	full := fs.Bool("full", false, "show every step of each job, not just the summary")

	target, err := oneTarget(fs, args, "dup logs [stack] [flags]")
	if err != nil {
		return err
	}
	if *limit < 1 {
		return fmt.Errorf("--limit must be at least 1, got %d", *limit)
	}

	cfg, err := config.LoadBasic(*configPath)
	if err != nil {
		return err
	}
	if cfg.HistoryFile == "" {
		return fmt.Errorf("history_file is disabled in %s, so there is nothing recorded.\n\n"+
			"Remove 'history_file: none' to record job history again", *configPath)
	}
	if target != "" {
		if _, ok := cfg.Target(target); !ok {
			return unknownStack(target, cfg)
		}
	}

	if *jobID != "" {
		*limit = 1
	}
	jobs, err := history.Read(cfg.HistoryFile, history.Query{
		Target: target, JobID: *jobID, Limit: *limit,
	})
	if err != nil {
		return err
	}

	if len(jobs) == 0 {
		return emptyHistory(cfg, target, *jobID)
	}
	if *jobID != "" || *full {
		for i, j := range jobs {
			if i > 0 {
				fmt.Println()
			}
			printJob(j)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "WHEN\tSTACK\tSTATE\tTRIGGER\tTOOK\tCHANGED\tJOB\tDETAIL")
	for _, j := range jobs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.StartedAt.Local().Format("02 Jan 15:04"), j.Target, j.State,
			orDash(j.Trigger),
			(time.Duration(j.DurationMS) * time.Millisecond).Round(time.Second),
			orDash(strings.Join(j.Changed, ",")), j.ID, j.Message)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\nFrom %s. Show one in full with:  dup logs --job <JOB>\n", cfg.HistoryFile)
	return nil
}

func emptyHistory(cfg *config.Config, target, jobID string) error {
	switch {
	case jobID != "":
		return fmt.Errorf("no job %q in %s", jobID, cfg.HistoryFile)
	case target != "":
		fmt.Printf("no updates recorded yet for %s\n", target)
	default:
		fmt.Println("no updates recorded yet")
	}
	fmt.Printf("\nHistory is written to %s as each update finishes.\n", cfg.HistoryFile)
	fmt.Printf("Trigger one with:  sudo dup update <stack> --dry-run\n")
	return nil
}

// at gives the absolute time first, because a relative one alone cannot be acted
// on without knowing the server's clock, and then the countdown for convenience.
func at(t *time.Time) string {
	if t == nil {
		return "-"
	}
	local := t.Local()
	stamp := local.Format("15:04:05")
	if !sameDay(local, time.Now()) {
		stamp = local.Format("02 Jan 15:04:05")
	}
	d := time.Until(local)
	switch {
	case d <= 0:
		return stamp + " (due now)"
	case d < time.Minute:
		return fmt.Sprintf("%s (in %ds)", stamp, int(d.Seconds()))
	default:
		return fmt.Sprintf("%s (in %s)", stamp, short(d.Round(time.Minute)))
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// nextCheckIn renders the countdown to the next registry check, which is the
// question "when will it look again" that nothing else can answer.
func nextCheckIn(next *time.Time) string {
	if next == nil {
		return "-"
	}
	d := time.Until(*next)
	if d <= 0 {
		return "due"
	}
	if d < time.Minute {
		return "<1m"
	}
	return short(d.Round(time.Minute))
}
