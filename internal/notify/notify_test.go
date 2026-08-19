package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func snap() job.Snapshot {
	now := time.Now().UTC()
	return job.Snapshot{
		ID: "abc123", Target: "app", State: job.StateSucceeded,
		Message: "updated web", Changed: []string{"web"}, FinishedAt: &now,
	}
}

func recorder(t *testing.T, status int, body string) (*httptest.Server, *[]byte, *http.Header) {
	t.Helper()
	var got []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

func TestDeliverPostsTheDupPayloadAndMarksATest(t *testing.T) {
	srv, got, hdr := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second}, "web01", quietLog())

	res, err := n.Deliver(context.Background(), snap(), true)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !res.OK() || res.Status != 200 {
		t.Fatalf("res = %+v, want a 2xx", res)
	}

	var p map[string]any
	if err := json.Unmarshal(*got, &p); err != nil {
		t.Fatalf("the body was not json: %v", err)
	}
	for k, want := range map[string]any{"host": "web01", "target": "app", "state": "succeeded", "ok": true, "test": true} {
		if p[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, p[k], want)
		}
	}
	if (*hdr).Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", (*hdr).Get("Content-Type"))
	}
	if (*hdr).Get("User-Agent") != "dup" {
		t.Errorf("user agent = %q", (*hdr).Get("User-Agent"))
	}
}

func TestARealNotificationCarriesNoTestField(t *testing.T) {
	srv, got, _ := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second}, "web01", quietLog())

	if _, err := n.Deliver(context.Background(), snap(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(*got), "\"test\"") {
		t.Errorf("a real notification must not claim to be a test: %s", *got)
	}
}

// Discord refuses any body without content, embeds or files, which is why a
// discord webhook url in notify.url silently did nothing.
func TestDiscordFormatSendsOnlyAMessage(t *testing.T) {
	srv, got, _ := recorder(t, 204, "")
	n := New(config.Notify{URL: srv.URL, Format: FormatDiscord, Timeout: 5 * time.Second}, "web01", quietLog())

	if _, err := n.Deliver(context.Background(), snap(), true); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(*got, &p); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if len(p) != 1 {
		t.Errorf("discord accepts content and nothing else, got %d fields: %s", len(p), *got)
	}
	content, _ := p["content"].(string)
	if !strings.Contains(content, "app") || !strings.Contains(content, "web01") {
		t.Errorf("content = %q, want the summary sentence", content)
	}
}

func TestDeliverKeepsTheRejectionBody(t *testing.T) {
	const reason = `{"message": "Cannot send an empty message", "code": 50006}`
	srv, _, _ := recorder(t, 400, reason)
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second}, "web01", quietLog())

	res, err := n.Deliver(context.Background(), snap(), true)
	if err != nil {
		t.Fatalf("a rejection is an answer, not a transport error: %v", err)
	}
	if res.OK() {
		t.Errorf("400 must not count as delivered")
	}
	if res.Body != reason {
		t.Errorf("body = %q, want the endpoint's own explanation", res.Body)
	}
}

// The reason an endpoint refused used to be dropped, so a misconfigured webhook
// looked silent in the journal rather than broken.
func TestNotifyLogsWhyItWasRejected(t *testing.T) {
	srv, _, _ := recorder(t, 400, `{"message":"Cannot send an empty message"}`)
	var logged bytes.Buffer
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second}, "web01",
		slog.New(slog.NewTextHandler(&logged, nil)))

	n.Notify(context.Background(), snap())

	out := logged.String()
	for _, want := range []string{"rejected", "400", "Cannot send an empty message"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q:\n%s", want, out)
		}
	}
}

func TestHeadersAreSentAndDefaultFormatIsDup(t *testing.T) {
	srv, _, hdr := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second,
		Headers: map[string]string{"X-Source": "dup", "Authorization": "Bearer t"}}, "web01", quietLog())

	if n.Format() != FormatAuto || n.Resolved() != FormatDup {
		t.Errorf("format = %q resolved = %q, want auto resolving to dup for an unknown host", n.Format(), n.Resolved())
	}
	if _, err := n.Deliver(context.Background(), snap(), false); err != nil {
		t.Fatal(err)
	}
	if (*hdr).Get("X-Source") != "dup" || (*hdr).Get("Authorization") != "Bearer t" {
		t.Errorf("configured headers were not sent: %v", *hdr)
	}
}

