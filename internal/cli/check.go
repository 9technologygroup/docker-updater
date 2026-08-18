package cli

import (
	"fmt"
	"os"

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

	fmt.Printf("config ok: %d %s (%d on auto update)\n",
		len(cfg.Targets), plural(len(cfg.Targets), "stack", "stacks"), auto)
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

	printWarnings(cfg)
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
