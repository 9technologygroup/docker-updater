package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/server"
)

const (
	maxAPIBody = 4 << 20

	jobPollInterval = 700 * time.Millisecond
)

type apiClient struct {
	base  string
	token string
	http  *http.Client
}

type apiStatusError struct {
	status int
	msg    string
	raw    []byte
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("the API refused the request (%d): %s", e.status, e.msg)
}

// clientBase is the URL the CLI dials. It follows the configured scheme, and a
// wildcard listen address stands in as loopback: 0.0.0.0 is not dialable and is
// never a name in the certificate.
func clientBase(cfg *config.Config) string {
	scheme := "http"
	if cfg.TLS.IsEnabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return scheme + "://" + cfg.Listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func newAPIClient(cfg *config.Config) (*apiClient, error) {
	token := string(cfg.BearerToken())
	if token == "" {
		return nil, fmt.Errorf("no bearer token is configured, so this command cannot talk to the API.\n\n"+
			"Add one to %s:\n\n  auth:\n    bearer_token_file: /etc/dup/bearer.token", DefaultConfigPath)
	}

	httpClient := &http.Client{Timeout: 6 * time.Minute}
	if cfg.TLS.IsEnabled() {
		tlsConfig, err := clientTLS(cfg)
		if err != nil {
			return nil, err
		}
		httpClient.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}

	return &apiClient{base: clientBase(cfg), token: token, http: httpClient}, nil
}

// clientTLS trusts the configured certificate in addition to the system roots,
// so a self-signed pair verifies without disabling verification. The printed
// fingerprint is the point of that certificate; skipping verification wastes it.
func clientTLS(cfg *config.Config) (*tls.Config, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if cfg.TLS.CertFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CertFile)
		if err != nil {
			return nil, fmt.Errorf("tls is enabled but %s could not be read: %w", cfg.TLS.CertFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls is enabled but %s holds no usable certificate", cfg.TLS.CertFile)
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}

func (c *apiClient) do(ctx context.Context, method, path, body string, out any) error {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return apiUnreachable(c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if resp.StatusCode >= 400 {
		// Decoded even on an error so a caller that knows the shape of the body
		// can still read it.
		if out != nil {
			_ = json.Unmarshal(raw, out)
		}
		return &apiStatusError{status: resp.StatusCode, msg: apiError(raw), raw: raw}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("could not read the API response: %w", err)
	}
	return nil
}

func apiError(raw []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return strings.TrimSpace(string(raw))
}

func runUpdate(args []string) error {
	fs, configPath := newFlagSet("update")
	tag := fs.String("tag", "", "image tag to deploy (only for targets with image_tag_env set)")
	reason := fs.String("reason", "", "why this update is happening, recorded on the job")
	dryRun := fs.Bool("dry-run", false, "pull and report what would change, without recreating anything")
	force := fs.Bool("force", false, "recreate even if the images have not changed")
	wait := fs.Duration("wait", 4*time.Minute, "how long to wait for the update to finish, up to "+server.MaxWait.String())

	target, err := oneTarget(fs, args, "dup update <stack> [flags]")
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("usage: dup update <stack> [flags]")
	}
	if *wait > server.MaxWait {
		fmt.Fprintf(os.Stderr, "note: --wait %s exceeds the %s the API will hold a request open, using %s\n",
			wait.String(), server.MaxWait, server.MaxWait)
		*wait = server.MaxWait
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}
	if _, ok := cfg.Target(target); !ok {
		return unknownStack(target, cfg)
	}

	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"tag": *tag, "reason": *reason, "dry_run": *dryRun, "force": *force,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait+time.Minute)
	defer cancel()

	var snap job.Snapshot
	if err := client.do(ctx, http.MethodPost, "/v1/targets/"+target+"/update", string(body), &snap); err != nil {
		var se *apiStatusError
		if errors.As(err, &se) && se.status == http.StatusConflict {
			return alreadyRunning(target, se.raw)
		}
		return err
	}
	if snap.ID == "" {
		return fmt.Errorf("the API accepted the update for %s but returned no job to follow", target)
	}

	renderer := newJobRenderer(os.Stdout, stdoutIsTTY())
	final, err := client.followJob(ctx, renderer, snap, *wait)
	if err != nil {
		return err
	}
	renderer.finish(final)

	if !final.State.Terminal() {
		fmt.Printf("\nStill running. Follow it with:  dup status %s\n", target)
		return nil
	}
	if !final.State.OK() {
		return fmt.Errorf("update finished as %s", final.State)
	}
	return nil
}

// followJob polls until the job is terminal or the wait budget runs out. The
// budget expiring is not a failure: the update is still running on the host.
func (c *apiClient) followJob(ctx context.Context, r *jobRenderer, snap job.Snapshot, wait time.Duration) (job.Snapshot, error) {
	deadline := time.Now().Add(wait)
	tick := time.NewTicker(jobPollInterval)
	defer tick.Stop()

	for !snap.State.Terminal() {
		r.update(snap)
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-tick.C:
		}
		var next job.Snapshot
		if err := c.do(ctx, http.MethodGet, "/v1/jobs/"+snap.ID, "", &next); err != nil {
			return snap, err
		}
		snap = next
	}
	return snap, nil
}

func printJob(snap job.Snapshot) {
	now := time.Now()
	for _, l := range jobSummaryLines(snap, false) {
		fmt.Println(l)
	}
	for _, l := range jobStepLines(snap, now, false) {
		fmt.Println(l)
	}
}

// alreadyRunning turns a 409 into something actionable. The API sends the job
// that holds the lock, and printing only the error string threw it away.
func alreadyRunning(target string, raw []byte) error {
	var body struct {
		RunningJob job.Snapshot `json:"running_job"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.RunningJob.ID == "" {
		return fmt.Errorf("an update is already running for %s; follow it with: dup status %s", target, target)
	}
	started := body.RunningJob.StartedAt.Local().Format("15:04:05")
	return fmt.Errorf("an update is already running for %s: job %s, started %s (%s)\n\nFollow it with:  dup status %s",
		target, body.RunningJob.ID, started, body.RunningJob.Trigger, target)
}

func unknownStack(name string, cfg *config.Config) error {
	return fmt.Errorf("unknown stack %q\n\nConfigured stacks: %s\nRun 'dup list' to see them with their update policy",
		name, joinOr(cfg.TargetNames(), "none"))
}
