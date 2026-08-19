package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/9technologygroup/docker-updater/internal/dockercfg"
)

const (
	DefaultListen      = "127.0.0.1:7788"
	DefaultAgentSocket = "/run/dup/agent.sock"
	DefaultCertFile    = "/etc/dup/self-signed.crt"
	DefaultKeyFile     = "/etc/dup/self-signed.key"
	DefaultLogFile     = "/var/log/dup/dup.log"
	DefaultHistoryFile = "/var/lib/dup/history.jsonl"
	DefaultDockerCfg   = dockercfg.DefaultBase
	MinSecretLen       = 32
	MaxBodyBytes       = 1 << 20
	MaxSocketPathLen   = 100
	MinCheckInterval   = time.Minute
)

var (
	targetNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	envNameRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	imageTagRe    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

type Config struct {
	Listen           string    `yaml:"listen"`
	AllowNonLoopback bool      `yaml:"allow_non_loopback"`
	TLS              TLS       `yaml:"tls"`
	CORS             CORS      `yaml:"cors"`
	AllowFrom        []string  `yaml:"allow_from"`
	TrustedProxies   []string  `yaml:"trusted_proxies"`
	AgentSocket      string    `yaml:"agent_socket"`
	AgentPeerUser    string    `yaml:"agent_peer_user"`
	LogLevel         string    `yaml:"log_level"`
	LogFile          string    `yaml:"log_file"`
	LogMaxSizeMB     int       `yaml:"log_max_size_mb"`
	LogKeep          int       `yaml:"log_keep"`
	HistoryFile      string    `yaml:"history_file"`
	DockerConfigDir  string    `yaml:"docker_config_dir"`
	HistoryMaxSizeMB int       `yaml:"history_max_size_mb"`
	HistoryKeep      int       `yaml:"history_keep"`
	Auth             Auth      `yaml:"auth"`
	Defaults         Defaults  `yaml:"defaults"`
	Notify           Notify    `yaml:"notify"`
	Targets          []*Target `yaml:"targets"`

	fingerprint    string
	byName         map[string]*Target
	allowFrom      []netip.Prefix
	trustedProxies []netip.Prefix
	warnings       []string
}

// Fingerprint identifies the exact config bytes this was loaded from. The API
// service and the root agent each load the file once at startup, so comparing
// fingerprints is how either of them can tell the other is running something
// older after an edit.
func (c *Config) Fingerprint() string { return c.fingerprint }

// Warnings are problems that do not stop dup running. A stack directory that is
// not mounted yet is the motivating case: failing here would gate ExecStartPre
// and keep both services down over one absent volume.
func (c *Config) Warnings() []string { return c.warnings }

type TLS struct {
	Enabled    *bool    `yaml:"enabled"`
	CertFile   string   `yaml:"cert_file"`
	KeyFile    string   `yaml:"key_file"`
	SelfSigned bool     `yaml:"self_signed"`
	Hosts      []string `yaml:"hosts"`
}

// IsEnabled reports whether dup should serve TLS. An explicit enabled: wins, so
// the paths can stay visible in the config without cert_file alone forcing TLS
// on. Without it the setting is inferred, which is what configs written before
// enabled: existed rely on.
func (t TLS) IsEnabled() bool {
	if t.Enabled != nil {
		return *t.Enabled
	}
	return t.CertFile != "" || t.KeyFile != "" || t.SelfSigned
}

type CORS struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type Hook struct {
	Command  string        `yaml:"command"`
	Args     []string      `yaml:"args"`
	Timeout  time.Duration `yaml:"timeout"`
	Required *bool         `yaml:"required"`
}

func (h *Hook) Configured() bool { return h != nil && h.Command != "" }

func (h *Hook) IsRequired() bool { return h == nil || h.Required == nil || *h.Required }

type Auth struct {
	BearerToken      string `yaml:"bearer_token"`
	BearerTokenFile  string `yaml:"bearer_token_file"`
	GitHubSecret     string `yaml:"github_secret"`
	GitHubSecretFile string `yaml:"github_secret_file"`

	bearer   []byte
	ghSecret []byte
}

type Defaults struct {
	CheckInterval   time.Duration  `yaml:"check_interval"`
	Soak            *time.Duration `yaml:"soak"`
	PullTimeout     time.Duration  `yaml:"pull_timeout"`
	HealthTimeout   time.Duration  `yaml:"health_timeout"`
	JobTimeout      time.Duration  `yaml:"job_timeout"`
	StabilityWindow time.Duration  `yaml:"stability_window"`
	Rollback        *bool          `yaml:"rollback"`
}

type Notify struct {
	URL     string            `yaml:"url"`
	Format  string            `yaml:"format"`
	Timeout time.Duration     `yaml:"timeout"`
	Headers map[string]string `yaml:"headers"`
}

type Target struct {
	Name            string         `yaml:"name"`
	Dir             string         `yaml:"dir"`
	ComposeFile     string         `yaml:"compose_file"`
	EnvFile         string         `yaml:"env_file"`
	Services        []string       `yaml:"services"`
	ImageTagEnv     string         `yaml:"image_tag_env"`
	AllowPrerelease bool           `yaml:"allow_prerelease"`
	AutoUpdate      bool           `yaml:"auto_update"`
	CheckInterval   time.Duration  `yaml:"check_interval"`
	Soak            *time.Duration `yaml:"soak"`
	Rollback        *bool          `yaml:"rollback"`
	PullTimeout     time.Duration  `yaml:"pull_timeout"`
	HealthTimeout   time.Duration  `yaml:"health_timeout"`
	JobTimeout      time.Duration  `yaml:"job_timeout"`
	StabilityWindow time.Duration  `yaml:"stability_window"`
	PreUpdate       *Hook          `yaml:"pre_update"`

	soak      time.Duration
	dockerCfg string
}

func (t *Target) SoakWindow() time.Duration { return t.soak }

// DockerConfigDir is where this stack's own registry credentials live. The
// agent points DOCKER_CONFIG at it only when HasDockerConfig, so a host that
// has never run `dup auth` keeps using root's own docker config.
func (t *Target) DockerConfigDir() string { return t.dockerCfg }

func (t *Target) HasDockerConfig() bool {
	return t.dockerCfg != "" && dockercfg.Exists(t.dockerCfg)
}

func (t *Target) RollbackEnabled() bool { return t.Rollback == nil || *t.Rollback }

func (c *Config) AutoUpdateTargets() []*Target {
	var out []*Target
	for _, t := range c.Targets {
		if t.AutoUpdate {
			out = append(out, t)
		}
	}
	return out
}

func (c *Config) InboundMethods() []string {
	var methods []string
	if len(c.Auth.bearer) > 0 {
		methods = append(methods, "token")
	}
	if len(c.Auth.ghSecret) > 0 {
		methods = append(methods, "github")
	}
	return methods
}

type options struct {
	checkPaths   bool
	checkSecrets bool
}

func Load(path string) (*Config, error) {
	return load(path, options{checkPaths: true, checkSecrets: true})
}

func LoadService(path string) (*Config, error) {
	return load(path, options{checkSecrets: true})
}

func LoadAgent(path string) (*Config, error) {
	return load(path, options{checkPaths: true})
}

// LoadBasic parses and validates the config without touching the auth secrets.
// Certificate generation has nothing to do with bearer tokens, and reading the
// secret files needs privileges the command may not have yet.
func LoadBasic(path string) (*Config, error) {
	return load(path, options{})
}

func load(path string, opt options) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, readConfigError(path, err)
	}

	var c Config
	sum := sha256.Sum256(raw)
	c.fingerprint = hex.EncodeToString(sum[:12])

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if opt.checkSecrets {
		if err := c.resolveSecrets(); err != nil {
			return nil, err
		}
	}
	if err := c.validateNotify(); err != nil {
		return nil, err
	}
	if err := c.validateNetworks(); err != nil {
		return nil, err
	}
	if err := c.validateTargets(opt.checkPaths); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validateNetworks() error {
	var err error
	if c.allowFrom, err = parsePrefixes("allow_from", c.AllowFrom); err != nil {
		return err
	}
	if c.trustedProxies, err = parsePrefixes("trusted_proxies", c.TrustedProxies); err != nil {
		return err
	}
	for _, origin := range c.CORS.AllowedOrigins {
		if origin == "*" {
			return fmt.Errorf("cors: \"*\" is not allowed; list the exact origins that may call this API")
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("cors: %q must be a full origin such as https://dash.example.com", origin)
		}
	}
	return nil
}