func TestNoURLMeansNoNotifierAndANilOneIsSafe(t *testing.T) {
	n := New(config.Notify{}, "web01", quietLog())
	if n != nil {
		t.Fatal("an empty url must not build a notifier")
	}
	n.Notify(context.Background(), snap())
}

func TestDetectReadsThePlatformOffTheHostname(t *testing.T) {
	cases := map[string]string{
		"https://discord.com/api/webhooks/1/abc":              FormatDiscord,
		"https://discordapp.com/api/webhooks/1/abc":           FormatDiscord,
		"https://ptb.discord.com/api/webhooks/1/abc":          FormatDiscord,
		"https://hooks.slack.com/services/T/B/X":              FormatSlack,
		"https://chat.googleapis.com/v1/spaces/S/messages":    FormatGoogleChat,
		"https://n8n.example.com/webhook/dup":                 FormatDup,
		"https://mattermost.example.com/hooks/abc":            FormatDup,
		"https://prod-01.uksouth.logic.azure.com/workflows/x": FormatDup,
		"not a url at all":                                    FormatDup,
	}
	for raw, want := range cases {
		if got := Detect(raw); got != want {
			t.Errorf("Detect(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Every recognised platform gets the exact body its own documentation asks for,
// so nothing depends on a vendor ignoring fields it does not know.
func TestEachPlatformGetsItsDocumentedBody(t *testing.T) {
	cases := []struct {
		format string
		field  string
		only   bool
	}{
		{FormatDiscord, "content", true},
		{FormatSlack, "text", true},
		{FormatGoogleChat, "text", true},
		{FormatTeams, "text", true},
		{FormatDup, "text", false},
	}
	for _, c := range cases {
		srv, got, _ := recorder(t, 200, "")
		n := New(config.Notify{URL: srv.URL, Format: c.format, Timeout: 5 * time.Second}, "web01", quietLog())
		if _, err := n.Deliver(context.Background(), snap(), false); err != nil {
			t.Fatalf("%s: %v", c.format, err)
		}
		var p map[string]any
		if err := json.Unmarshal(*got, &p); err != nil {
			t.Fatalf("%s: not json: %v", c.format, err)
		}
		if _, ok := p[c.field]; !ok {
			t.Errorf("%s: body has no %q: %s", c.format, c.field, *got)
		}
		if c.only && len(p) != 1 {
			t.Errorf("%s: sent %d fields, want only %q: %s", c.format, len(p), c.field, *got)
		}
		if !c.only && p["state"] != "succeeded" {
			t.Errorf("%s: the full payload must survive alongside text: %s", c.format, *got)
		}
	}
}

func TestEventDefaultsKeepTodaysOutcomesAndAddTheFailures(t *testing.T) {
	var n config.Notify
	on := []string{
		config.EventUpdateSucceeded, config.EventUpdateNoChange, config.EventUpdateDryRun,
		config.EventUpdateFailed, config.EventUpdateRolledBack, config.EventUpdateRollbackFailed,
		config.EventCheckFailed, config.EventCheckRecovered,
	}
	off := []string{config.EventUpdateAvailable, config.EventUpdateWithdrawn, config.EventUpdateStarted}
	for _, e := range on {
		if !n.Wants(e) {
			t.Errorf("%s should be on by default", e)
		}
	}
	for _, e := range off {
		if n.Wants(e) {
			t.Errorf("%s should be off by default", e)
		}
	}
}

// Naming one event must not silence the rest, or a config that turns on
// update_available would quietly stop reporting failures.
func TestNamingOneEventLeavesTheOthersAtTheirDefault(t *testing.T) {
	n := config.Notify{Events: map[string]bool{config.EventUpdateAvailable: true}}
	if !n.Wants(config.EventUpdateAvailable) {
		t.Error("the named event should be on")
	}
	if !n.Wants(config.EventUpdateFailed) {
		t.Error("an unnamed event should keep its default")
	}
}

func TestAnUnsubscribedOutcomeIsNotSent(t *testing.T) {
	srv, got, _ := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second,
		Events: map[string]bool{config.EventUpdateSucceeded: false}}, "web01", quietLog())

	n.Notify(context.Background(), snap())
	if len(*got) != 0 {
		t.Errorf("a muted outcome was still posted: %s", *got)
	}
}

func TestEveryPayloadNamesItsEvent(t *testing.T) {
	cases := []struct {
		name string
		call func(*Notifier)
		want string
	}{
		{"finished", func(n *Notifier) { n.Notify(context.Background(), snap()) }, config.EventUpdateSucceeded},
		{"started", func(n *Notifier) { n.NotifyStarted(context.Background(), snap()) }, config.EventUpdateStarted},
		{"available", func(n *Notifier) {
			n.Available(context.Background(), "app", []string{"web"}, time.Now().Add(30*time.Minute))
		}, config.EventUpdateAvailable},
		{"withdrawn", func(n *Notifier) { n.Withdrawn(context.Background(), "app", "gone") }, config.EventUpdateWithdrawn},
		{"check failed", func(n *Notifier) {
			n.CheckFailed(context.Background(), "app", errors.New("401 Unauthorized\nsecond line"))
		}, config.EventCheckFailed},
		{"check recovered", func(n *Notifier) { n.CheckRecovered(context.Background(), "app") }, config.EventCheckRecovered},
	}
	for _, c := range cases {
		srv, got, _ := recorder(t, 200, "")
		n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second, Events: map[string]bool{
			config.EventUpdateStarted: true, config.EventUpdateAvailable: true, config.EventUpdateWithdrawn: true,
		}}, "web01", quietLog())

		c.call(n)
		var p map[string]any
		if err := json.Unmarshal(*got, &p); err != nil {
			t.Fatalf("%s: %v (body %q)", c.name, err, *got)
		}
		if p["event"] != c.want {
			t.Errorf("%s: event = %v, want %q", c.name, p["event"], c.want)
		}
		if s, _ := p["summary"].(string); s == "" {
			t.Errorf("%s: no summary, so a chat platform would post an empty message", c.name)
		}
		if strings.Contains(fmt.Sprint(p["summary"]), "\n") {
			t.Errorf("%s: summary spans lines: %q", c.name, p["summary"])
		}
	}
}

