package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

const (
	maxErrorBytes    = 8 << 10
	maxDiscoverBytes = 4 << 20
)

type Client struct {
	http   *http.Client
	socket string
}

func NewClient(socket string) *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Client{
		socket: socket,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
				DisableCompression: true,
			},
		},
	}
}

func (c *Client) Socket() string { return c.socket }

func (c *Client) url(path string) string { return "http://dup-agent" + path }

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(wire.HealthPath), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("update agent socket %s: %w", c.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update agent health check returned %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Update(ctx context.Context, t *config.Target, req job.Request, sink job.Sink) (job.State, string, error) {
	payload, err := json.Marshal(wire.ExecRequest{
		Target: t.Name,
		Tag:    req.Tag,
		DryRun: req.DryRun,
		Force:  req.Force,
	})
	if err != nil {
		return job.StateFailed, "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(wire.ExecPath), bytes.NewReader(payload))
	if err != nil {
		return job.StateFailed, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return job.StateFailed, "", fmt.Errorf("could not reach the update agent on %s: %w", c.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		return job.StateFailed, "", fmt.Errorf("the update agent refused the request (%d): %s", resp.StatusCode, agentError(body))
	}

	return c.consume(resp.Body, sink)
}

func (c *Client) consume(body io.Reader, sink job.Sink) (job.State, string, error) {
	dec := json.NewDecoder(body)

	var (
		state     job.State
		message   string
		gotResult bool
	)
	for {
		var ev wire.Event
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return job.StateFailed, "", fmt.Errorf("update agent stream ended badly: %w", err)
		}

		switch ev.Type {
		case wire.EventStep:
			if ev.Step != nil {
				sink.AddStep(*ev.Step)
			}
		case wire.EventBefore:
			sink.SetBefore(ev.States)
		case wire.EventAfter:
			sink.SetAfter(ev.States)
		case wire.EventChanged:
			sink.SetChanged(ev.Changed)
		case wire.EventResult:
			state, message, gotResult = ev.State, ev.Message, true
		}
	}

	if !gotResult {
		return job.StateFailed, "", errors.New("the update agent closed the stream without reporting a result; the stack may be mid-update")
	}
	return state, message, nil
}

func (c *Client) Check(ctx context.Context, target string) (wire.CheckResult, error) {
	var result wire.CheckResult

	payload, err := json.Marshal(wire.CheckRequest{Target: target})
	if err != nil {
		return result, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(wire.CheckPath), bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return result, fmt.Errorf("could not reach the update agent on %s: %w", c.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("the update agent refused the check (%d): %s", resp.StatusCode, agentError(body))
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("could not read the agent's check result: %w", err)
	}
	return result, nil
}

func (c *Client) Discover(ctx context.Context) (wire.DiscoverResult, error) {
	var result wire.DiscoverResult

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(wire.DiscoverPath), nil)
	if err != nil {
		return result, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return result, fmt.Errorf("could not reach the update agent on %s: %w", c.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiscoverBytes))
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("the update agent refused the request (%d): %s", resp.StatusCode, agentError(body))
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("could not read the agent's discovery result: %w", err)
	}
	return result, nil
}

func agentError(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return string(bytes.TrimSpace(body))
}
