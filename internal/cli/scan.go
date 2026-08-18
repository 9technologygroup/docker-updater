package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
)

type scanResult struct {
	Target    string   `json:"target"`
	Available bool     `json:"available"`
	Changed   []string `json:"changed"`
	Message   string   `json:"message"`
}

func runScan(args []string) error {
	fs, configPath := newFlagSet("scan")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to allow for the whole scan")

	target, err := oneTarget(fs, args, "dup scan [stack] [flags]")
	if err != nil {
		return err
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}

	targets := cfg.TargetNames()
	if target != "" {
		if _, ok := cfg.Target(target); !ok {
			return unknownStack(target, cfg)
		}
		targets = []string{target}
	}
	if len(targets) == 0 {
		fmt.Println("no stacks configured, so there is nothing to check")
		return nil
	}

	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("Checking %d %s against %s. This pulls images, so it can take a while.\n\n",
		len(targets), plural(len(targets), "stack", "stacks"), plural(len(targets), "its registry", "their registries"))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STACK\tRESULT\tSERVICES\tDETAIL")

	available, failed, busy := 0, 0, 0
	// Clipping an error into a table cell can hide the only copy of it, so the
	// full text is printed underneath instead.
	var problems []string
	// Sequentially: the agent serialises per target and caps concurrency anyway,
	// and a parallel scan would just queue behind that while pulling in bulk.
	for _, name := range targets {
		var out scanResult
		err := client.do(ctx, http.MethodPost, "/v1/targets/"+name+"/check", "", &out)

		result, services, detail := "up to date", "-", ""
		var se *apiStatusError
		refused := errors.As(err, &se)

		switch {
		// A target already being checked or updated is not a fault. The agent
		// serialises per stack, and the scheduler may simply have got there first.
		case refused && (se.status == http.StatusConflict || se.status == http.StatusServiceUnavailable):
			busy++
			result, detail = "busy", "already being checked or updated, try again shortly"
		case err != nil:
			failed++
			result, detail = "check FAILED", "see below"
			problems = append(problems, name+": "+err.Error())
		case out.Available:
			available++
			result = "update available"
			services = orDash(clip(strings.Join(out.Changed, ","), 24))
			detail = clip(out.Message, 44)
		default:
			detail = clip(out.Message, 44)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, result, services, detail)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	for _, p := range problems {
		fmt.Printf("\n%s\n", p)
	}

	fmt.Println()
	if busy > 0 {
		fmt.Printf("%d %s busy, so nothing was learned about %s. The scheduler checks on its own\n",
			busy, plural(busy, "stack was", "stacks were"), plural(busy, "it", "them"))
		fmt.Printf("interval anyway, and dup list shows anything waiting out a soak.\n\n")
	}
	switch {
	case available > 0:
		fmt.Printf("%d %s an update waiting. Apply one with:\n\n  sudo dup update <stack>\n",
			available, plural(available, "stack has", "stacks have"))
	case failed == 0 && busy == 0:
		fmt.Printf("Everything is on its latest image.\n")
	}
	if failed > 0 {
		fmt.Printf("%d %s could not be checked. See:  dup logs\n", failed, plural(failed, "stack", "stacks"))
		return fmt.Errorf("%d of %d %s could not be checked", failed, len(targets), plural(len(targets), "stack", "stacks"))
	}
	fmt.Printf("\nThis did not change anything. Auto-update stacks still wait out their soak.\n")
	return nil
}
