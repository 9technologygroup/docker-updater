package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
)

const (
	// FormatAuto picks a shape from the webhook's hostname. It is the default
	// because no vendor documents what it does with fields it does not know, so
	// a single body carrying every platform's field would rest on luck.
	FormatAuto       = "auto"
	FormatDup        = "dup"
	FormatDiscord    = "discord"
	FormatSlack      = "slack"
	FormatTeams      = "teams"
	FormatGoogleChat = "google-chat"
)

// Formats are every value notify.format accepts.
var Formats = []string{FormatAuto, FormatDup, FormatDiscord, FormatSlack, FormatTeams, FormatGoogleChat}

// Detect reads the destination off the URL. Only the hosted platforms have
// fixed published hostnames; a self-hosted one is indistinguishable from any
// other endpoint, so it falls back to the payload that carries everything.
func Detect(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return FormatDup
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "discord.com" || host == "discordapp.com" ||
		strings.HasSuffix(host, ".discord.com") || strings.HasSuffix(host, ".discordapp.com"):
		return FormatDiscord
	case host == "hooks.slack.com":
		return FormatSlack
	case host == "chat.googleapis.com":
		return FormatGoogleChat
	}
	return FormatDup
}

// maxResponse is what is kept from a rejection. The reason a webhook refused
// is in its body, and discarding it was why a misconfigured endpoint looked
// silent rather than broken.
const maxResponse = 2 << 10

type Notifier struct {
	url      string
	format   string
	resolved string
	detected bool
	cfg      config.Notify
	headers  map[string]string
	client   *http.Client
	log      *slog.Logger
	host     string
}

// Result is what came back, so a caller can show it rather than guess.
type Result struct {
	URL      string
	Format   string
	Detected bool
	Status   int
	Body     string
	Sent     []byte
	Duration time.Duration
}

func (r Result) OK() bool { return r.Status > 0 && r.Status < 300 }