func parsePrefixes(field string, values []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.Contains(v, "/") {
			p, err := netip.ParsePrefix(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a valid CIDR: %w", field, v, err)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a valid IP address or CIDR: %w", field, v, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

func (c *Config) AllowFromPrefixes() []netip.Prefix    { return c.allowFrom }
func (c *Config) TrustedProxyPrefixes() []netip.Prefix { return c.trustedProxies }

func (c *Config) applyDefaults() error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFile == "" {
		c.LogFile = DefaultLogFile
	}
	if c.HistoryFile == "" {
		c.HistoryFile = DefaultHistoryFile
	}
	// "none" turns a file off without leaving the key looking unset.
	if c.LogFile == "none" {
		c.LogFile = ""
	}
	if c.HistoryFile == "none" {
		c.HistoryFile = ""
	}
	if c.DockerConfigDir == "" {
		c.DockerConfigDir = DefaultDockerCfg
	}
	if !filepath.IsAbs(c.DockerConfigDir) {
		return fmt.Errorf("docker_config_dir must be an absolute path")
	}
	c.DockerConfigDir = filepath.Clean(c.DockerConfigDir)
	for _, f := range []struct {
		name, path string
	}{{"log_file", c.LogFile}, {"history_file", c.HistoryFile}} {
		if f.path != "" && !filepath.IsAbs(f.path) {
			return fmt.Errorf("%s must be an absolute path, or \"none\" to disable", f.name)
		}
	}
	if c.LogMaxSizeMB <= 0 {
		c.LogMaxSizeMB = 10
	}
	if c.LogKeep < 0 {
		c.LogKeep = 0
	}
	if c.LogKeep == 0 {
		c.LogKeep = 5
	}
	if c.HistoryMaxSizeMB <= 0 {
		c.HistoryMaxSizeMB = 8
	}
	if c.HistoryKeep <= 0 {
		c.HistoryKeep = 4
	}
	if c.Defaults.PullTimeout <= 0 {
		c.Defaults.PullTimeout = 10 * time.Minute
	}
	if c.Defaults.HealthTimeout <= 0 {
		c.Defaults.HealthTimeout = 3 * time.Minute
	}
	if c.Defaults.JobTimeout <= 0 {
		c.Defaults.JobTimeout = 25 * time.Minute
	}
	if c.Defaults.StabilityWindow <= 0 {
		c.Defaults.StabilityWindow = 15 * time.Second
	}
	if c.Defaults.CheckInterval <= 0 {
		c.Defaults.CheckInterval = 6 * time.Hour
	}
	if c.Defaults.Soak == nil {
		fallback := 30 * time.Minute
		c.Defaults.Soak = &fallback
	}
	if *c.Defaults.Soak < 0 {
		return fmt.Errorf("defaults: soak cannot be negative")
	}
	if c.Notify.Timeout <= 0 {
		c.Notify.Timeout = 15 * time.Second
	}
	if c.AgentSocket == "" {
		c.AgentSocket = DefaultAgentSocket
	}
	if !filepath.IsAbs(c.AgentSocket) {
		return fmt.Errorf("agent_socket must be an absolute path")
	}
	if c.TLS.SelfSigned {
		if c.TLS.CertFile == "" {
			c.TLS.CertFile = DefaultCertFile
		}
		if c.TLS.KeyFile == "" {
			c.TLS.KeyFile = DefaultKeyFile
		}
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls: cert_file and key_file must be set together")
	}
	if c.TLS.IsEnabled() && c.TLS.CertFile == "" {
		return fmt.Errorf("tls.enabled is true but there is nothing to serve with; set self_signed: true to have dup generate a pair, or point cert_file and key_file at a certificate you manage")
	}
	if len(c.AgentSocket) > MaxSocketPathLen {
		return fmt.Errorf("agent_socket path is %d bytes; the kernel limit for unix sockets is %d, pick a shorter path", len(c.AgentSocket), MaxSocketPathLen)
	}
	return nil
}

func (c *Config) resolveSecrets() error {
	serviceGID, haveServiceGID := lookupServiceGID(c.AgentPeerUser)

	bearer, err := resolveSecret("bearer token", c.Auth.BearerToken, c.Auth.BearerTokenFile, "UPDATER_BEARER_TOKEN", serviceGID, haveServiceGID)
	if err != nil {
		return err
	}
	gh, err := resolveSecret("github secret", c.Auth.GitHubSecret, c.Auth.GitHubSecretFile, "UPDATER_GITHUB_SECRET", serviceGID, haveServiceGID)
	if err != nil {
		return err
	}
	if len(bearer) == 0 && len(gh) == 0 {
		return fmt.Errorf("auth: at least one of bearer token or github secret must be configured")
	}
	c.Auth.bearer = bearer
	c.Auth.ghSecret = gh
	c.Auth.BearerToken = ""
	c.Auth.GitHubSecret = ""
	return nil
}

func lookupServiceGID(name string) (int64, bool) {
	if name == "" {
		return 0, false
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, false
	}
	gid, err := strconv.ParseInt(u.Gid, 10, 64)
	if err != nil {
		return 0, false
	}
	return gid, true
}

func resolveSecret(label, inline, file, envKey string, serviceGID int64, haveServiceGID bool) ([]byte, error) {
	var val string
	switch {
	case os.Getenv(envKey) != "":
		val = os.Getenv(envKey)
	case file != "":
		if !filepath.IsAbs(file) {
			return nil, fmt.Errorf("auth: %s file must be an absolute path", label)
		}
		info, err := os.Stat(file)
		if err != nil {
			return nil, fmt.Errorf("auth: %s file: %w", label, err)
		}
		if err := checkSecretFile(file, info, serviceGID, haveServiceGID); err != nil {
			return nil, fmt.Errorf("auth: %s file %s %s", label, file, err)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("auth: %s file: %w", label, err)
		}
		val = string(b)
	default:
		val = inline
	}

	val = strings.TrimSpace(val)
	if val == "" {
		return nil, nil
	}
	if len(val) < MinSecretLen {
		return nil, fmt.Errorf("auth: %s must be at least %d characters", label, MinSecretLen)
	}
	return []byte(val), nil
}

func checkSecretFile(path string, info os.FileInfo, serviceGID int64, haveServiceGID bool) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("is not a regular file")
	}

	perm := info.Mode().Perm()
	if perm&0o007 != 0 || perm&0o020 != 0 {
		return fmt.Errorf("has mode %04o; it must not be world accessible or group writable (use 0640 root:dup, or 0600)", perm)
	}

	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != 0 && int64(stat.Uid) != int64(os.Getuid()) {
			return fmt.Errorf("is owned by uid %d; it must be owned by root", stat.Uid)
		}
		// The group must be the account the service runs as. Comparing against the
		// current process group instead would reject root:dup 0640 whenever a root
		// command such as `dup check` reads it, which is every ExecStartPre.
		if perm&0o040 != 0 {
			fileGID := int64(stat.Gid)
			allowed := fileGID == int64(os.Getgid())
			if haveServiceGID && fileGID == serviceGID {
				allowed = true
			}
			if !allowed {
				return fmt.Errorf("is group readable by gid %d, which is neither this process's group (%d) nor the service account's group; anyone in that group could read it", stat.Gid, os.Getgid())
			}
		}
	}

	dir := filepath.Dir(path)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("directory %s could not be checked: %w", dir, err)
	}
	if dperm := dirInfo.Mode().Perm(); dperm&0o022 != 0 {
		return fmt.Errorf("sits in %s which is group or world writable (mode %04o), so the file can be replaced", dir, dperm)
	}
	return nil
}

