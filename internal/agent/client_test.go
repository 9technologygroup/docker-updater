package agent

import (
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/job"
)

type captureSink struct {
	started  []string
	progress []job.ServiceState
	steps    []job.Step
	before   []job.ServiceState
	after    []job.ServiceState
	changed  []string
}

func (c *captureSink) StartStep(name string)            { c.started = append(c.started, name) }
func (c *captureSink) SetProgress(s []job.ServiceState) { c.progress = s }
func (c *captureSink) AddStep(s job.Step)               { c.steps = append(c.steps, s) }
func (c *captureSink) SetBefore(s []job.ServiceState)   { c.before = s }
func (c *captureSink) SetAfter(s []job.ServiceState)    { c.after = s }
func (c *captureSink) SetChanged(services []string)     { c.changed = services }

func TestClientConsumesStreamAndRequiresResult(t *testing.T) {
	c := &Client{}

	stream := strings.Join([]string{
		`{"type":"step","step":{"name":"pull","ok":true}}`,
		`{"type":"changed","changed":["app"]}`,
		`{"type":"after","states":[{"service":"app","state":"running"}]}`,
		`{"type":"result","state":"succeeded","message":"updated app"}`,
	}, "\n")

	sink := &captureSink{}
	state, message, err := c.consume(strings.NewReader(stream), sink)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if state != job.StateSucceeded || message != "updated app" {
		t.Errorf("state = %q message = %q", state, message)
	}
	if len(sink.steps) != 1 || sink.steps[0].Name != "pull" {
		t.Errorf("steps = %+v", sink.steps)
	}
	if len(sink.changed) != 1 || sink.changed[0] != "app" {
		t.Errorf("changed = %v", sink.changed)
	}
	if len(sink.after) != 1 {
		t.Errorf("after = %+v", sink.after)
	}
}

func TestClientTreatsTruncatedStreamAsFailure(t *testing.T) {
	c := &Client{}
	stream := `{"type":"step","step":{"name":"pull","ok":true}}`

	state, _, err := c.consume(strings.NewReader(stream), &captureSink{})
	if err == nil {
		t.Fatal("a stream with no result event must be an error")
	}
	if state != job.StateFailed {
		t.Errorf("state = %q, want failed", state)
	}
	if !strings.Contains(err.Error(), "mid-update") {
		t.Errorf("error = %q, want it to warn the stack may be mid-update", err)
	}
}

func TestClientTranslatesStepStartAndIgnoresUnknownEvents(t *testing.T) {
	c := &Client{}

	stream := strings.Join([]string{
		`{"type":"step_start","step":{"name":"pull","running":true}}`,
		`{"type":"step","step":{"name":"pull","ok":true}}`,
		`{"type":"step_start","step":{"name":"up","running":true}}`,
		`{"type":"something_a_newer_agent_sends","step":{"name":"noise"}}`,
		`{"type":"step","step":{"name":"up","ok":true}}`,
		`{"type":"result","state":"succeeded","message":"updated app"}`,
	}, "\n")

	sink := &captureSink{}
	state, _, err := c.consume(strings.NewReader(stream), sink)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if state != job.StateSucceeded {
		t.Errorf("state = %q, want succeeded", state)
	}
	if len(sink.started) != 2 || sink.started[0] != "pull" || sink.started[1] != "up" {
		t.Errorf("started = %v, want [pull up]", sink.started)
	}
	if len(sink.steps) != 2 {
		t.Fatalf("steps = %+v, want two", sink.steps)
	}
	for i, s := range sink.steps {
		if s.Name != sink.started[i] {
			t.Errorf("step %d named %q closes a start named %q", i, s.Name, sink.started[i])
		}
	}
}

func TestClientIgnoresAStepStartWithNoStep(t *testing.T) {
	c := &Client{}

	stream := strings.Join([]string{
		`{"type":"step_start"}`,
		`{"type":"result","state":"succeeded","message":"done"}`,
	}, "\n")

	sink := &captureSink{}
	if _, _, err := c.consume(strings.NewReader(stream), sink); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(sink.started) != 0 {
		t.Errorf("started = %v, want nothing", sink.started)
	}
}
