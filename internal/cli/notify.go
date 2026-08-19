package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/notify"
)

func runNotify(args []string) error {
	fs, configPath := newFlagSet("notify")
	quiet := fs.Bool("quiet", false, "print only the outcome, for a cron job or a health check")

	if err := noArgs(fs, args, "notify"); err != nil {
		return err
	}

	cfg, err := config.LoadBasic(*configPath)
	if err != nil {
		return err
	}
	if cfg.Notify.URL == "" {
		return fmt.Errorf("no outbound webhook is configured.\n\n"+
			"Add one to %s:\n\n  notify:\n    url: https://n8n.example.com/webhook/dup", *configPath)
	}

	n := notify.New(cfg.Notify, hostname(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n == nil {
		return fmt.Errorf("notify.url is set but the notifier could not be built")
	}

	out := os.Stdout
	if !*quiet {
		printNotifyConfig(out, cfg, n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Notify.Timeout+5*time.Second)
	defer cancel()

	res, derr := n.Deliver(ctx, testSnapshot(), true)

	if !*quiet {
		_, _ = fmt.Fprintf(out, "\nsending %d bytes\n", len(res.Sent))
		_, _ = fmt.Fprintf(out, "  %s\n", clip(string(res.Sent), 400))
	}

	if derr != nil {
		if *quiet {
			return fmt.Errorf("the webhook could not be reached: %w", derr)
		}
		_, _ = fmt.Fprintf(out, "\nno response after %s\n", res.Duration.Round(time.Millisecond))
		_, _ = fmt.Fprintf(out, "  %v\n", derr)
		printNotifyAdvice(out, cfg, res, 0)
		return fmt.Errorf("the webhook could not be reached")
	}

	if *quiet {
		if !res.OK() {
			return fmt.Errorf("the webhook rejected the test, status %d: %s", res.Status, clip(res.Body, 200))
		}
		_, _ = fmt.Fprintf(out, "delivered, %d in %s\n", res.Status, res.Duration.Round(time.Millisecond))
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nresponse %d in %s\n", res.Status, res.Duration.Round(time.Millisecond))
	if body := strings.TrimSpace(res.Body); body != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", clip(body, 400))
	} else {
		_, _ = fmt.Fprintf(out, "  (empty body, which is normal for a webhook that accepted it)\n")
	}

	if !res.OK() {
		printNotifyAdvice(out, cfg, res, res.Status)
		return fmt.Errorf("the webhook rejected the test, status %d", res.Status)
	}

	_, _ = fmt.Fprintf(out, "\ndelivered.\n")
	if res.Format == notify.FormatDiscord {
		_, _ = fmt.Fprintf(out, "Discord only accepts a message, so it got the summary sentence and nothing\n")
		_, _ = fmt.Fprintf(out, "machine readable. It says it is a test in the words, not in a field.\n")
	} else {
		_, _ = fmt.Fprintf(out, "The receiving end got a payload marked \"test\": true, so it can tell this\n")
		_, _ = fmt.Fprintf(out, "apart from a real update.\n")
	}
	return nil
}

func printNotifyConfig(w io.Writer, cfg *config.Config, n *notify.Notifier) {
	_, _ = fmt.Fprintf(w, "url      %s\n", n.URL())
	_, _ = fmt.Fprintf(w, "format   %s\n", n.Format())
	_, _ = fmt.Fprintf(w, "timeout  %s\n", cfg.Notify.Timeout)

	if len(cfg.Notify.Headers) == 0 {
		_, _ = fmt.Fprintf(w, "headers  none\n")
		return
	}
	names := make([]string, 0, len(cfg.Notify.Headers))
	for k := range cfg.Notify.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	// Names only. A header is where people put the receiving end's auth token.
	_, _ = fmt.Fprintf(w, "headers  %s\n", strings.Join(names, ", "))
}

func printNotifyAdvice(w io.Writer, cfg *config.Config, res notify.Result, status int) {
	_, _ = fmt.Fprintln(w)
	switch {
	case config.IsDiscordWebhook(cfg.Notify.URL) && res.Format != notify.FormatDiscord:
		_, _ = fmt.Fprintf(w, "That is a discord webhook, and discord refuses any body without content,\n")
		_, _ = fmt.Fprintf(w, "embeds or files. dup posts its own json, so discord answers 400. Set:\n\n")
		_, _ = fmt.Fprintf(w, "  notify:\n    format: discord\n\n")
		_, _ = fmt.Fprintf(w, "and dup posts the summary sentence instead.\n")
	case status == 401 || status == 403:
		_, _ = fmt.Fprintf(w, "The endpoint refused the credentials. Check notify.headers, which is where\n")
		_, _ = fmt.Fprintf(w, "an auth token for the receiving end belongs.\n")
	case status == 404:
		_, _ = fmt.Fprintf(w, "Nothing is listening at that path. A discord or n8n webhook url is\n")
		_, _ = fmt.Fprintf(w, "revoked by deleting it, and then answers 404 rather than telling you.\n")
	case status >= 500:
		_, _ = fmt.Fprintf(w, "The endpoint itself failed. dup does not retry a notification, so an\n")
		_, _ = fmt.Fprintf(w, "update that finishes during an outage there is reported nowhere.\n")
	case status == 0:
		_, _ = fmt.Fprintf(w, "Nothing answered. Check dns, the firewall, and that the agent host can\n")
		_, _ = fmt.Fprintf(w, "reach it at all: curl -sS -X POST %s\n", cfg.Notify.URL)
	default:
		_, _ = fmt.Fprintf(w, "The body above is what the endpoint said. dup does not retry, so a real\n")
		_, _ = fmt.Fprintf(w, "update failing this way is reported nowhere but the journal.\n")
	}
}

// testSnapshot is a finished job that never ran. It carries the shape a real
// one would, so a receiver's branching can be exercised rather than guessed at.
func testSnapshot() job.Snapshot {
	now := time.Now().UTC()
	return job.Snapshot{
		ID:         "0000000000000000",
		Target:     "dup-notify-test",
		State:      job.StateDryRun,
		Message:    "test notification from dup notify, no stack was touched",
		Trigger:    "cli",
		Reason:     "dup notify",
		StartedAt:  now,
		FinishedAt: &now,
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
