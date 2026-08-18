package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/9technologygroup/docker-updater/internal/selfupdate"
	"github.com/9technologygroup/docker-updater/internal/version"
)

const (
	implicitCheckTimeout = 2 * time.Second
	explicitCheckTimeout = 10 * time.Second
)

func runVersion(args []string) error {
	return runVersionTo(args, os.Stdout, os.Stderr, selfupdate.New())
}

// runVersionTo keeps stdout to exactly one line. install.sh captures
// `dup version` into a shell variable, so an advisory on stdout would corrupt it.
// Everything about a newer release goes to stderr, as git and brew do.
func runVersionTo(args []string, stdout, stderr io.Writer, checker *selfupdate.Checker) error {
	fs := flag.NewFlagSet("dup version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	full := fs.Bool("full", false, "print commit, build date and toolchain too")
	check := fs.Bool("check", false, "check for a newer release now, ignoring the cache")
	noCheck := fs.Bool("no-check", false, "do not check for a newer release")
	if err := noArgs(fs, args, "version"); err != nil {
		return err
	}

	if *full {
		_, _ = fmt.Fprint(stdout, version.Full("dup"))
	} else {
		_, _ = fmt.Fprintln(stdout, version.Info("dup"))
	}

	explicit := *check || *full
	if *noCheck {
		return nil
	}

	timeout := implicitCheckTimeout
	if explicit {
		timeout = explicitCheckTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	status, err := checker.Check(ctx, version.Short(), *check)
	if *full {
		printLatestLine(stdout, status, err)
		return nil
	}
	if err != nil {
		if explicit {
			_, _ = fmt.Fprintln(stderr, checkFailure(err))
		}
		return nil
	}
	if status.Newer {
		printAdvisory(stderr, status, checker.Repo)
	}
	return nil
}

func printAdvisory(w io.Writer, s selfupdate.Status, repo string) {
	_, _ = fmt.Fprintf(w, "a newer dup is available: %s (you have %s)\n", s.Latest.Tag, s.Current)
	_, _ = fmt.Fprintf(w, "  upgrade:  curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sudo sh\n", repo)
	if notes := s.Latest.NotesURL(repo); notes != "" {
		_, _ = fmt.Fprintf(w, "  notes:    %s\n", notes)
	}
}

func printLatestLine(w io.Writer, s selfupdate.Status, err error) {
	switch {
	case errors.Is(err, selfupdate.ErrDisabled):
		_, _ = fmt.Fprintf(w, "  latest:  not checked (%s is set)\n", selfupdate.DisableEnv)
	case errors.Is(err, selfupdate.ErrDevBuild):
		_, _ = fmt.Fprintf(w, "  latest:  not checked (development build)\n")
	case err != nil:
		_, _ = fmt.Fprintf(w, "  latest:  could not be checked\n")
	case s.Newer:
		_, _ = fmt.Fprintf(w, "  latest:  %s, released %s\n", s.Latest.Tag, s.Latest.PublishedAt.Format("2006-01-02"))
	default:
		_, _ = fmt.Fprintf(w, "  latest:  %s, released %s (up to date)\n", s.Latest.Tag, s.Latest.PublishedAt.Format("2006-01-02"))
	}
}

func checkFailure(err error) string {
	switch {
	case errors.Is(err, selfupdate.ErrDisabled):
		return "update checks are disabled by " + selfupdate.DisableEnv
	case errors.Is(err, selfupdate.ErrDevBuild):
		return "this is a development build, so there is nothing to compare against a release"
	case errors.Is(err, selfupdate.ErrNoRelease):
		return "could not check for a newer release: no published release found"
	default:
		return "could not check for a newer release: " + err.Error()
	}
}
