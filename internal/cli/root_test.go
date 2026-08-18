package cli

import (
	"flag"
	"strings"
	"testing"
)

func TestCanonicalAcceptsTheSpellingsPeopleType(t *testing.T) {
	for _, in := range []string{"version", "-version", "--version", "ver", "-ver", "--ver", "v", "-v", "-V", "VERSION"} {
		got, ok := canonical(in)
		if !ok || got != "version" {
			t.Errorf("canonical(%q) = %q, %v; want version, true", in, got, ok)
		}
	}
	for _, in := range []string{"help", "-help", "--help", "h", "-h"} {
		got, ok := canonical(in)
		if !ok || got != "help" {
			t.Errorf("canonical(%q) = %q, %v; want help, true", in, got, ok)
		}
	}
}

// A near miss is a typo, not an invitation to guess. Prefix matching would also
// make "dup c" ambiguous between check and cert.
func TestCanonicalRejectsNearMisses(t *testing.T) {
	for _, in := range []string{"vers", "-vers", "verzion", "--vrsion", "---version", "-", "--", "", "check", "cert", "c"} {
		if got, ok := canonical(in); ok {
			t.Errorf("canonical(%q) = %q, true; want no match", in, got)
		}
	}
}

func TestParseFlagsAcceptsAnyOrder(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantPositional []string
		wantDry        bool
		wantConfig     string
	}{
		{"positional first", []string{"web", "--dry-run"}, []string{"web"}, true, "/default"},
		{"flag first", []string{"--dry-run", "web"}, []string{"web"}, true, "/default"},
		{"flag with value first", []string{"--config", "/x", "web"}, []string{"web"}, false, "/x"},
		{"interleaved", []string{"--config", "/x", "web", "--dry-run"}, []string{"web"}, true, "/x"},
		{"no positional", []string{"--dry-run"}, nil, true, "/default"},
		{"two positionals", []string{"web", "api"}, []string{"web", "api"}, false, "/default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			config := fs.String("config", "/default", "")
			dry := fs.Bool("dry-run", false, "")

			got, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tc.args, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantPositional, ",") {
				t.Errorf("positional = %v, want %v", got, tc.wantPositional)
			}
			if *dry != tc.wantDry {
				t.Errorf("--dry-run = %v, want %v", *dry, tc.wantDry)
			}
			if *config != tc.wantConfig {
				t.Errorf("--config = %q, want %q", *config, tc.wantConfig)
			}
		})
	}
}

// A second stack name is a mistake, and silently ignoring it is how "dup status
// --config X web --limit 5" used to show every stack while looking filtered.
func TestOneTargetRejectsASecondPositional(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	if _, err := oneTarget(fs, []string{"web", "api"}, "dup status [stack]"); err == nil {
		t.Fatal("a second stack name was accepted")
	}
}

func TestNoArgsRejectsAStrayPositional(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	if err := noArgs(fs, []string{"oops"}, "check"); err == nil {
		t.Fatal("a stray argument was accepted")
	}
}

// Asking for help is not an error, so -h must not reach main as one.
func TestHelpIsNotAnError(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	_, err := parseFlags(fs, []string{"-h"})
	if err != errHelpRequested {
		t.Fatalf("parseFlags(-h) = %v, want errHelpRequested", err)
	}
	if err := Run([]string{"update", "-h"}); err != nil {
		t.Errorf("Run(update -h) = %v, want nil", err)
	}
}

func TestUsageListsEveryCommandExactlyOnce(t *testing.T) {
	var b strings.Builder
	usage(&b)
	out := b.String()

	for _, c := range commands() {
		if n := strings.Count(out, "  "+c.name+" "); n != 1 {
			t.Errorf("usage mentions %q %d times, want 1", c.name, n)
		}
	}
	for _, group := range groupOrder {
		if !strings.Contains(out, group) {
			t.Errorf("usage is missing the %q group", group)
		}
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// "dup help help" used to bounce between the alias branch and the help branch
// until the stack overflowed.
func TestHelpHelpTerminates(t *testing.T) {
	for _, args := range [][]string{
		{"help", "help"}, {"help", "-h"}, {"-h", "help"}, {"help", "--help"}, {"h", "h"},
	} {
		if err := Run(args); err != nil {
			t.Errorf("Run(%v) = %v, want nil", args, err)
		}
	}
}

func TestHelpForwardsToTheSubcommand(t *testing.T) {
	if err := Run([]string{"help", "update"}); err != nil {
		t.Errorf("Run(help update) = %v, want nil", err)
	}
	if err := Run([]string{"help", "nosuchcommand"}); err == nil {
		t.Error("help for an unknown command should still report it as unknown")
	}
}
