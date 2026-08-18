// Package rotate provides a size-rotating file writer that gzips what it retires.
// Both the service log and the job history use it, so there is one rotation
// implementation to reason about rather than two.
package rotate

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultMaxBytes int64 = 10 << 20
	DefaultKeep           = 5
)

type Config struct {
	Path     string
	MaxBytes int64
	Keep     int
	// Mode applies to the file and, with the execute bits added, to its directory.
	Mode os.FileMode
}

type Writer struct {
	cfg Config
	mu  sync.Mutex
	f   *os.File
	n   int64
}

func Open(cfg Config) (*Writer, error) {
	if cfg.Path == "" {
		return nil, errors.New("rotate: path is required")
	}
	if !filepath.IsAbs(cfg.Path) {
		return nil, fmt.Errorf("rotate: %s must be an absolute path", cfg.Path)
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Keep < 0 {
		cfg.Keep = 0
	}
	if cfg.Mode == 0 {
		cfg.Mode = 0o640
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Path), cfg.Mode|0o111); err != nil {
		return nil, fmt.Errorf("rotate: create %s: %w", filepath.Dir(cfg.Path), err)
	}

	w := &Writer{cfg: cfg}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, w.cfg.Mode)
	if err != nil {
		return fmt.Errorf("rotate: open %s: %w", w.cfg.Path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("rotate: stat %s: %w", w.cfg.Path, err)
	}
	w.f, w.n = f, info.Size()
	return nil
}

// Write rotates first when this write would take the file past MaxBytes, so a
// single record is never split across two files.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		return 0, errors.New("rotate: writer is closed")
	}
	if w.n > 0 && w.n+int64(len(p)) > w.cfg.MaxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("rotate: close: %w", err)
	}
	w.f = nil

	if w.cfg.Keep == 0 {
		if err := os.Remove(w.cfg.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate: discard: %w", err)
		}
		w.n = 0
		return w.open()
	}

	// Oldest first, so nothing is overwritten while shifting.
	if err := os.Remove(w.archive(w.cfg.Keep)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate: prune: %w", err)
	}
	for i := w.cfg.Keep - 1; i >= 1; i-- {
		from, to := w.archive(i), w.archive(i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate: shift %s: %w", from, err)
		}
	}

	if err := gzipFile(w.cfg.Path, w.archive(1), w.cfg.Mode); err != nil {
		return err
	}
	if err := os.Remove(w.cfg.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate: remove rotated: %w", err)
	}

	w.n = 0
	return w.open()
}

func (w *Writer) archive(i int) string {
	return fmt.Sprintf("%s.%d.gz", w.cfg.Path, i)
}

func gzipFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rotate: open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	// A partial archive would be indistinguishable from a whole one, so build it
	// beside the target and rename only once it is complete.
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("rotate: create %s: %w", tmp, err)
	}
	defer func() { _ = os.Remove(tmp) }()

	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		_ = zw.Close()
		_ = out.Close()
		return fmt.Errorf("rotate: compress %s: %w", src, err)
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		return fmt.Errorf("rotate: finish %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("rotate: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rotate: publish %s: %w", dst, err)
	}
	return nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
