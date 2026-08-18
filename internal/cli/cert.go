package cli

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/9technologygroup/docker-updater/internal/certs"
	"github.com/9technologygroup/docker-updater/internal/config"
)

// DefaultServiceUser owns the group that may read the private key. The installer
// and both packages create this account before anything calls dup cert.
const DefaultServiceUser = "dup"

func runCert(args []string) error {
	fs, configPath := newFlagSet("cert")
	force := fs.Bool("force", false, "replace an existing certificate")
	defaults := fs.Bool("defaults", false, "generate at the default paths without reading a config, for first install")
	if err := noArgs(fs, args, "cert"); err != nil {
		return err
	}

	// Refuse before generating, not after. Writing the key first and then saying
	// it landed with the wrong owner leaves a key the service cannot read, and
	// the failure only surfaces later as a TLS error naming neither this command
	// nor the ownership.
	if os.Geteuid() != 0 {
		return fmt.Errorf("run this as root.\n\n" +
			"The private key must end up owned root:dup mode 0640, and only root can\n" +
			"set that. Re-run:\n\n  sudo dup cert")
	}

	if *defaults {
		return generateDefault(*force)
	}

	cfg, err := config.LoadBasic(*configPath)
	if err != nil {
		return err
	}

	if !cfg.TLS.Enabled() {
		return fmt.Errorf("TLS is not enabled in %s, so there is nothing to generate a certificate for.\n\n"+
			"dup does not edit your config for you. Add this to %s and re-run:\n\n"+
			"  tls:\n"+
			"    self_signed: true\n\n"+
			"Then:  sudo dup cert && sudo systemctl restart dup\n\n"+
			"If you already have a certificate, point at it instead:\n\n"+
			"  tls:\n"+
			"    cert_file: /etc/ssl/certs/yours.crt\n"+
			"    key_file:  /etc/ssl/private/yours.key",
			*configPath, *configPath)
	}
	if !cfg.TLS.SelfSigned {
		return fmt.Errorf("tls.self_signed is not set, so dup will not overwrite %s.\n\n"+
			"It expects the certificate you configured to be one you manage. Put your\n"+
			"certificate and key at:\n\n  %s\n  %s\n\n"+
			"Or set tls.self_signed: true in %s to have dup generate a pair instead",
			cfg.TLS.CertFile, cfg.TLS.CertFile, cfg.TLS.KeyFile, *configPath)
	}
	if cfg.AgentPeerUser == "" {
		return fmt.Errorf("agent_peer_user is not set in %s, so dup cannot tell which account\n"+
			"should be able to read the key. Add 'agent_peer_user: dup' and re-run", *configPath)
	}
	gid, err := lookupGID(cfg.AgentPeerUser)
	if err != nil {
		return fmt.Errorf("agent_peer_user %q does not resolve to an account on this host: %w\n\n"+
			"The key would be unreadable by the service. Create the account first, or\n"+
			"correct agent_peer_user in %s", cfg.AgentPeerUser, err, *configPath)
	}

	hosts := cfg.TLS.Hosts
	if len(hosts) == 0 {
		hosts = certs.DefaultHosts(cfg.Listen)
	}
	return writeCert(cfg.TLS.CertFile, cfg.TLS.KeyFile, hosts, gid, cfg.AgentPeerUser, apiURL(cfg), *force)
}

// generateDefault runs before a config exists. The installer calls it so a fresh
// host has a usable certificate the moment the reference config is copied into
// place, rather than leaving TLS as a step somebody has to remember.
func generateDefault(force bool) error {
	gid, err := lookupGID(DefaultServiceUser)
	if err != nil {
		return fmt.Errorf("the %q account does not exist yet: %w\n\n"+
			"It is created by the installer and by the deb and rpm packages, so run\n"+
			"one of those first", DefaultServiceUser, err)
	}
	hosts := certs.DefaultHosts(config.DefaultListen)
	return writeCert(config.DefaultCertFile, config.DefaultKeyFile, hosts, gid,
		DefaultServiceUser, "https://"+config.DefaultListen, force)
}

func writeCert(certFile, keyFile string, hosts []string, gid int, owner, url string, force bool) error {
	if certs.Exists(certFile, keyFile) && !force {
		existing, err := certs.Describe(certFile)
		if err != nil {
			return err
		}
		fmt.Printf("certificate already exists, leaving it alone\n")
		printCert(existing)
		fmt.Printf("\nrun 'dup cert --force' to replace it\n")
		return nil
	}

	result, err := certs.Generate(certFile, keyFile, hosts, gid)
	if err != nil {
		return err
	}

	fmt.Printf("generated a self-signed certificate\n")
	printCert(result)
	fmt.Printf("\nkey owned root:%s mode 0640\n", owner)
	fmt.Printf("\ndup serves %s once TLS is enabled and it is restarted:\n\n", url)
	fmt.Printf("  sudo dup check\n")
	fmt.Printf("  sudo systemctl restart dup-agent dup\n")
	fmt.Printf("\nThis is self-signed, so clients will not trust it until you tell them to.\n")
	fmt.Printf("dup's own commands trust it automatically by reading cert_file.\n")
	fmt.Printf("In n8n, either import %s as a trusted certificate or disable verification for this host only.\n", result.CertFile)
	fmt.Printf("Pin the fingerprint above rather than turning verification off everywhere.\n")
	return nil
}

func printCert(c certs.Result) {
	fmt.Printf("  file         %s\n", c.CertFile)
	if c.KeyFile != "" {
		fmt.Printf("  key          %s\n", c.KeyFile)
	}
	fmt.Printf("  valid until  %s\n", c.NotAfter.Local().Format(time.RFC1123))
	fmt.Printf("  hosts        %s\n", joinOr(c.Hosts, "none"))
	fmt.Printf("  sha256       %s\n", c.Fingerprint)
}

func lookupGID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Gid)
}