func (c *Config) validateNotify() error {
	if c.Notify.URL == "" {
		return nil
	}
	u, err := url.Parse(c.Notify.URL)
	if err != nil {
		return fmt.Errorf("notify: invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("notify: url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("notify: url must have a host")
	}
	switch c.Notify.Format {
	case "", "auto", "dup", "discord", "slack", "teams", "google-chat":
	default:
		return fmt.Errorf("notify: format %q is not one of auto, dup, discord, slack, teams or google-chat", c.Notify.Format)
	}
	// A warning, never an error: dup check gates ExecStartPre for both units,
	// and a webhook nobody reads is not a reason to refuse to start. Only fires
	// when the operator has overridden the detection with something that cannot
	// work, since the default resolves this on its own.
	if c.Notify.Format == "dup" && IsDiscordWebhook(c.Notify.URL) {
		c.warnings = append(c.warnings,
			"notify.format is dup but notify.url is a discord webhook, which refuses that payload; remove the format line to let dup detect it, and check with: sudo dup notify")
	}
	return nil
}

// IsDiscordWebhook spots the endpoints that only accept discord's own body.
func IsDiscordWebhook(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "discord.com" && host != "discordapp.com" &&
		!strings.HasSuffix(host, ".discord.com") && !strings.HasSuffix(host, ".discordapp.com") {
		return false
	}
	return strings.Contains(u.Path, "/webhooks/")
}

// A config with no targets is valid. dup still enumerates what is on the host,
// which is how you decide what to put under its control in the first place.
func (c *Config) validateTargets(checkPaths bool) error {
	c.byName = make(map[string]*Target, len(c.Targets))

	for i, t := range c.Targets {
		if t == nil {
			return fmt.Errorf("targets[%d]: empty entry", i)
		}
		if !targetNameRe.MatchString(t.Name) {
			return fmt.Errorf("targets[%d]: name %q must match %s", i, t.Name, targetNameRe)
		}
		if _, dup := c.byName[t.Name]; dup {
			return fmt.Errorf("targets: duplicate name %q", t.Name)
		}

		store, err := dockercfg.StoreDir(c.DockerConfigDir, t.Name)
		if err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		t.dockerCfg = store

		if !filepath.IsAbs(t.Dir) {
			return fmt.Errorf("target %q: dir must be an absolute path", t.Name)
		}
		t.Dir = filepath.Clean(t.Dir)

		dirPresent := true
		if checkPaths {
			info, err := os.Stat(t.Dir)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				dirPresent = false
				c.warnings = append(c.warnings, fmt.Sprintf("stack %q points at %s, which does not exist", t.Name, t.Dir))
			case err != nil:
				return fmt.Errorf("target %q: dir: %w", t.Name, err)
			case !info.IsDir():
				return fmt.Errorf("target %q: dir %s is not a directory", t.Name, t.Dir)
			}
			if dirPresent && t.ComposeFile == "" {
				if err := requireDefaultComposeFile(t); err != nil {
					return err
				}
			}
		}

		if err := validateProjectFile(t.Name, "compose_file", t.Dir, t.ComposeFile, checkPaths && dirPresent); err != nil {
			return err
		}
		if err := validateProjectFile(t.Name, "env_file", t.Dir, t.EnvFile, checkPaths && dirPresent); err != nil {
			return err
		}

		for _, s := range t.Services {
			if !serviceNameRe.MatchString(s) {
				return fmt.Errorf("target %q: service %q must match %s", t.Name, s, serviceNameRe)
			}
		}
		if t.ImageTagEnv != "" && !envNameRe.MatchString(t.ImageTagEnv) {
			return fmt.Errorf("target %q: image_tag_env %q must match %s", t.Name, t.ImageTagEnv, envNameRe)
		}

		if t.Rollback == nil {
			t.Rollback = c.Defaults.Rollback
		}
		if t.PullTimeout <= 0 {
			t.PullTimeout = c.Defaults.PullTimeout
		}
		if t.HealthTimeout <= 0 {
			t.HealthTimeout = c.Defaults.HealthTimeout
		}
		if t.JobTimeout <= 0 {
			t.JobTimeout = c.Defaults.JobTimeout
		}
		if t.StabilityWindow <= 0 {
			t.StabilityWindow = c.Defaults.StabilityWindow
		}
		if t.CheckInterval <= 0 {
			t.CheckInterval = c.Defaults.CheckInterval
		}
		t.soak = *c.Defaults.Soak
		if t.Soak != nil {
			t.soak = *t.Soak
		}
		if t.soak < 0 {
			return fmt.Errorf("target %q: soak cannot be negative", t.Name)
		}
		if err := validateHook(t.Name, t.PreUpdate, checkPaths); err != nil {
			return err
		}
		if t.AutoUpdate && t.CheckInterval < MinCheckInterval {
			return fmt.Errorf("target %q: check_interval %s is shorter than the %s minimum; polling a registry harder than that gets you rate limited", t.Name, t.CheckInterval, MinCheckInterval)
		}
		if t.StabilityWindow >= t.HealthTimeout {
			return fmt.Errorf("target %q: stability_window (%s) must be shorter than health_timeout (%s)", t.Name, t.StabilityWindow, t.HealthTimeout)
		}

		c.byName[t.Name] = t
	}

	projects := make(map[string]string, len(c.Targets))
	for _, t := range c.Targets {
		project := filepath.Base(t.Dir)
		if other, clash := projects[project]; clash {
			return fmt.Errorf("targets %q and %q both live in a directory called %q, so compose would treat them as the same project and updating one could stop the other's containers; rename one directory", other, t.Name, project)
		}
		projects[project] = t.Name
	}
	return nil
}

