package notify

import (
	"bytes"
	"context"
	"encoding/json"
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
