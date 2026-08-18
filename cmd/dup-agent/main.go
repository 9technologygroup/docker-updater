package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/PatchMon/docker-updater/internal/agentd"
	"github.com/PatchMon/docker-updater/internal/compose"
	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/pipeline"
	"github.com/PatchMon/docker-updater/internal/version"
)

const shutdownGrace = 90 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/dup/config.yml", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Info("dup-agent"))
		return nil
	}

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		return err
	}

	uid, err := lookupUID(cfg.AgentPeerUser)
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	docker := compose.New("docker")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	verifyCtx, verifyCancel := context.WithTimeout(ctx, 30*time.Second)
	err = docker.Verify(verifyCtx)
	verifyCancel()
	if err != nil {
		return err
	}

	srv := agentd.NewServer(cfg, pipeline.New(docker), log).WithDocker(docker)
	srv.RequirePeerUID(uid)

	if !agentd.PeerCredSupported() {
		log.Warn("peer credential checking is not implemented on this platform, so socket permissions are the only control on who may drive the agent",
			"platform", runtime.GOOS, "agent_peer_user", cfg.AgentPeerUser)
	}

	listener, err := agentd.Listen(cfg.AgentSocket)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ConnContext:       agentd.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	log.Info("dup-agent started",
		"version", version.Short(), "socket", cfg.AgentSocket, "peer_user", cfg.AgentPeerUser,
		"targets", cfg.TargetNames())

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("agent shutdown", "error", err)
	}
	log.Info("stopped")
	return nil
}

func lookupUID(name string) (uint32, error) {
	if name == "" {
		return 0, fmt.Errorf("agent_peer_user is not set in the config; add 'agent_peer_user: dup' so the agent can verify who is calling it")
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, fmt.Errorf("agent_peer_user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("agent_peer_user %q has a non-numeric uid %q", name, u.Uid)
	}
	return uint32(uid), nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