func validateHook(target string, h *Hook, checkPaths bool) error {
	if !h.Configured() {
		return nil
	}
	if !filepath.IsAbs(h.Command) {
		return fmt.Errorf("target %q: pre_update.command must be an absolute path", target)
	}
	if h.Timeout <= 0 {
		h.Timeout = 10 * time.Minute
	}
	if !checkPaths {
		return nil
	}

	info, err := os.Stat(h.Command)
	if err != nil {
		return fmt.Errorf("target %q: pre_update.command: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target %q: pre_update.command %s is not a regular file", target, h.Command)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("target %q: pre_update.command %s is not executable", target, h.Command)
	}
	return nil
}

func validateProjectFile(target, field, dir, name string, checkPaths bool) error {
	if name == "" {
		return nil
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("target %q: %s must be a bare filename inside dir, got %q", target, field, name)
	}
	if !checkPaths {
		return nil
	}

	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); err != nil {
		return fmt.Errorf("target %q: %s: %w", target, field, err)
	}

	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("target %q: dir: %w", target, err)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return fmt.Errorf("target %q: %s: %w", target, field, err)
	}
	if resolved != filepath.Join(resolvedDir, name) {
		return fmt.Errorf("target %q: %s %q resolves to %s, outside its own directory; symlinked compose and env files are refused", target, field, name, resolved)
	}
	return nil
}

