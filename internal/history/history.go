// Package history keeps a durable record of finished update jobs. The in-memory
// job store is bounded and dies with the process, so without this a restart
// loses every trace of what dup did and when.
package history

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/rotate"
)

const (
	DefaultPath = "/var/lib/dup/history.jsonl"
	// A rotated archive is read back whole, so this caps memory during a read as
	// much as it caps disk.
	DefaultMaxBytes int64 = 8 << 20
	DefaultKeep           = 4
)

type Config struct {
	Path     string
	MaxBytes int64
	Keep     int
}

type Writer struct {
	mu sync.Mutex
	w  *rotate.Writer
}

func Open(cfg Config) (*Writer, error) {
	if cfg.Path == "" {
		cfg.Path = DefaultPath
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultKeep
	}

	w, err := rotate.Open(rotate.Config{
		Path:     cfg.Path,
		MaxBytes: cfg.MaxBytes,
		Keep:     cfg.Keep,
		Mode:     0o640,
	})
	if err != nil {
		return nil, err
	}
	return &Writer{w: w}, nil
}

// Append records one finished job. A job still running is skipped, so the file
// holds outcomes rather than intermediate states.
func (w *Writer) Append(snap job.Snapshot) error {
	if w == nil || !snap.State.Terminal() {
		return nil
	}

	line, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("history: encode job %s: %w", snap.ID, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("history: write job %s: %w", snap.ID, err)
	}
	return nil
}

func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	return w.w.Close()
}

type Query struct {
	Target string
	JobID  string
	Limit  int
}

// Read returns matching jobs newest first, walking the live file and then the
// archives from newest to oldest, stopping as soon as Limit is satisfied.
func Read(path string, q Query) ([]job.Snapshot, error) {
	if path == "" {
		path = DefaultPath
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}

	var out []job.Snapshot
	for _, f := range files(path) {
		found, err := readFile(f, q)
		if err != nil {
			return out, err
		}
		// Within one file, later lines are newer.
		for i := len(found) - 1; i >= 0; i-- {
			out = append(out, found[i])
			if len(out) >= q.Limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// files lists the live file first, then archives in ascending index order, which
// is newest to oldest.
func files(path string) []string {
	list := []string{path}

	matches, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		return list
	}
	sort.Slice(matches, func(i, j int) bool {
		return archiveIndex(matches[i], path) < archiveIndex(matches[j], path)
	})
	return append(list, matches...)
}

func archiveIndex(name, path string) int {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, path+"."), ".gz")
	n := 0
	for _, c := range trimmed {
		if c < '0' || c > '9' {
			return 1 << 30
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func readFile(path string, q Query) ([]job.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			// A half-written archive should not make the whole command fail.
			return nil, nil
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}

	var out []job.Snapshot
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var snap job.Snapshot
		if err := json.Unmarshal(line, &snap); err != nil {
			// A truncated tail is expected after a hard kill. Skip it.
			continue
		}
		if q.Target != "" && snap.Target != q.Target {
			continue
		}
		// Prefix, because the listing shows a shortened id and then tells you to
		// pass it to --job. An exact match made that instruction wrong.
		if q.JobID != "" && !strings.HasPrefix(snap.ID, q.JobID) {
			continue
		}
		out = append(out, snap)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("history: read %s: %w", path, err)
	}
	return out, nil
}
