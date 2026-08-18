package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/9technologygroup/docker-updater/internal/version"
)

const (
	DefaultRepo  = "9technologygroup/docker-updater"
	DefaultBase  = "https://api.github.com"
	DefaultCache = "/var/lib/dup/update-check.json"
	DefaultTTL   = 24 * time.Hour

	// DisableEnv switches every check off, everywhere, and cannot be overridden
	// by a flag. Anyone setting it fleet-wide means it.
	DisableEnv = "DUP_NO_UPDATE_CHECK"
	RepoEnv    = "DUP_GITHUB_REPO"
	TokenEnv   = "DUP_GITHUB_TOKEN" //nolint:gosec // the name of a variable, not a token
	CacheEnv   = "DUP_CACHE_FILE"

	maxBody      = 1 << 20
	maxRedirects = 3
)

var (
	ErrDisabled    = errors.New("update checks are disabled")
	ErrDevBuild    = errors.New("development build")
	ErrRateLimited = errors.New("github rate limit reached")
	ErrNoRelease   = errors.New("no published release")
)

var repoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

type Release struct {
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at"`
}

// NotesURL is derived from the configured repo, never from the API response or
// the cache, so nothing off the wire can steer a URL we print.
func (r Release) NotesURL(repo string) string {
	if r.Tag == "" || !repoRe.MatchString(repo) {
		return ""
	}
	return "https://github.com/" + repo + "/releases/tag/" + url.PathEscape(r.Tag)
}

type Status struct {
	Current   string
	Latest    Release
	Newer     bool
	CheckedAt time.Time
	FromCache bool
}

type Checker struct {
	Repo      string
	BaseURL   string
	CachePath string
	TTL       time.Duration
	Client    *http.Client
	Now       func() time.Time
}

func New() *Checker {
	return &Checker{
		Repo:      envOr(RepoEnv, DefaultRepo),
		BaseURL:   DefaultBase,
		CachePath: envOr(CacheEnv, DefaultCache),
		TTL:       DefaultTTL,
		Client:    &http.Client{Timeout: 10 * time.Second},
		Now:       time.Now,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Disabled reports whether checks are switched off by the environment.
func Disabled() bool { return os.Getenv(DisableEnv) != "" }

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Check compares current against the newest published release. force skips the
// cache TTL; without it a fresh cache entry is used and no request is made.
func (c *Checker) Check(ctx context.Context, current string, force bool) (Status, error) {
	if Disabled() {
		return Status{Current: current}, ErrDisabled
	}
	if !Checkable(current) {
		return Status{Current: current}, ErrDevBuild
	}

	cached, ok := c.readCache()
	if ok && !force && c.now().Sub(cached.CheckedAt) < c.TTL {
		return c.status(current, cached.Release, cached.CheckedAt, true)
	}
	// A rate-limit reset in the future suppresses the request whatever the TTL says.
	if ok && c.now().Before(cached.RetryAfter) {
		if cached.Release.Tag == "" {
			return Status{Current: current}, ErrRateLimited
		}
		return c.status(current, cached.Release, cached.CheckedAt, true)
	}

	rel, err := c.Latest(ctx)
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			c.writeCache(cacheFile{Release: cached.Release, CheckedAt: cached.CheckedAt, RetryAfter: rateLimitReset(err, c.now())})
		}
		if ok && cached.Release.Tag != "" {
			return c.status(current, cached.Release, cached.CheckedAt, true)
		}
		return Status{Current: current}, err
	}

	now := c.now()
	c.writeCache(cacheFile{Release: rel, CheckedAt: now})
	return c.status(current, rel, now, false)
}

func (c *Checker) status(current string, rel Release, checkedAt time.Time, fromCache bool) (Status, error) {
	s := Status{Current: current, Latest: rel, CheckedAt: checkedAt, FromCache: fromCache}
	if rel.Tag == "" {
		return s, ErrNoRelease
	}
	cmp, err := Compare(rel.Tag, current)
	if err != nil {
		return s, err
	}
	s.Newer = cmp > 0
	return s, nil
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

// Latest fetches the newest published release. GitHub defines /releases/latest as
// the newest non-draft, non-prerelease release, which is what a stable install
// should hear about, and it is the endpoint install.sh already uses.
func (c *Checker) Latest(ctx context.Context) (Release, error) {
	repo := c.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	if !repoRe.MatchString(repo) {
		return Release{}, fmt.Errorf("%q is not a valid owner/name repository", repo)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBase
	}

	endpoint := base + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "dup/"+version.Short())
	if tok := os.Getenv(TokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.client(base).Do(req)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Release{}, ErrNoRelease
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return Release{}, rateLimitErr(resp.Header.Get("X-RateLimit-Reset"))
		}
		return Release{}, fmt.Errorf("github returned %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Release{}, fmt.Errorf("github returned %d", resp.StatusCode)
	}

	var gh ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&gh); err != nil {
		return Release{}, fmt.Errorf("could not read the release response: %w", err)
	}
	if gh.Draft || gh.Prerelease || gh.TagName == "" {
		return Release{}, ErrNoRelease
	}
	if !Valid(gh.TagName) {
		return Release{}, fmt.Errorf("github reported a tag that is not a version: %q", clip(gh.TagName))
	}
	return Release{Tag: gh.TagName, PublishedAt: gh.PublishedAt}, nil
}

// client refuses a redirect that leaves the configured host, so a renamed repo
// still resolves but nothing can walk us onto another origin.
func (c *Checker) client(base string) *http.Client {
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if hc.CheckRedirect != nil {
		return hc
	}
	baseHost := ""
	if u, err := url.Parse(base); err == nil {
		baseHost = u.Host
	}
	clone := *hc
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if baseHost != "" && req.URL.Host != baseHost {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Host)
		}
		return nil
	}
	return &clone
}

type rateLimitError struct{ reset time.Time }

func (e *rateLimitError) Error() string {
	if e.reset.IsZero() {
		return ErrRateLimited.Error()
	}
	return fmt.Sprintf("%s, try again after %s", ErrRateLimited.Error(), e.reset.UTC().Format("15:04 MST"))
}

func (e *rateLimitError) Is(target error) bool { return target == ErrRateLimited }

func rateLimitErr(reset string) error {
	e := &rateLimitError{}
	if secs, err := strconv.ParseInt(reset, 10, 64); err == nil && secs > 0 {
		e.reset = time.Unix(secs, 0)
	}
	return e
}

func rateLimitReset(err error, fallbackFrom time.Time) time.Time {
	var rl *rateLimitError
	if errors.As(err, &rl) && !rl.reset.IsZero() {
		return rl.reset
	}
	return fallbackFrom.Add(time.Hour)
}

func clip(s string) string {
	const max = 40
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// Cached returns the last known status without making a network call, so an API
// handler can report it without letting a caller drive requests to GitHub.
func (c *Checker) Cached(current string) (Status, bool) {
	cf, ok := c.readCache()
	if !ok || cf.Release.Tag == "" {
		return Status{}, false
	}
	s, err := c.status(current, cf.Release, cf.CheckedAt, true)
	if err != nil {
		return Status{}, false
	}
	return s, true
}
