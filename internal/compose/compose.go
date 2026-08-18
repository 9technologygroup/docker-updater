package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/9technologygroup/docker-updater/internal/config"
)

const maxOutputBytes = 64 << 10

type Runner struct {
	Bin string
}

func New(bin string) *Runner {
	if bin == "" {
		bin = "docker"
	}
	return &Runner{Bin: bin}
}

type Result struct {
	Args   []string
	Output string
	Err    error
}

type Container struct {
	Service string
	Name    string
	State   string
	Health  string
	Image   string
	ImageID string
}

func (c Container) Running() bool { return strings.EqualFold(c.State, "running") }

func (c Container) Healthy() bool {
	switch strings.ToLower(c.Health) {
	case "", "healthy":
		return true
	default:
		return false
	}
}

func (c Container) Terminal() bool {
	switch strings.ToLower(c.State) {
	case "exited", "dead", "removing":
		return true
	default:
		return false
	}
}

func (r *Runner) Verify(ctx context.Context) error {
	if _, err := exec.LookPath(r.Bin); err != nil {
		return fmt.Errorf("docker binary %q not found in PATH: %w", r.Bin, err)
	}
	res := r.exec(ctx, "", nil, "compose", "version", "--short")
	if res.Err != nil {
		return fmt.Errorf("docker compose unavailable: %w (%s)", res.Err, strings.TrimSpace(res.Output))
	}
	return nil
}

func (r *Runner) baseArgs(t *config.Target) []string {
	args := []string{"compose"}
	if t.ComposeFile != "" {
		args = append(args, "-f", t.ComposeFile)
	}
	if t.EnvFile != "" {
		args = append(args, "--env-file", t.EnvFile)
	}
	return args
}

func (r *Runner) Validate(ctx context.Context, t *config.Target, env []string) Result {
	args := append(r.baseArgs(t), "config", "--quiet")
	return r.exec(ctx, t.Dir, env, args...)
}

func (r *Runner) DesiredImages(ctx context.Context, t *config.Target, env []string) (map[string]string, Result) {
	args := append(r.baseArgs(t), "config", "--format", "json")
	res := r.exec(ctx, t.Dir, env, args...)
	if res.Err != nil {
		return nil, res
	}

	var doc struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(res.Output), &doc); err != nil {
		res.Err = fmt.Errorf("parse compose config: %w", err)
		return nil, res
	}

	images := make(map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		if svc.Image != "" {
			images[name] = svc.Image
		}
	}
	return images, res
}

func (r *Runner) Pull(ctx context.Context, t *config.Target, env []string, services []string) Result {
	args := append(r.baseArgs(t), "pull")
	args = appendServices(args, services)
	return r.exec(ctx, t.Dir, env, args...)
}

func (r *Runner) Up(ctx context.Context, t *config.Target, env []string, services []string, forceRecreate bool) Result {
	args := append(r.baseArgs(t), "up", "-d", "--no-build")
	if forceRecreate {
		args = append(args, "--force-recreate")
	}
	args = appendServices(args, services)
	return r.exec(ctx, t.Dir, env, args...)
}

func (r *Runner) Tag(ctx context.Context, imageID, ref string) Result {
	return r.exec(ctx, "", nil, "tag", "--", imageID, ref)
}

func (r *Runner) ImageExists(ctx context.Context, imageID string) bool {
	res := r.exec(ctx, "", nil, "image", "inspect", "--format", "{{.Id}}", imageID)
	return res.Err == nil
}

func (r *Runner) ImageID(ctx context.Context, ref string) (string, error) {
	res := r.exec(ctx, "", nil, "image", "inspect", "--format", "{{.Id}}", ref)
	if res.Err != nil {
		return "", fmt.Errorf("inspect image %s: %w (%s)", ref, res.Err, strings.TrimSpace(res.Output))
	}
	return strings.TrimSpace(res.Output), nil
}

