package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/9technologygroup/docker-updater/internal/agent"
	"github.com/9technologygroup/docker-updater/internal/certs"
	"github.com/9technologygroup/docker-updater/internal/config"
)

func runCheck(args []string) error {
	fs, configPath := newFlagSet("check")
	if err := noArgs(fs, args, "check"); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.AgentPeerUser == "" {
		return fmt.Errorf("agent_peer_user is not set; add 'agent_peer_user: dup' so the agent can verify who is calling it")
	}
	// The same guard dup serve applies, so this command cannot pass a config the
	// service will then refuse to start with.
	if err := checkListen(cfg); err != nil {
		return err
	}
	if cfg.TLS.IsEnabled() && !certs.Exists(cfg.TLS.CertFile, cfg.TLS.KeyFile) {
		return missingCertError(cfg, *configPath)
	}

	auto := 0
	for _, t := range cfg.Targets {
		if t.AutoUpdate {
			auto++
		}
	}

	if len(cfg.Targets) == 0 {
		fmt.Printf("config ok: no stacks configured yet\n")
	} else {
		fmt.Printf("config ok: %d %s (%d on auto update)\n",
			len(cfg.Targets), plural(len(cfg.Targets), "stack", "stacks"), auto)
	}
	fmt.Printf("  api          %s\n", apiURL(cfg))
	fmt.Printf("  agent socket %s\n", cfg.AgentSocket)
	fmt.Printf("  allow from   %s\n", joinOr(cfg.AllowFrom, "anywhere that can reach the port"))
	if len(cfg.TrustedProxies) > 0 {
		fmt.Printf("  proxies      %s (X-Forwarded-For honoured from these)\n", joinOr(cfg.TrustedProxies, "none"))
	}
	fmt.Printf("  cors         %s\n", joinOr(cfg.CORS.AllowedOrigins, "disabled, no browser may call this API"))

	hooks := 0
	for _, t := range cfg.Targets {
		if t.PreUpdate.Configured() {
			hooks++
		}
	}
	if hooks > 0 {
		fmt.Printf("  pre-update   %d %s configured\n", hooks, plural(hooks, "hook", "hooks"))
	}
	printDockerConfigStatus(cfg)

	printWarnings(cfg)
	printAgentDrift(cfg)
	if len(cfg.Targets) == 0 {
		fmt.Printf("\ndup is not managing anything yet. See what is on this host:\n\n")
		fmt.Printf("  sudo dup list\n\n")
		fmt.Printf("Then add stacks under 'targets:' in the config, and re-run this\n")
		fmt.Printf("and 'sudo dup audit' before restarting.\n")
	}
	return nil
}

// printWarnings reports problems that do not stop dup running. This command
// gates ExecStartPre for both units, so a stack directory that is not mounted
// yet must not take the whole service down.
func printWarnings(cfg *config.Config) {
	warnings := cfg.Warnings()
	if len(warnings) == 0 {
		return
	}
	fmt.Printf("\n%d %s:\n", len(warnings), plural(len(warnings), "warning", "warnings"))
	for _, w := range warnings {
		fmt.Printf("  %s\n", w)
	}
	fmt.Printf("\ndup will start, and updates for those stacks will fail until the path is there.\n")
}

// printDockerConfigStatus is informational only: it never fails the check, since
// a stack with no credentials of its own may simply be pulling public images or
// relying on root's own docker login.
func printDockerConfigStatus(cfg *config.Config) {
	withStore, without := 0, 0
	for _, t := range cfg.Targets {
		if t.HasDockerConfig() {
			withStore++
		} else {
			without++
		}
	}
	if withStore > 0 {
		fmt.Printf("  registry auth %d %s own credentials\n", withStore, plural(withStore, "stack has", "stacks have"))
	}
	if without > 0 {
		fmt.Printf("  registry auth %d %s root's docker config\n", without, plural(without, "stack falls back to", "stacks fall back to"))
	}
}

// printAgentDrift is the whole point of running check after an edit: the file can
// be perfectly valid while the root agent is still running the version it loaded
// at startup, and until now the only symptom was a bare "unknown target" later.
func printAgentDrift(cfg *config.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	health, err := agent.NewClient(cfg.AgentSocket).Health(ctx)
	if err != nil || health.ConfigFingerprint == "" {
		// Not running, or too old to report. Neither is this command's business.
		return
	}
	if health.ConfigFingerprint == cfg.Fingerprint() {
		return
	}

	fmt.Printf("\nThis config does not match the one the update agent is running.\n")
	fmt.Printf("  on disk        %s\n", cfg.Fingerprint())
	fmt.Printf("  agent loaded   %s", health.ConfigFingerprint)
	if !health.ConfigLoadedAt.IsZero() {
		fmt.Printf(" at %s", health.ConfigLoadedAt.Local().Format("02 Jan 15:04:05"))
	}
	fmt.Printf("\n  agent stacks   %s\n", joinOr(health.Targets, "none"))
	fmt.Printf("\nUpdates will fail for anything the agent has not loaded. Apply it:\n\n")
	fmt.Printf("  sudo systemctl restart dup-agent dup\n")
}

func missingCertError(cfg *config.Config, configPath string) error {
	if cfg.TLS.SelfSigned {
		return fmt.Errorf("tls.self_signed is set but %s and %s do not exist yet.\n\n"+
			"Generate them:\n\n  sudo dup cert",
			cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
	return fmt.Errorf("tls is enabled in %s but the certificate you configured is not there yet.\n\n"+
		"Expected:\n\n  %s\n  %s\n\n"+
		"Put your own certificate and key at those paths, or set tls.self_signed: true\n"+
		"and run 'sudo dup cert' to have dup generate a pair instead",
		configPath, cfg.TLS.CertFile, cfg.TLS.KeyFile)
}

func runAudit(args []string) error {
	fs, configPath := newFlagSet("audit")
	if err := noArgs(fs, args, "audit"); err != nil {
		return err
	}

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		return err
	}
	return auditConfig(cfg, os.Stdout, os.Stderr)
}
