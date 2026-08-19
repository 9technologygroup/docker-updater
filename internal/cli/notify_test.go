package cli

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/notify"
)

func TestNotifyAdviceNamesTheDiscordMismatch(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{Notify: config.Notify{URL: "https://discord.com/api/webhooks/123/abc"}}
	printNotifyAdvice(&buf, cfg, notify.Result{Format: notify.FormatDup}, 400)

	for _, want := range []string{"discord webhook", "format: discord"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("advice does not mention %q:\n%s", want, buf.String())
		}
	}
}

func TestNotifyAdviceDoesNotBlameDiscordWhenTheFormatIsRight(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{Notify: config.Notify{URL: "https://discord.com/api/webhooks/123/abc"}}
	printNotifyAdvice(&buf, cfg, notify.Result{Format: notify.FormatDiscord}, 404)

	if strings.Contains(buf.String(), "format: discord") {
		t.Errorf("the format is already discord, so that cannot be the problem:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "404") && !strings.Contains(buf.String(), "listening") {
		t.Errorf("a 404 should be explained:\n%s", buf.String())
	}
}

func TestNotifyConfigNeverPrintsAHeaderValue(t *testing.T) {
	var buf bytes.Buffer
	cfg := &config.Config{Notify: config.Notify{
		URL: "https://n8n.example.com/webhook/dup", Timeout: 15e9,
		Headers: map[string]string{"Authorization": "Bearer super-secret-value"},
	}}
	printNotifyConfig(&buf, cfg, notify.New(cfg.Notify, "web01", quietTestLog()))

	if strings.Contains(buf.String(), "super-secret-value") {
		t.Errorf("a header value reached the terminal:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Authorization") {
		t.Errorf("the header name should still be listed:\n%s", buf.String())
	}
}

func quietTestLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
