package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const maxCacheFile = 4 << 10

// cacheFile holds a tag and timestamps and nothing else. It is written by the
// unprivileged service account and read by root, so it must never carry a URL,
// a path or a command; the tag is re-validated through the semver parser on read.
type cacheFile struct {
	Release    Release   `json:"release"`
	CheckedAt  time.Time `json:"checked_at"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
}

func (c *Checker) readCache() (cacheFile, bool) {
	path := c.CachePath
	if path == "" {
		return cacheFile{}, false
	}

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCacheFile {
		return cacheFile{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cacheFile{}, false
	}

	var cf cacheFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return cacheFile{}, false
	}
	if cf.Release.Tag != "" && !Valid(cf.Release.Tag) {
		return cacheFile{}, false
	}
	return cf, true
}

// writeCache is best effort. An ordinary user running "dup version" cannot write
// the state directory, and that must not stop the check reporting its result.
func (c *Checker) writeCache(cf cacheFile) {
	path := c.CachePath
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	// Written by the service account and read by root, or the other way round,
	// so both need access. The contents are a version tag and two timestamps by
	// design; tightening these modes breaks the other reader.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // see above
		return
	}

	raw, err := json.Marshal(cf)
	if err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { //nolint:gosec // see writeCache
		return
	}
	_ = os.Rename(tmpName, path)
}
