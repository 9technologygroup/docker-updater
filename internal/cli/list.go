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

	printHeader(cfg)
	printTargets(cfg)
	printWarnings(cfg)
	printActivity(cfg)

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

func printHeader(cfg *config.Config) {
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
	fmt.Printf("api %s   inbound %s   outbound %s\n\n",
		apiURL(cfg), joinOr(cfg.InboundMethods(), "none"), outbound)
}

func printTargets(cfg *config.Config) {
	if len(cfg.Targets) == 0 {
		fmt.Printf("No stacks configured yet. Everything below is a candidate.\n")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STACK\tAUTO\tEVERY\tSOAK\tROLLBACK\tSERVICES\tDIR")

	for _, t := range cfg.Targets {
		every, soak := "-", "-"
		auto := "no"
		if t.AutoUpdate {
			auto = "yes"
			every = short(t.CheckInterval)
			soak = short(t.SoakWindow())
		}
		rollback := "no"
		if t.RollbackEnabled() {
			rollback = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, auto, every, soak, rollback, joinOr(t.Services, "all"), t.Dir)
	}
	_ = w.Flush()
}

// printActivity shows what the scheduler is holding. An update that has been
// detected and is soaking is otherwise invisible until it applies.
func printActivity(cfg *config.Config) {
	views, err := fetchTargets(cfg)
	if err != nil {
		return
	}

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
		when := "now"
		if remaining := time.Until(*v.PendingApplies); remaining > 0 {
			when = "in " + short(remaining.Round(time.Minute))
		}
		_, _ = fmt.Fprintf(w, "  %s\tupdate waiting out its soak\tapplies %s\t%s\n",
			v.Name, when, joinOr(v.PendingChanged, "unknown services"))
	}
	_ = w.Flush()
}

// fetchTargets is best effort. dup list is useful with the API down, so a
// failure here drops the activity section rather than the whole command.
func fetchTargets(cfg *config.Config) ([]targetView, error) {
	client, err := newAPIClient(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out struct {
		Targets []targetView `json:"targets"`
	}
	if err := client.do(ctx, http.MethodGet, "/v1/targets", "", &out); err != nil {
		return nil, err
	}
	return out.Targets, nil
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
