package cli

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/PatchMon/docker-updater/internal/certs"
	"github.com/PatchMon/docker-updater/internal/config"
)

func runCert(args []string) error {
	fs, configPath := newFlagSet("cert")
	force := fs.Bool("force", false, "replace an existing certificate")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadService(*configPath)
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
		return fmt.Errorf("tls.self_signed is not set, so dup will not overwrite %s; it expects the certificate you configured to be one you manage", cfg.TLS.CertFile)
	}

	if certs.Exists(cfg.TLS.CertFile, cfg.TLS.KeyFile) && !*force {
		existing, err := certs.Describe(cfg.TLS.CertFile)
		if err != nil {
			return err
		}
		fmt.Printf("certificate already exists, leaving it alone\n")
		printCert(existing)
		fmt.Printf("\nrun 'dup cert --force' to replace it\n")
		return nil
	}

	hosts := cfg.TLS.Hosts
	if len(hosts) == 0 {
		hosts = certs.DefaultHosts(cfg.Listen)
	}

	gid := -1
	if cfg.AgentPeerUser != "" {
		if g, err := lookupGID(cfg.AgentPeerUser); err == nil {
			gid = g
		}
	}

	result, err := certs.Generate(cfg.TLS.CertFile, cfg.TLS.KeyFile, hosts, gid)
	if err != nil {
		return err
	}

	fmt.Printf("generated a self-signed certificate\n")
	printCert(result)
	if os.Geteuid() != 0 {
		fmt.Printf("\nNot running as root, so ownership was left as-is.\n")
		fmt.Printf("In production run this as root so the key ends up root:%s 0640.\n", cfg.AgentPeerUser)
	}
	fmt.Printf("\nTLS is enabled in %s, so dup will serve %s once restarted:\n\n", *configPath, apiURL(cfg))
	fmt.Printf("  sudo systemctl restart dup\n")
	fmt.Printf("  dup check\n")
	fmt.Printf("\nThis is self-signed, so clients will not trust it until you tell them to.\n")
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
