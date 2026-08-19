package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/9technologygroup/docker-updater/internal/agent"
	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/dockercfg"
	"github.com/9technologygroup/docker-updater/internal/registry"
)

func runAuth(args []string) error {
	fs, configPath := newFlagSet("auth")
	list := fs.Bool("list", false, "list stored registry credentials, without prompting for anything")
	remove := fs.String("remove", "", "remove the stored credentials for this registry host")
	force := fs.Bool("force", false, "overwrite a host that already has stored credentials")

	target, err := oneTarget(fs, args, "dup auth [stack] [flags]")
	if err != nil {
		return err
	}
	if *list && *remove != "" {
		return fmt.Errorf("--list and --remove cannot be combined")
	}

	// Refuse before prompting or writing, not after. The store ends up owned
	// by whichever account runs this, and only root may read it back.
	if os.Geteuid() != 0 {
		// Echo the arguments back: a bare "sudo dup auth" means every stack,
		// which is not what someone who named one asked for.
		return fmt.Errorf("run this as root.\n\n"+
			"Registry credentials are stored root:root 0600 so the unprivileged API\n"+
			"service can never read them. Re-run:\n\n  sudo dup auth%s", authArgsSuffix(target, *list, *remove, *force))
	}

	cfg, err := config.LoadBasic(*configPath)
	if err != nil {
		return err
	}

	switch {
	case *remove != "":
		return runAuthRemove(cfg, target, *remove)
	case *list:
		return runAuthList(cfg, target)
	default:
		return runAuthAdd(cfg, target, *force)
	}
}

func runAuthList(cfg *config.Config, target string) error {
	targets := cfg.Targets
	if target != "" {
		t, ok := cfg.Target(target)
		if !ok {
			return unknownStack(target, cfg)
		}
		targets = []*config.Target{t}
	}

	type row struct{ stack, host, username, added string }
	var rows []row
	for _, t := range targets {
		if !t.HasDockerConfig() {
			continue
		}
		store, err := dockercfg.Read(t.DockerConfigDir())
		if err != nil {
			return err
		}
		added := "-"
		if info, err := os.Stat(dockercfg.Path(t.DockerConfigDir())); err == nil {
			added = info.ModTime().Local().Format("02 Jan 15:04")
		}
		for _, host := range store.Hosts() {
			user, _ := store.Username(host)
			rows = append(rows, row{t.Name, host, user, added})
		}
	}

	if len(rows) == 0 {
		if target != "" {
			fmt.Printf("no registry credentials stored for %s\n", target)
		} else {
			fmt.Println("no registry credentials stored for any stack")
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STACK\tHOST\tUSERNAME\tADDED")
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.stack, r.host, r.username, r.added)
	}
	_ = w.Flush()
	return nil
}

func runAuthRemove(cfg *config.Config, target, host string) error {
	if target == "" {
		return fmt.Errorf("usage: dup auth <stack> --remove <host>")
	}
	t, ok := cfg.Target(target)
	if !ok {
		return unknownStack(target, cfg)
	}
	normalised, err := dockercfg.NormaliseHost(host)
	if err != nil {
		return err
	}
	if !t.HasDockerConfig() {
		fmt.Printf("%s has no stored registry credentials\n", target)
		return nil
	}

	store, err := dockercfg.Read(t.DockerConfigDir())
	if err != nil {
		return err
	}
	if !store.Remove(normalised) {
		fmt.Printf("%s has no stored credentials for %s\n", target, normalised)
		return nil
	}
	if store.Empty() {
		if err := dockercfg.Delete(t.DockerConfigDir()); err != nil {
			return err
		}
		fmt.Printf("removed %s from %s; no registries left, removed the credential store\n", normalised, target)
		return nil
	}
	if err := store.Write(t.DockerConfigDir()); err != nil {
		return err
	}
	fmt.Printf("removed %s from %s\n", normalised, target)
	return nil
}

func runAuthAdd(cfg *config.Config, target string, force bool) error {
	if !stdinIsTTY() {
		return fmt.Errorf("dup auth needs an interactive terminal to prompt for credentials; stdin is not a TTY\n\n" +
			"Run it from a real shell, not a script or a pipe")
	}

	targets := cfg.Targets
	if target != "" {
		t, ok := cfg.Target(target)
		if !ok {
			return unknownStack(target, cfg)
		}
		targets = []*config.Target{t}
	}
	if len(targets) == 0 {
		fmt.Println("no stacks configured, so there is nothing to add credentials for")
		return nil
	}

	stdin := bufio.NewReader(os.Stdin)
	var total authTally
	for i, t := range targets {
		if i > 0 {
			fmt.Println()
		}
		tally, err := authTarget(cfg, t, force, stdin)
		if err != nil {
			return err
		}
		total.add(tally)
	}

	fmt.Println()
	fmt.Println(total.summary())
	if total.failed > 0 {
		return fmt.Errorf("%d %s not stored", total.failed,
			plural(total.failed, "registry was", "registries were"))
	}
	return nil
}

// authTally separates a registry deliberately left alone from one that would
// not take the credentials. Skipping a public registry is not a failure.
type authTally struct {
	saved, skipped, failed int
}

func (a *authTally) add(b authTally) {
	a.saved += b.saved
	a.skipped += b.skipped
	a.failed += b.failed
}

