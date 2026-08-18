package agent

import (
	"strings"
	"testing"

	"github.com/9technologygroup/docker-updater/internal/job"
)

type captureSink struct {
	steps   []job.Step
	before  []job.ServiceState
	after   []job.ServiceState
	changed []string
}

func (c *captureSink) AddStep(s job.Step)             { c.steps = append(c.steps, s) }
func (c *captureSink) SetBefore(s []job.ServiceState) { c.before = s }
func (c *captureSink) SetAfter(s []job.ServiceState)  { c.after = s }
func (c *captureSink) SetChanged(services []string)   { c.changed = services }

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
