package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
)

const (
	FormatDup     = "dup"
	FormatDiscord = "discord"
)

// maxResponse is what is kept from a rejection. The reason a webhook refused
// is in its body, and discarding it was why a misconfigured endpoint looked
// silent rather than broken.
const maxResponse = 2 << 10

type Notifier struct {
	url     string
	format  string
	headers map[string]string
	client  *http.Client
	log     *slog.Logger
	host    string
}

// Result is what came back, so a caller can show it rather than guess.
type Result struct {
	URL      string
	Format   string
	Status   int
	Body     string
	Sent     []byte
	Duration time.Duration
}

func (r Result) OK() bool { return r.Status > 0 && r.Status < 300 }

type payload struct {
	Host       string       `json:"host"`
	Target     string       `json:"target"`
	JobID      string       `json:"job_id"`
	State      string       `json:"state"`
	OK         bool         `json:"ok"`
	Summary    string       `json:"summary"`
	Message    string       `json:"message,omitempty"`
	Trigger    string       `json:"trigger,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Changed    []string     `json:"changed_services,omitempty"`
	Test       bool         `json:"test,omitempty"`
	Images     []imageState `json:"images,omitempty"`
	DurationMS int64        `json:"duration_ms"`
	FinishedAt time.Time    `json:"finished_at"`
}

type imageState struct {
	Service string `json:"service"`
	Image   string `json:"image,omitempty"`
	State   string `json:"state,omitempty"`
	Health  string `json:"health,omitempty"`
}

func New(cfg config.Notify, host string, log *slog.Logger) *Notifier {
	if cfg.URL == "" {
		return nil
	}
	format := cfg.Format
	if format == "" {
		format = FormatDup
	}
	return &Notifier{
		url:     cfg.URL,
		format:  format,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: cfg.Timeout},
		log:     log,
		host:    host,
	}
}

func (n *Notifier) URL() string    { return n.url }
func (n *Notifier) Format() string { return n.format }

// Deliver posts one notification and reports what happened. Notify wraps it for
// the scheduler, which only wants it logged.
func (n *Notifier) Deliver(ctx context.Context, snap job.Snapshot, test bool) (Result, error) {
	res := Result{URL: n.url, Format: n.format}

	body, err := n.encode(snap, test)
	if err != nil {
		return res, err
	}
	res.Sent = body

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dup")
	for k, v := range n.headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := n.client.Do(req)
	res.Duration = time.Since(start)
	if err != nil {
		return res, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	res.Status = resp.StatusCode
	res.Body = strings.TrimSpace(string(raw))
	return res, nil
}

func (n *Notifier) encode(snap job.Snapshot, test bool) ([]byte, error) {
	p := n.build(snap)
	p.Test = test
	if n.format == FormatDiscord {
		// Discord refuses anything without content, embeds or files, so the
		// summary sentence is what it gets.
		return json.Marshal(struct {
			Content string `json:"content"`
		}{Content: p.Summary})
	}
	return json.Marshal(p)
}

func (n *Notifier) Notify(ctx context.Context, snap job.Snapshot) {
	if n == nil {
		return
	}

	res, err := n.Deliver(ctx, snap, false)
	if err != nil {
		n.log.Error("notify: request failed", "job", snap.ID, "url", n.url, "error", err)
		return
	}
	if !res.OK() {
		// The body is the only thing that says why, and dropping it made a
		// misconfigured endpoint look silent instead of broken.
		n.log.Error("notify: rejected", "job", snap.ID, "url", n.url,
			"status", res.Status, "format", n.format, "response", res.Body)
		return
	}
	n.log.Info("notify: delivered", "job", snap.ID, "status", res.Status, "format", n.format)
}

func (n *Notifier) build(snap job.Snapshot) payload {
	finished := time.Now().UTC()
	if snap.FinishedAt != nil {
		finished = *snap.FinishedAt
	}

	images := make([]imageState, 0, len(snap.After))
	for _, s := range snap.After {
		images = append(images, imageState{Service: s.Service, Image: s.Image, State: s.State, Health: s.Health})
	}

	return payload{
		Host:       n.host,
		Target:     snap.Target,
		JobID:      snap.ID,
		State:      string(snap.State),
		OK:         snap.State.OK(),
		Summary:    summarise(n.host, snap),
		Message:    snap.Message,
		Trigger:    snap.Trigger,
		Reason:     snap.Reason,
		Changed:    snap.Changed,
		Images:     images,
		DurationMS: snap.DurationMS,
		FinishedAt: finished,
	}
}

func summarise(host string, snap job.Snapshot) string {
	var b strings.Builder
	switch snap.State {
	case job.StateSucceeded:
		b.WriteString("Updated ")
	case job.StateNoChange:
		b.WriteString("No change for ")
	case job.StateDryRun:
		b.WriteString("Dry run for ")
	case job.StateRolledBack:
		b.WriteString("Rolled back ")
	case job.StateRollbackFailed:
		b.WriteString("ROLLBACK FAILED for ")
	default:
		b.WriteString("Update FAILED for ")
	}
	b.WriteString(snap.Target)
	b.WriteString(" on ")
	b.WriteString(host)
	if len(snap.Changed) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(snap.Changed, ", "))
		b.WriteString(")")
	}
	if snap.Message != "" {
		b.WriteString(": ")
		b.WriteString(snap.Message)
	}
	return b.String()
}
