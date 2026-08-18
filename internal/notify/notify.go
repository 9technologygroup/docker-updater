package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/job"
)

type Notifier struct {
	url     string
	headers map[string]string
	client  *http.Client
	log     *slog.Logger
	host    string
}

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
	return &Notifier{
		url:     cfg.URL,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: cfg.Timeout},
		log:     log,
		host:    host,
	}
}

func (n *Notifier) Notify(ctx context.Context, snap job.Snapshot) {
	if n == nil {
		return
	}

	body, err := json.Marshal(n.build(snap))
	if err != nil {
		n.log.Error("notify: marshal failed", "job", snap.ID, "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		n.log.Error("notify: build request failed", "job", snap.ID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dup")
	for k, v := range n.headers {
		req.Header.Set(k, v)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		n.log.Error("notify: request failed", "job", snap.ID, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		n.log.Error("notify: rejected", "job", snap.ID, "status", resp.StatusCode)
		return
	}
	n.log.Info("notify: delivered", "job", snap.ID, "status", resp.StatusCode)
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
