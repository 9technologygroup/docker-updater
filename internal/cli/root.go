package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/version"
)

const DefaultConfigPath = "/etc/dup/config.yml"

type command struct {
	name    string
	group   string
	summary string
	run     func(args []string) error
}

const (
	groupSetup = "Set up, in this order"
	groupDaily = "Day to day"
	groupUnit  = "Run by systemd"
)

var groupOrder = []string{groupSetup, groupDaily, groupUnit}

func commands() []command {
	return []command{
		{"check", groupSetup, "validate the config file and exit", runCheck},
		{"audit", groupSetup, "verify the service account cannot rewrite what runs as root", runAudit},
		{"cert", groupSetup, "generate the self-signed TLS certificate (root)", runCert},

		{"list", groupDaily, "show configured stacks, their update policy, and what dup is not covering", runList},
		{"status", groupDaily, "show what is happening now, and the next scheduled check", runStatus},
		{"logs", groupDaily, "show the durable history of finished updates, newest first", runLogs},
		{"update", groupDaily, "trigger an update for one stack", runUpdate},
		{"version", groupDaily, "print the version and check for a newer release", runVersion},

		{"serve", groupUnit, "run the unprivileged HTTP API", runServe},
	}
}

// aliases maps the spellings people actually type onto a command. Only exact
// members are accepted, so a typo still prints usage rather than being guessed at.
var aliases = map[string]string{
	"v": "version", "ver": "version", "version": "version",
	"h": "help", "help": "help",
}

func canonical(arg string) (string, bool) {
	trimmed := arg
	for range 2 {
		if !strings.HasPrefix(trimmed, "-") {
			break
		}
		trimmed = trimmed[1:]
	}
	if trimmed == "" || strings.HasPrefix(trimmed, "-") {
		return "", false
	}
	name, ok := aliases[strings.ToLower(trimmed)]
	return name, ok
}

func Run(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	name := args[0]
	if c, ok := canonical(name); ok {
		name = c
	}
	if name == "help" {
		// Forward only to something that is not itself help, or "dup help help"
		// bounces between these two branches until the stack runs out.
		if len(args) > 1 {
			if sub, ok := canonical(args[1]); !ok || sub != "help" {
				return Run([]string{args[1], "-h"})
			}
		}
		usage(os.Stdout)
		return nil
	}

	for _, c := range commands() {
		if c.name == name {
			if err := c.run(args[1:]); err != nil && !errors.Is(err, errHelpRequested) {
				return err
			}
			return nil
		}
	}

	fmt.Fprintf(os.Stderr, "dup: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", args[0])
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "dup %s - webhook driven Docker Compose updates with health checks and rollback\n\n", version.Short())
	_, _ = fmt.Fprintf(w, "usage: dup <command> [flags]\n")

	all := commands()
	var width int
	for _, c := range all {
		if len(c.name) > width {
			width = len(c.name)
		}
	}

	for _, group := range groupOrder {
		_, _ = fmt.Fprintf(w, "\n%s\n", group)
		for _, c := range all {
			if c.group == group {
				_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.summary)
			}
		}
	}

	_, _ = fmt.Fprintf(w, "\nrun 'dup <command> -h' for the flags a command takes\n")
	_, _ = fmt.Fprintf(w, "config defaults to %s\n", DefaultConfigPath)
}

func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("dup "+name, flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath, "path to the config file")
	return fs, configPath
}

// errHelpRequested unwinds a -h to a clean exit. Asking for help is not an error,
// and returning flag.ErrHelp makes main print "error: flag: help requested".
var errHelpRequested = errors.New("help requested")

// parseFlags accepts flags and positional arguments in any order. Go's flag
// package stops at the first non-flag argument, so re-parsing what follows is
// what lets "dup update --dry-run web" and "dup update web --dry-run" agree.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelpRequested
			}
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// oneTarget parses flags and returns the single optional stack name.
func oneTarget(fs *flag.FlagSet, args []string, usageLine string) (string, error) {
	positional, err := parseFlags(fs, args)
	if err != nil {
		return "", err
	}
	switch len(positional) {
	case 0:
		return "", nil
	case 1:
		return positional[0], nil
	default:
		return "", fmt.Errorf("usage: %s", usageLine)
	}
}

func noArgs(fs *flag.FlagSet, args []string, name string) error {
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("dup %s takes no arguments, got %q", name, positional[0])
	}
	return nil
}

func apiURL(cfg *config.Config) string {
	if cfg.TLS.IsEnabled() {
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
