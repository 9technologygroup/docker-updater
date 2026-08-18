package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PatchMon/docker-updater/internal/agent"
	"github.com/PatchMon/docker-updater/internal/audit"
	"github.com/PatchMon/docker-updater/internal/certs"
	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/job"
	"github.com/PatchMon/docker-updater/internal/notify"
	"github.com/PatchMon/docker-updater/internal/schedule"
	"github.com/PatchMon/docker-updater/internal/server"
	"github.com/PatchMon/docker-updater/internal/version"
)

const serveShutdownGrace = 30 * time.Second

func runServe(args []string) error {
	fs, configPath := newFlagSet("serve")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := checkListen(cfg); err != nil {
		return err
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}

	notifier := notify.New(cfg.Notify, host, log)
	store := job.NewStore()

	var n job.Notifier
	if notifier != nil {
		n = notifier
	}

	client := agent.NewClient(cfg.AgentSocket)
	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := client.Ping(pingCtx); err != nil {
		log.Warn("update agent is not reachable yet, updates will fail until it is", "error", err)
	}
	pingCancel()

	manager := job.NewManager(client, store, n, log)
	srv := server.New(cfg, manager, log, host, version.Short())

	scheduler := schedule.New(cfg, client, manager, log)
	go scheduler.Run(ctx)

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}

	scheme := "http"
	if cfg.TLS.Enabled() {
		scheme = "https"
		if !certs.Exists(cfg.TLS.CertFile, cfg.TLS.KeyFile) {
			_ = listener.Close()
			return fmt.Errorf("tls is enabled but %s or %s is missing; run 'dup cert' as root to create a self-signed pair, or point cert_file and key_file at your own", cfg.TLS.CertFile, cfg.TLS.KeyFile)
		}
		httpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	log.Info("dup started",
		"version", version.Short(), "listen", cfg.Listen, "scheme", scheme, "host", host, "uid", os.Getuid(),
		"agent_socket", cfg.AgentSocket, "targets", cfg.TargetNames(),
		"auto_update", scheduler.Managed(), "notify", cfg.Notify.URL != "",
		"allow_from", cfg.AllowFrom, "trusted_proxies", cfg.TrustedProxies)

	serveErr := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLS.Enabled() {
			err = httpServer.ServeTLS(listener, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = httpServer.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down, waiting for in-flight updates")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serveShutdownGrace)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
	store.DrainRunning(shutdownCtx)
	log.Info("stopped")
	return nil
}

func checkListen(cfg *config.Config) error {
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if cfg.TLS.Enabled() || cfg.AllowNonLoopback {
		return nil
	}
	return fmt.Errorf("listen %s is off-loopback with no tls; either enable tls, or keep dup on 127.0.0.1 behind a reverse proxy, or set allow_non_loopback: true to serve plaintext on the network anyway", cfg.Listen)
}

func auditConfig(cfg *config.Config, stdout, stderr io.Writer) error {
	if cfg.AgentPeerUser == "" {
		return fmt.Errorf("agent_peer_user is not set, so there is no service account to audit")
	}

	id, err := audit.LookupIdentity(cfg.AgentPeerUser)
	if err != nil {
		return err
	}

	findings := audit.Run(cfg, id)
	if len(findings) == 0 {
		_, _ = fmt.Fprintf(stdout, "audit ok: %q cannot write any target directory, compose file or env file\n", cfg.AgentPeerUser)
		return nil
	}

	_, _ = fmt.Fprintf(stderr, "audit FAILED: %q can write files that decide what runs as root:\n\n", cfg.AgentPeerUser)
	for _, f := range findings {
		_, _ = fmt.Fprintf(stderr, "  %s\n    %s\n", f.Path, f.Reason)
	}
	_, _ = fmt.Fprintf(stderr, "\nAnyone who compromises the API service could rewrite these and have the agent run it as root.\nMake them owned by root and not writable by %s, then run this again.\n", cfg.AgentPeerUser)
	return fmt.Errorf("%d %s writable by the service account", len(findings), plural(len(findings), "path is", "paths are"))
}