func TestAvailableCarriesWhenItWillApply(t *testing.T) {
	srv, got, _ := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second,
		Events: map[string]bool{config.EventUpdateAvailable: true}}, "web01", quietLog())

	at := time.Now().Add(30 * time.Minute)
	n.Available(context.Background(), "app", []string{"web", "db"}, at)

	var p map[string]any
	if err := json.Unmarshal(*got, &p); err != nil {
		t.Fatal(err)
	}
	if p["applies_at"] == nil {
		t.Errorf("no applies_at, which is the only actionable part: %s", *got)
	}
	if svc, _ := p["changed_services"].([]any); len(svc) != 2 {
		t.Errorf("changed_services = %v, want both", p["changed_services"])
	}
}

// A non-job event has no job, so it must not carry empty job fields that a
// receiver would have to know to ignore.
func TestANonJobEventOmitsTheJobFields(t *testing.T) {
	srv, got, _ := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second,
		Events: map[string]bool{config.EventUpdateAvailable: true}}, "web01", quietLog())

	n.Available(context.Background(), "app", []string{"web"}, time.Now().Add(time.Hour))

	var p map[string]any
	if err := json.Unmarshal(*got, &p); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"state", "job_id", "duration_ms"} {
		if _, ok := p[absent]; ok {
			t.Errorf("%q should be omitted for a non-job event: %s", absent, *got)
		}
	}
	for _, present := range []string{"event", "target", "summary", "applies_at"} {
		if _, ok := p[present]; !ok {
			t.Errorf("%q is missing: %s", present, *got)
		}
	}
}

func TestAJobEventStillCarriesStateAndID(t *testing.T) {
	srv, got, _ := recorder(t, 200, "")
	n := New(config.Notify{URL: srv.URL, Timeout: 5 * time.Second}, "web01", quietLog())

	n.Notify(context.Background(), snap())

	var p map[string]any
	if err := json.Unmarshal(*got, &p); err != nil {
		t.Fatal(err)
	}
	if p["state"] != "succeeded" || p["job_id"] != "abc123" {
		t.Errorf("a finished job must keep state and job_id: %s", *got)
	}
}
