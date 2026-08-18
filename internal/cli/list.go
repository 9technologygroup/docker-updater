package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PatchMon/docker-updater/internal/agent"
	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/version"
	"github.com/PatchMon/docker-updater/internal/wire"
)

func runList(args []string) error {
	fs, configPath := newFlagSet("list")
	all := fs.Bool("all", false, "also list compose projects that dup already covers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}

	printHeader(cfg)
	printTargets(cfg)
	printMissingDirs(cfg, *configPath)

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
	fmt.Printf("dup %s  %d %s configured, %d on auto update\n",
		version.Short(), len(cfg.Targets), plural(len(cfg.Targets), "stack", "stacks"), auto)

	outbound := cfg.Notify.URL
	if outbound == "" {
		outbound = "none"
	}
	fmt.Printf("api %s   inbound %s   outbound %s\n\n",
		apiURL(cfg), joinOr(cfg.InboundMethods(), "none"), outbound)
}

func printTargets(cfg *config.Config) {
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

func printMissingDirs(cfg *config.Config, configPath string) {
	var missing []*config.Target
	for _, t := range cfg.Targets {
		if info, err := os.Stat(t.Dir); err != nil || !info.IsDir() {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return
	}

	fmt.Printf("\n%d %s point at a directory that does not exist:\n",
		len(missing), plural(len(missing), "stack", "stacks"))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, t := range missing {
		_, _ = fmt.Fprintf(w, "  %s\t%s\n", t.Name, t.Dir)
	}
	_ = w.Flush()
	fmt.Printf("\nEdit %s so the stacks match this host, then run: sudo dup check\n", configPath)
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
