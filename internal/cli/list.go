package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/9technologygroup/docker-updater/internal/agent"
	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/version"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

type targetView struct {
	Name           string     `json:"name"`
	Busy           bool       `json:"busy"`
	RunningJob     string     `json:"running_job,omitempty"`
	PendingSince   *time.Time `json:"pending_since,omitempty"`
	PendingApplies *time.Time `json:"pending_applies_at,omitempty"`
	PendingChanged []string   `json:"pending_changed,omitempty"`
	AutoUpdate     bool       `json:"auto_update"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	NextCheckAt    *time.Time `json:"next_check_at,omitempty"`
}

type targetsResponse struct {
	Targets []targetView `json:"targets"`
	Now     time.Time    `json:"now"`
}

func runList(args []string) error {
	fs, configPath := newFlagSet("list")
	all := fs.Bool("all", false, "also list compose projects that dup already covers")
	if err := noArgs(fs, args, "list"); err != nil {
		return err
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}

	resp, _ := fetchTargets(cfg)
	views := resp.Targets

	printHeader(cfg, resp.Now)
	printTargets(cfg, views)
	printWarnings(cfg)
	printActivity(views)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := agent.NewClient(cfg.AgentSocket).Discover(ctx)
	if err != nil {
		fmt.Printf("\nCoverage unknown.\n\n%s\n", agentUnreachable(cfg.AgentSocket, err))
		return nil
	}
	printCoverage(result, *all)
	return nil
}

func printHeader(cfg *config.Config, now time.Time) {
	auto := len(cfg.AutoUpdateTargets())
	if len(cfg.Targets) == 0 {
		fmt.Printf("dup %s  no stacks configured\n", version.Short())
	} else {
		fmt.Printf("dup %s  %d %s configured, %d on auto update\n",
			version.Short(), len(cfg.Targets), plural(len(cfg.Targets), "stack", "stacks"), auto)
	}

	outbound := cfg.Notify.URL
	if outbound == "" {
		outbound = "none"
	}
	fmt.Printf("api %s   inbound %s   outbound %s\n",
		apiURL(cfg), joinOr(cfg.InboundMethods(), "none"), outbound)
	if now.IsZero() {
		fmt.Printf("server time unknown, the API is not reachable\n\n")
	} else {
		fmt.Printf("server time %s\n\n", now.Local().Format("Mon 02 Jan 2006 15:04:05 MST"))
	}
}

func printTargets(cfg *config.Config, views []targetView) {
	if len(cfg.Targets) == 0 {
		fmt.Printf("No stacks configured yet. Everything below is a candidate.\n")
		return
	}
	byName := make(map[string]targetView, len(views))
	for _, v := range views {
		byName[v.Name] = v
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// No AUTO column: a dash under CHECK already says this stack only updates
	// when asked, and the dashes were most of the table's width.
	_, _ = fmt.Fprintln(w, "STACK\tCHECK\tNEXT\tSOAK\tRB\tSERVICES\tDIR")

	manual := 0
	for _, t := range cfg.Targets {
		every, soak, next := "-", "-", "-"
		if t.AutoUpdate {
			every = short(t.CheckInterval)
			soak = short(t.SoakWindow())
			next = nextCheckIn(byName[t.Name].NextCheckAt)
		} else {
			manual++
		}
		rollback := "off"
		if t.RollbackEnabled() {
			rollback = "on"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, every, next, soak, rollback, joinOr(t.Services, "all"), clipPath(t.Dir, 34))
	}
	_ = w.Flush()

	if manual > 0 {
		fmt.Printf("\nA dash under CHECK means that stack updates only when asked.\n")
	}
}

// clipPath keeps the tail of a path, which is the part that identifies the stack.
// Full paths pushed the table past any sensible terminal width.
func clipPath(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max+3:]
}

// clip keeps the head of free text, where the useful part usually is.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// printActivity shows what the scheduler is holding. An update that has been
// detected and is soaking is otherwise invisible until it applies.
func printActivity(views []targetView) {
	var pending, running []targetView
	for _, v := range views {
		switch {
		case v.Busy:
			running = append(running, v)
		case v.PendingApplies != nil:
			pending = append(pending, v)
		}
	}
	if len(pending) == 0 && len(running) == 0 {
		return
	}

	fmt.Printf("\nIn flight\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, v := range running {
		_, _ = fmt.Fprintf(w, "  %s\tupdating now\tjob %s\n", v.Name, v.RunningJob)
	}
	for _, v := range pending {
		_, _ = fmt.Fprintf(w, "  %s\tupdate waiting out its soak\tapplies %s\tnew image for %s\n",
			v.Name, at(v.PendingApplies), joinOr(v.PendingChanged, "unknown services"))
	}
	_ = w.Flush()
}

// fetchTargets is best effort. dup list is useful with the API down, so a
// failure here drops the activity section rather than the whole command.
func fetchTargets(cfg *config.Config) (targetsResponse, error) {
	var out targetsResponse

	client, err := newAPIClient(cfg)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.do(ctx, http.MethodGet, "/v1/targets", "", &out); err != nil {
		return out, err
	}
	return out, nil
}

func printCoverage(result wire.DiscoverResult, all bool) {
	if result.Warning != "" {
		fmt.Printf("\n%s\n", result.Warning)
	}

	uncovered := result.Uncovered()
	if len(uncovered) == 0 && len(result.Loose) == 0 {
		fmt.Printf("\nEverything on this host is covered by dup.\n")
	} else {
		fmt.Printf("\nNot covered by dup\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, p := range uncovered {
			_, _ = fmt.Fprintf(w, "  compose project\t%s\t%s\t%s\n", p.Name, orDash(p.Dir), p.Status)
		}
		for _, c := range result.Loose {
			_, _ = fmt.Fprintf(w, "  loose container\t%s\t%s\t%s\n", c.Name, orDash(c.Image), c.State)
		}
		_ = w.Flush()
	}

	if !all {
		return
	}

	fmt.Printf("\nCovered compose projects\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, p := range result.Projects {
		if p.Target == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s\tstack %s\t%s\n", p.Name, p.Target, p.Status)
	}
	_ = w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func short(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