func requireDefaultComposeFile(t *Target) error {
	for _, n := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if _, err := os.Stat(filepath.Join(t.Dir, n)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("target %q: no compose file found in %s and compose_file is unset", t.Name, t.Dir)
}

func (c *Config) Target(name string) (*Target, bool) {
	t, ok := c.byName[name]
	return t, ok
}

func (c *Config) TargetNames() []string {
	names := make([]string, 0, len(c.Targets))
	for _, t := range c.Targets {
		names = append(names, t.Name)
	}
	return names
}

func (c *Config) BearerToken() []byte  { return c.Auth.bearer }
func (c *Config) GitHubSecret() []byte { return c.Auth.ghSecret }

func ValidImageTag(tag string) bool { return imageTagRe.MatchString(tag) }

func readConfigError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("there is no config at %s.\n\n"+
			"Create one from the reference that ships with dup:\n\n"+
			"  sudo cp /etc/dup/config.example.yml %s\n"+
			"  sudo chown root:dup %s && sudo chmod 0640 %s\n"+
			"  sudo nano %s\n\n"+
			"Then:  sudo dup check", path, path, path, path, path)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("not allowed to read %s.\n\n"+
			"It is owned root:dup and not world readable on purpose: it names every\n"+
			"stack dup may restart. Either run the command with sudo, or join the group:\n\n"+
			"  sudo usermod -aG dup $USER   (then log out and back in)", path)
	default:
		return fmt.Errorf("read config %s: %w", path, err)
	}
}