type payload struct {
	Host       string       `json:"host"`
	Target     string       `json:"target"`
	JobID      string       `json:"job_id,omitempty"`
	State      string       `json:"state,omitempty"`
	OK         bool         `json:"ok"`
	Summary    string       `json:"summary"`
	Message    string       `json:"message,omitempty"`
	Trigger    string       `json:"trigger,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Changed    []string     `json:"changed_services,omitempty"`
	Event      string       `json:"event"`
	Test       bool         `json:"test,omitempty"`
	AppliesAt  *time.Time   `json:"applies_at,omitempty"`
	Error      string       `json:"error,omitempty"`
	Text       string       `json:"text,omitempty"`
	Images     []imageState `json:"images,omitempty"`
	DurationMS int64        `json:"duration_ms,omitempty"`
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
		format = FormatAuto
	}
	resolved, detected := format, false
	if format == FormatAuto {
		resolved, detected = Detect(cfg.URL), true
	}
	return &Notifier{
		url:      cfg.URL,
		format:   format,
		resolved: resolved,
		detected: detected,
		cfg:      cfg,
		headers:  cfg.Headers,
		client:   &http.Client{Timeout: cfg.Timeout},
		log:      log,
		host:     host,
	}
}

func (n *Notifier) URL() string      { return n.url }
func (n *Notifier) Format() string   { return n.format }
func (n *Notifier) Resolved() string { return n.resolved }
func (n *Notifier) Detected() bool   { return n.detected }

// eventFor maps a finished job onto the event an operator subscribes to.
func eventFor(state job.State) string {
	switch state {
	case job.StateSucceeded:
		return config.EventUpdateSucceeded
	case job.StateNoChange:
		return config.EventUpdateNoChange
	case job.StateDryRun:
		return config.EventUpdateDryRun
	case job.StateRolledBack:
		return config.EventUpdateRolledBack
	case job.StateRollbackFailed:
		return config.EventUpdateRollbackFailed
	default:
		return config.EventUpdateFailed
	}
}

// Deliver posts one notification and reports what happened. Notify wraps it for
// the scheduler, which only wants it logged.
func (n *Notifier) Deliver(ctx context.Context, snap job.Snapshot, test bool) (Result, error) {
	return n.deliver(ctx, n.jobPayload(snap, eventFor(snap.State), test))
}

func (n *Notifier) deliver(ctx context.Context, p payload) (Result, error) {
	res := Result{URL: n.url, Format: n.resolved, Detected: n.detected}

	body, err := n.encode(p)
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

func (n *Notifier) jobPayload(snap job.Snapshot, event string, test bool) payload {
	p := n.build(snap)
	p.Event = event
	p.Test = test
	return p
}

func (n *Notifier) encode(p payload) ([]byte, error) {
	switch n.resolved {
	case FormatDiscord:
		// Discord refuses anything without content, embeds or files.
		return json.Marshal(struct {
			Content string `json:"content"`
		}{Content: p.Summary})
	case FormatSlack, FormatGoogleChat, FormatTeams:
		return json.Marshal(struct {
			Text string `json:"text"`
		}{Text: p.Summary})
	}

	// text rides along with the full payload so a self-hosted Mattermost or a
	// Teams flow renders something, while n8n keeps every field it already reads.
	p.Text = p.Summary
	return json.Marshal(p)
}

// Wants reports whether this event is subscribed to, so a caller can skip the
// work of building one nobody asked for.
func (n *Notifier) Wants(event string) bool { return n != nil && n.cfg.Wants(event) }

func (n *Notifier) Notify(ctx context.Context, snap job.Snapshot) {
	if n == nil {
		return
	}
	event := eventFor(snap.State)
	if !n.cfg.Wants(event) {
		n.log.Debug("notify: not subscribed", "job", snap.ID, "event", event)
		return
	}
	n.send(ctx, snap.ID, event, n.jobPayload(snap, event, false))
}

// NotifyStarted announces an update that has just begun. It is off by default:
// it lands seconds before containers are recreated, too late to act on.
func (n *Notifier) NotifyStarted(ctx context.Context, snap job.Snapshot) {
	if !n.Wants(config.EventUpdateStarted) {
		return
	}
	p := n.jobPayload(snap, config.EventUpdateStarted, false)
	p.OK = true
	p.Summary = "Starting update of " + snap.Target + " on " + n.host
	n.send(ctx, snap.ID, config.EventUpdateStarted, p)
}

// Available fires once when a new image is first seen, which is the point in
// the whole cycle where there is still time to pin a tag or turn auto update
// off before anything is recreated.
func (n *Notifier) Available(ctx context.Context, target string, services []string, appliesAt time.Time) {
	if !n.Wants(config.EventUpdateAvailable) {
		return
	}
	at := appliesAt.UTC()
	p := n.event(config.EventUpdateAvailable, target, true)
	p.Changed = services
	p.AppliesAt = &at
	p.Summary = "New image for " + joinOr(services, target) + " on " + target + " (" + n.host +
		"), applying at " + at.Format("15:04 MST")
	n.send(ctx, "", config.EventUpdateAvailable, p)
}

func (n *Notifier) Withdrawn(ctx context.Context, target, reason string) {
	if !n.Wants(config.EventUpdateWithdrawn) {
		return
	}
	p := n.event(config.EventUpdateWithdrawn, target, true)
	p.Message = reason
	p.Summary = "The pending image for " + target + " on " + n.host + " is gone, nothing will be applied"
	n.send(ctx, "", config.EventUpdateWithdrawn, p)
}

// CheckFailed and CheckRecovered are sent on the transition only. A stack that
// stays broken is not worth a notification every check_interval, because that
// is how people learn to ignore them.
func (n *Notifier) CheckFailed(ctx context.Context, target string, cause error) {
	if !n.Wants(config.EventCheckFailed) {
		return
	}
	p := n.event(config.EventCheckFailed, target, false)
	if cause != nil {
		p.Error = cause.Error()
	}
	p.Summary = "Cannot check " + target + " on " + n.host + " against its registry: " + firstLine(p.Error)
	n.send(ctx, "", config.EventCheckFailed, p)
}

func (n *Notifier) CheckRecovered(ctx context.Context, target string) {
	if !n.Wants(config.EventCheckRecovered) {
		return
	}
	p := n.event(config.EventCheckRecovered, target, true)
	p.Summary = "Checking " + target + " on " + n.host + " is working again"
	n.send(ctx, "", config.EventCheckRecovered, p)
}

func (n *Notifier) event(event, target string, ok bool) payload {
	return payload{
		Host: n.host, Target: target, Event: event, OK: ok,
		Trigger: "auto", FinishedAt: time.Now().UTC(),
	}
}

func (n *Notifier) send(ctx context.Context, id, event string, p payload) {
	res, err := n.deliver(ctx, p)
	if err != nil {
		n.log.Error("notify: request failed", "job", id, "event", event, "url", n.url, "error", err)
		return
	}
	if !res.OK() {
		// The body is the only thing that says why, and dropping it made a
		// misconfigured endpoint look silent instead of broken.
		n.log.Error("notify: rejected", "job", id, "event", event, "url", n.url,
			"status", res.Status, "format", n.resolved, "response", res.Body)
		return
	}
	n.log.Info("notify: delivered", "job", id, "event", event, "status", res.Status, "format", n.resolved)
}

func joinOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return strings.Join(values, ", ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
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
