package cli

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/version"
)

const DefaultConfigPath = "/etc/dup/config.yml"

type command struct {
	name    string
	summary string
	run     func(args []string) error
}

func commands() []command {
	return []command{
		{"list", "show configured stacks, their update policy, and what dup is not covering", runList},
		{"status", "show recent update jobs for one stack or all of them", runStatus},
		{"update", "trigger an update for one stack", runUpdate},
		{"check", "validate the config file and exit", runCheck},
		{"audit", "verify the service account cannot rewrite what runs as root", runAudit},
		{"cert", "generate the self-signed TLS certificate (root)", runCert},
		{"serve", "run the unprivileged HTTP API (systemd runs this)", runServe},
		{"version", "print the version", runVersion},
	}
}

func Run(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	case "-v", "--version":
		return runVersion(nil)
	}

	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(args[1:])
		}
	}

	fmt.Fprintf(os.Stderr, "dup: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", args[0])
}

func usage(w *os.File) {
	_, _ = fmt.Fprintf(w, "dup %s - webhook driven Docker Compose updates with health checks and rollback\n\n", version.Short())
	_, _ = fmt.Fprintf(w, "usage: dup <command> [flags]\n\n")

	var width int
	for _, c := range commands() {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range commands() {
		_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.summary)
	}
	_, _ = fmt.Fprintf(w, "\nrun 'dup <command> -h' for the flags a command takes\n")
	_, _ = fmt.Fprintf(w, "config defaults to %s\n", DefaultConfigPath)
}

func runVersion(args []string) error {
	fs := flag.NewFlagSet("dup version", flag.ContinueOnError)
	full := fs.Bool("full", false, "print commit, build date and toolchain too")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *full {
		fmt.Print(version.Full("dup"))
		return nil
	}
	fmt.Println(version.Info("dup"))
	return nil
}

func popPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("dup "+name, flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath, "path to the config file")
	return fs, configPath
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func apiURL(cfg *config.Config) string {
	if cfg.TLS.Enabled() {
		return "https://" + cfg.Listen
	}
	return "http://" + cfg.Listen
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return strings.Join(values, ",")
}
