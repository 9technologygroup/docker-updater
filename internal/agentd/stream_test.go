package agentd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/wire"
)

func TestExecStreamAnnouncesAStepBeforeItFinishes(t *testing.T) {
	cfg := newTestConfig(t, shortSocket(t))
	sock := startAgent(t, cfg)

	status, body := post(t, sock, `{"target":"smoke"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, body)
	}

	var events []wire.Event
	dec := json.NewDecoder(strings.NewReader(body))
	for {
		var ev wire.Event
		if err := dec.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
	}

	startedAt, finishedAt := -1, -1
	for i, ev := range events {
		if ev.Step == nil || ev.Step.Name != "validate" {
			continue
		}
		switch ev.Type {
		case wire.EventStepStart:
			if startedAt < 0 {
				startedAt = i
			}
			if !ev.Step.Running {
				t.Error("a step_start must mark the step as running")
			}
		case wire.EventStep:
			if finishedAt < 0 {
				finishedAt = i
			}
			if ev.Step.Running {
				t.Error("a completed step must not still be marked running")
			}
		}
	}

	if startedAt < 0 {
		t.Fatalf("no step_start for validate in %s", body)
	}
	if finishedAt < 0 {
		t.Fatalf("no step for validate in %s", body)
	}
	if startedAt > finishedAt {
		t.Errorf("step_start arrived at %d, after the completed step at %d", startedAt, finishedAt)
	}
}