func (r *Runner) Output(ctx context.Context, args ...string) (string, error) {
	res := r.exec(ctx, "", nil, args...)
	if res.Err != nil {
		return "", fmt.Errorf("%s %s: %w (%s)", r.Bin, strings.Join(args, " "), res.Err, strings.TrimSpace(res.Output))
	}
	return res.Output, nil
}

func (r *Runner) Containers(ctx context.Context, t *config.Target, env []string) ([]Container, Result) {
	args := append(r.baseArgs(t), "ps", "--all", "--format", "json")
	res := r.exec(ctx, t.Dir, env, args...)
	if res.Err != nil {
		return nil, res
	}
	containers, err := parsePS(res.Output)
	if err != nil {
		res.Err = err
		return nil, res
	}
	if err := r.attachImageIDs(ctx, containers); err != nil {
		res.Err = err
		return containers, res
	}
	return containers, res
}

func (r *Runner) attachImageIDs(ctx context.Context, containers []Container) error {
	if len(containers) == 0 {
		return nil
	}
	args := []string{"inspect", "--type", "container", "--format", "{{.Name}}\t{{.Image}}\t{{.Config.Image}}"}
	for _, c := range containers {
		args = append(args, c.Name)
	}
	res := r.exec(ctx, "", nil, args...)
	if res.Err != nil {
		return fmt.Errorf("docker inspect: %w (%s)", res.Err, strings.TrimSpace(res.Output))
	}

	byName := make(map[string][2]string)
	for _, line := range strings.Split(res.Output, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 3 {
			continue
		}
		byName[strings.TrimPrefix(parts[0], "/")] = [2]string{parts[1], parts[2]}
	}
	for i := range containers {
		v, ok := byName[containers[i].Name]
		if !ok {
			continue
		}
		containers[i].ImageID = v[0]
		if v[1] != "" {
			containers[i].Image = v[1]
		}
	}
	return nil
}

func appendServices(args, services []string) []string {
	if len(services) == 0 {
		return args
	}
	args = append(args, "--")
	return append(args, services...)
}

func (r *Runner) exec(ctx context.Context, dir string, env []string, args ...string) Result {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = childEnv(env)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.DeadlineExceeded) {
		err = fmt.Errorf("timed out: %w", ctxErr)
	}
	return Result{Args: args, Output: truncate(buf.String()), Err: err}
}

var passthroughEnv = []string{
	"PATH", "HOME", "USER", "LANG", "TZ",
	"DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// childEnv drops any inherited variable that extra also sets, rather than
// relying on a later duplicate winning: which of two entries the child sees is
// platform dependent, and DOCKER_CONFIG decides which credentials a pull uses.
func childEnv(extra []string) []string {
	overridden := make(map[string]bool, len(extra))
	for _, kv := range extra {
		if key, _, ok := strings.Cut(kv, "="); ok {
			overridden[key] = true
		}
	}

	out := make([]string, 0, len(passthroughEnv)+len(extra))
	for _, key := range passthroughEnv {
		if overridden[key] {
			continue
		}
		if v, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+v)
		}
	}
	return append(out, extra...)
}

func truncate(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	return "…[truncated]…\n" + s[len(s)-maxOutputBytes:]
}

type psEntry struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Health  string `json:"Health"`
	Image   string `json:"Image"`
}

func parsePS(out string) ([]Container, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}

	var entries []psEntry
	if strings.HasPrefix(out, "[") {
		if err := json.Unmarshal([]byte(out), &entries); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
	} else {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e psEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				return nil, fmt.Errorf("parse compose ps line: %w", err)
			}
			entries = append(entries, e)
		}
	}

	containers := make([]Container, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		health := e.Health
		if health == "" {
			health = healthFromStatus(e.Status)
		}
		containers = append(containers, Container{
			Service: e.Service,
			Name:    e.Name,
			State:   e.State,
			Health:  health,
			Image:   e.Image,
		})
	}
	return containers, nil
}

func healthFromStatus(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "(healthy)"):
		return "healthy"
	case strings.Contains(s, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(s, "health: starting"):
		return "starting"
	default:
		return ""
	}
}

func ProjectName(dir string) string { return filepath.Base(dir) }
