package cli

import (
	"fmt"
	"os"

	"github.com/9technologygroup/docker-updater/internal/certs"
	"github.com/9technologygroup/docker-updater/internal/config"
)

func runCheck(args []string) error {
	fs, configPath := newFlagSet("check")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.AgentPeerUser == "" {
		return fmt.Errorf("agent_peer_user is not set; add 'agent_peer_user: dup' so the agent can verify who is calling it")
	}

	auto := 0
	for _, t := range cfg.Targets {
		if t.AutoUpdate {
			auto++
		}
	}
	if cfg.TLS.Enabled() {
		if !certs.Exists(cfg.TLS.CertFile, cfg.TLS.KeyFile) {
			return fmt.Errorf("tls is enabled but %s or %s is missing; run 'dup cert' as root to create them", cfg.TLS.CertFile, cfg.TLS.KeyFile)
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
	return nil
}

func runAudit(args []string) error {
	fs, configPath := newFlagSet("audit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		return err
	}
	return auditConfig(cfg, os.Stdout, os.Stderr)
}
