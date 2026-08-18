package cli

import (
	"io"
	"log/slog"
	"os"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/rotate"
)

// newServiceLogger writes to stdout, which systemd captures into the journal,
// and additionally to a rotating file when one is configured. The journal stays
// in the picture either way, so a file that cannot be opened is a warning rather
// than a reason not to start.
func newServiceLogger(cfg *config.Config) (*slog.Logger, func()) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}

	if cfg.LogFile == "" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), func() {}
	}

	w, err := rotate.Open(rotate.Config{
		Path:     cfg.LogFile,
		MaxBytes: int64(cfg.LogMaxSizeMB) << 20,
		Keep:     cfg.LogKeep,
		Mode:     0o640,
	})
	if err != nil {
		log := slog.New(slog.NewJSONHandler(os.Stdout, opts))
		log.Warn("logging to file is disabled, the journal still has everything",
			"path", cfg.LogFile, "error", err)
		return log, func() {}
	}

	log := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, w), opts))
	log.Info("logging to file", "path", cfg.LogFile,
		"max_size_mb", cfg.LogMaxSizeMB, "keep", cfg.LogKeep)
	return log, func() { _ = w.Close() }
}
