package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/9technologygroup/docker-updater/internal/agentd"
	"github.com/9technologygroup/docker-updater/internal/compose"
	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/pipeline"
	"github.com/9technologygroup/docker-updater/internal/rotate"
	"github.com/9technologygroup/docker-updater/internal/version"
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
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.BoolVar(&showVersion, "ver", false, "")
	flag.BoolVar(&showVersion, "v", false, "")
	flag.Parse()

	if showVersion {
		fmt.Println(version.Info("dup-agent"))
		return nil
	}
	if flag.NArg() > 0 {
		return fmt.Errorf("dup-agent takes no arguments, got %q; the config path is a flag: dup-agent -config %s", flag.Arg(0), *configPath)
	}

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		return err
	}

	uid, err := lookupUID(cfg.AgentPeerUser)
	if err != nil {
		return err
	}

	log, closeLog := newAgentLogger(cfg)
	defer closeLog()
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

// newAgentLogger mirrors the API service: stdout for the journal, plus a
// rotating file when one is configured. The agent writes to its own file so the
// two units never contend for the same handle.
func newAgentLogger(cfg *config.Config) (*slog.Logger, func()) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}

	if cfg.LogFile == "" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), func() {}
	}

	path := agentLogPath(cfg.LogFile)
	w, err := rotate.Open(rotate.Config{
		Path:     path,
		MaxBytes: int64(cfg.LogMaxSizeMB) << 20,
		Keep:     cfg.LogKeep,
		Mode:     0o640,
	})
	if err != nil {
		log := slog.New(slog.NewJSONHandler(os.Stdout, opts))
		log.Warn("logging to file is disabled, the journal still has everything",
			"path", path, "error", err)
		return log, func() {}
	}
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, w), opts))
	log.Info("logging to file", "path", path)
	return log, func() { _ = w.Close() }
}

// agentLogPath keeps the root agent's log in its own directory. Sharing one with
// the API service would mean the unprivileged account could rewrite or delete the
// root process's record of what it ran, and that record is the thing you reach
// for when something has gone wrong.
func agentLogPath(logFile string) string {
	dir, name := filepath.Split(strings.TrimRight(logFile, "/"))
	ext := filepath.Ext(name)
	if ext == "" {
		ext = ".log"
	}
	return filepath.Join(strings.TrimRight(dir, "/")+"-agent", "dup-agent"+ext)
}