func (a authTally) summary() string {
	if a.saved == 0 && a.failed == 0 && a.skipped == 0 {
		return "nothing to do"
	}
	parts := []string{fmt.Sprintf("stored %d", a.saved)}
	if a.skipped > 0 {
		parts = append(parts, fmt.Sprintf("left %d alone", a.skipped))
	}
	if a.failed > 0 {
		parts = append(parts, fmt.Sprintf("could not store %d", a.failed))
	}
	return strings.Join(parts, ", ")
}

type authOutcome int

const (
	authSaved authOutcome = iota
	authSkipped
	authFailed
)

func authTarget(cfg *config.Config, t *config.Target, force bool, stdin *bufio.Reader) (tally authTally, err error) {
	hosts, err := resolveHosts(cfg, t, stdin)
	if err != nil {
		return tally, err
	}
	if len(hosts) == 0 {
		fmt.Printf("%s: no registries found, nothing to store credentials for\n", t.Name)
		return tally, nil
	}

	fmt.Println(t.Name)
	for _, host := range hosts {
		normalised, nerr := dockercfg.NormaliseHost(host)
		if nerr != nil {
			fmt.Printf("  %v\n", nerr)
			tally.failed++
			continue
		}

		// Re-read right before each write rather than once for the whole target,
		// so a second "dup auth" running against the same stack loses at most one
		// host's update instead of everything this one has saved so far.
		store, rerr := dockercfg.Read(t.DockerConfigDir())
		if rerr != nil {
			return tally, rerr
		}
		if store.Has(normalised) && !force {
			fmt.Printf("  %s already has stored credentials, leaving it alone (--force to replace)\n", normalised)
			tally.skipped++
			continue
		}

		outcome, herr := promptAndVerify(stdin, store, normalised)
		if herr != nil {
			return tally, herr
		}
		switch outcome {
		case authSkipped:
			tally.skipped++
			continue
		case authFailed:
			tally.failed++
			continue
		}
		if werr := store.Write(t.DockerConfigDir()); werr != nil {
			return tally, werr
		}
		tally.saved++
		fmt.Printf("    saved\n")
	}
	return tally, nil
}

// resolveHosts asks the agent which registries a stack's images use, so the
// caller never has to type a hostname it could get wrong. A stack argument
// only reaches here after config validation, so this is the one place that
// still has to fall back by hand.
func resolveHosts(cfg *config.Config, t *config.Target, stdin *bufio.Reader) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := agent.NewClient(cfg.AgentSocket).Images(ctx, t.Name)
	if err == nil {
		return result.Registries, nil
	}

	fmt.Printf("%s: %s\n\n", t.Name, agentUnreachable(cfg.AgentSocket, err))
	host, perr := promptLine(stdin, fmt.Sprintf("%s: registry host (blank to skip): ", t.Name))
	if perr != nil {
		return nil, perr
	}
	if host == "" {
		return nil, nil
	}
	return []string{host}, nil
}

func promptAndVerify(stdin *bufio.Reader, store *dockercfg.Config, host string) (authOutcome, error) {
	fmt.Printf("  %s\n", host)

	username, err := promptLine(stdin, "    username (blank if this registry needs no login): ")
	if err != nil {
		return authFailed, err
	}
	if username == "" {
		fmt.Println("    left alone, dup will pull from it anonymously")
		return authSkipped, nil
	}

	password, err := promptPassword(stdin, "    password: ")
	if err != nil {
		return authFailed, err
	}
	if password == "" {
		fmt.Println("    left alone, no password given")
		return authSkipped, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if verr := registry.Verify(ctx, dockercfg.ProbeHost(host), username, password); verr != nil {
		switch {
		case errors.Is(verr, registry.ErrRejected):
			fmt.Printf("    rejected: wrong username or password, nothing saved\n")
		case errors.Is(verr, registry.ErrUnreachable):
			fmt.Printf("    could not reach %s, nothing saved: %v\n", host, verr)
		default:
			fmt.Printf("    could not verify, nothing saved: %v\n", verr)
		}
		return authFailed, nil
	}

	if serr := store.Set(host, username, password); serr != nil {
		fmt.Printf("    %v, nothing saved\n", serr)
		return authFailed, nil
	}
	return authSaved, nil
}

func promptLine(stdin *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := stdin.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptPassword turns terminal echo off for the read and always restores it,
// including when the process is interrupted mid-read, so a Ctrl-C never
// leaves the shell silently swallowing keystrokes afterwards.
func promptPassword(stdin *bufio.Reader, prompt string) (string, error) {
	if !stdinIsTTY() {
		return "", fmt.Errorf("stdin is not a terminal, refusing to read a password")
	}
	fmt.Print(prompt)

	fd := int(os.Stdin.Fd())
	original, err := getTermState(fd)
	if err != nil {
		return "", fmt.Errorf("could not read the terminal mode: %w", err)
	}
	if err := setTermState(fd, echoOff(original)); err != nil {
		return "", fmt.Errorf("could not turn off terminal echo: %w", err)
	}
	restore := func() { _ = setTermState(fd, original) }

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Println()
			os.Exit(130)
		case <-done:
		}
	}()

	line, err := stdin.ReadString('\n')
	close(done)
	signal.Stop(sig)
	restore()
	fmt.Println()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func authArgsSuffix(target string, list bool, remove string, force bool) string {
	var b strings.Builder
	if target != "" {
		b.WriteString(" " + target)
	}
	if list {
		b.WriteString(" --list")
	}
	if remove != "" {
		b.WriteString(" --remove " + remove)
	}
	if force {
		b.WriteString(" --force")
	}
	return b.String()
}
