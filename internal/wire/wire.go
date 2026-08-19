package wire

import (
	"time"

	"github.com/9technologygroup/docker-updater/internal/job"
)

const (
	ExecPath     = "/v1/exec"
	CheckPath    = "/v1/check"
	DiscoverPath = "/v1/discover"
	ImagesPath   = "/v1/images"
	HealthPath   = "/healthz"

	MaxBodyBytes = 64 << 10

	EventStep      = "step"
	EventStepStart = "step_start"
	EventBefore    = "before"
	EventAfter     = "after"
	EventChanged   = "changed"
	EventResult    = "result"
)

type HealthResult struct {
	Status            string    `json:"status"`
	ConfigFingerprint string    `json:"config_fingerprint,omitempty"`
	ConfigLoadedAt    time.Time `json:"config_loaded_at,omitempty"`
	Targets           []string  `json:"targets,omitempty"`
}

type ExecRequest struct {
	Target string `json:"target"`
	Tag    string `json:"tag,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

type CheckRequest struct {
	Target string `json:"target"`
}

type ImagesRequest struct {
	Target string `json:"target"`
}

type ImagesResult struct {
	Images     map[string]string `json:"images,omitempty"`
	Registries []string          `json:"registries,omitempty"`
}

type CheckResult struct {
	Available bool     `json:"available"`
	Changed   []string `json:"changed,omitempty"`
	Message   string   `json:"message,omitempty"`
}

type Event struct {
	Type    string             `json:"type"`
	Step    *job.Step          `json:"step,omitempty"`
	States  []job.ServiceState `json:"states,omitempty"`
	Changed []string           `json:"changed,omitempty"`
	State   job.State          `json:"state,omitempty"`
	Message string             `json:"message,omitempty"`
}

type Project struct {
	Name        string   `json:"name"`
	Status      string   `json:"status,omitempty"`
	ConfigFiles []string `json:"config_files,omitempty"`
	Dir         string   `json:"dir,omitempty"`
	Target      string   `json:"target,omitempty"`
}

type Container struct {
	Name    string `json:"name"`
	Image   string `json:"image,omitempty"`
	State   string `json:"state,omitempty"`
	Project string `json:"project,omitempty"`
}

type DiscoverResult struct {
	Projects []Project   `json:"projects"`
	Loose    []Container `json:"loose_containers"`
	Warning  string      `json:"warning,omitempty"`
}

func (r DiscoverResult) Uncovered() []Project {
	var out []Project
	for _, p := range r.Projects {
		if p.Target == "" {
			out = append(out, p)
		}
	}
	return out
}
