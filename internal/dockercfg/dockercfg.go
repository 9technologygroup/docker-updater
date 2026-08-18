// Package dockercfg reads and writes the credential half of docker's
// config.json, so the unprivileged half of dup and the root agent agree on the
// exact file docker itself will read from DOCKER_CONFIG.
package dockercfg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	FileName = "config.json"
	DirMode  = 0o700
	FileMode = 0o600

	DefaultBase = "/etc/dup/docker"

	// HubHost is the key docker itself writes for Docker Hub. Storing a plain
	// "docker.io" entry instead would never be found at pull time.
	HubHost = "https://index.docker.io/v1/"
)

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

var ErrNoHost = errors.New("no registry host given")

type Config struct {
	fields map[string]json.RawMessage
	auths  map[string]map[string]json.RawMessage
}

func New() *Config {
	return &Config{
		fields: map[string]json.RawMessage{},
		auths:  map[string]map[string]json.RawMessage{},
	}
}

func Path(dir string) string { return filepath.Join(dir, FileName) }

func Exists(dir string) bool {
	info, err := os.Stat(Path(dir))
	return err == nil && info.Mode().IsRegular()
}

// Read treats an absent file as an empty store so a first `dup auth` does not
// have to special-case it. Use Exists to tell the two apart.
func Read(dir string) (*Config, error) {
	raw, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", Path(dir), err)
	}

	c := New()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c.fields); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(dir), err)
	}
	if auths, ok := c.fields["auths"]; ok {
		if err := json.Unmarshal(auths, &c.auths); err != nil {
			return nil, fmt.Errorf("parse %s: auths: %w", Path(dir), err)
		}
		delete(c.fields, "auths")
	}
	if c.auths == nil {
		c.auths = map[string]map[string]json.RawMessage{}
	}
	return c, nil
}

func (c *Config) Hosts() []string {
	hosts := make([]string, 0, len(c.auths))
	for h := range c.auths {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

func (c *Config) Has(host string) bool {
	_, ok := c.auths[host]
	return ok
}

func (c *Config) Empty() bool { return len(c.auths) == 0 }

// Username reports who an entry belongs to, for listing. It never returns the
// password half.
func (c *Config) Username(host string) (string, bool) {
	entry, ok := c.auths[host]
	if !ok {
		return "", false
	}
	var encoded string
	if err := json.Unmarshal(entry["auth"], &encoded); err != nil {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	user, _, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", false
	}
	return user, true
}

func (c *Config) Set(host, username, password string) error {
	if host == "" {
		return ErrNoHost
	}
	if username == "" || password == "" {
		return errors.New("both a username and a password are needed")
	}
	if strings.ContainsRune(username, ':') {
		return errors.New("a registry username cannot contain a colon")
	}

	encoded, err := json.Marshal(base64.StdEncoding.EncodeToString([]byte(username + ":" + password)))
	if err != nil {
		return err
	}

	entry := c.auths[host]
	if entry == nil {
		entry = map[string]json.RawMessage{}
	}
	entry["auth"] = encoded
	// A leftover token or plaintext pair from an earlier write would win over
	// the entry we just stored.
	delete(entry, "identitytoken")
	delete(entry, "registrytoken")
	delete(entry, "username")
	delete(entry, "password")
	c.auths[host] = entry
	return nil
}

func (c *Config) Remove(host string) bool {
	if _, ok := c.auths[host]; !ok {
		return false
	}
	delete(c.auths, host)
	return true
}

func (c *Config) marshal() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(c.fields)+1)
	for k, v := range c.fields {
		out[k] = v
	}
	auths, err := json.Marshal(c.auths)
	if err != nil {
		return nil, err
	}
	out["auths"] = auths

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Write replaces the file in one rename. The directory and its parent are
// tightened on every write: the dup service account must never be able to read
// a stack's credentials, and a directory created by hand may not be 0700.
func (c *Config) Write(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("credential directory %q must be an absolute path", dir)
	}
	if err := EnsureBase(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}

	body, err := c.marshal()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config.json.*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if err := tmp.Chmod(FileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, Path(dir)); err != nil {
		return fmt.Errorf("install %s: %w", Path(dir), err)
	}
	return nil
}

// EnsureBase creates the credential store and tightens it. It refuses a shallow
// path rather than chmod 0700 a system directory somebody pointed it at.
func EnsureBase(base string) error {
	if err := validBase(base); err != nil {
		return err
	}
	if err := os.MkdirAll(base, DirMode); err != nil {
		return fmt.Errorf("create %s: %w", base, err)
	}
	if err := os.Chmod(base, DirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", base, err)
	}
	return nil
}

func validBase(base string) error {
	if !filepath.IsAbs(base) {
		return fmt.Errorf("credential directory %q must be an absolute path", clip(base))
	}
	clean := filepath.Clean(base)
	if strings.Count(clean, string(filepath.Separator)) < 2 {
		return fmt.Errorf("credential directory %q is a top level directory; point docker_config_dir at something like %s", clip(clean), DefaultBase)
	}
	return nil
}

func Delete(dir string) error {
	if err := os.Remove(Path(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", Path(dir), err)
	}
	return nil
}

// StoreDir joins defensively. Stack names are already regex validated upstream,
// but this is the only thing standing between a config edit and a write outside
// the credential store.
func StoreDir(base, stack string) (string, error) {
	if err := validBase(base); err != nil {
		return "", err
	}
	if stack == "" || stack == "." || stack == ".." || stack != filepath.Base(stack) || strings.ContainsRune(stack, filepath.Separator) {
		return "", fmt.Errorf("stack name %q cannot be used as a directory name", clip(stack))
	}
	clean := filepath.Clean(base)
	dir := filepath.Join(clean, stack)
	if filepath.Dir(dir) != clean {
		return "", fmt.Errorf("stack name %q escapes %s", clip(stack), clean)
	}
	return dir, nil
}

// NormaliseHost maps what someone types onto the key docker uses.
func NormaliseHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		return "", ErrNoHost
	}
	switch strings.ToLower(host) {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "index.docker.io/v1":
		return HubHost, nil
	}
	if !hostRe.MatchString(host) {
		return "", fmt.Errorf("%q is not a registry host such as harbor.example.com or registry.example.com:5000", clip(host))
	}
	return host, nil
}

// ProbeHost is where the v2 API lives for a stored key, which is not the key
// itself for Docker Hub.
func ProbeHost(host string) string {
	if host == HubHost {
		return "registry-1.docker.io"
	}
	return host
}

func clip(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
