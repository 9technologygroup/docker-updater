package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/compose"
	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

const (
	healthPollInterval = 3 * time.Second
	failFastGrace      = 20 * time.Second
	rollbackSlack      = 5 * time.Minute
)

type Pipeline struct {
	docker *compose.Runner
}

func New(docker *compose.Runner) *Pipeline {
	return &Pipeline{docker: docker}
}

func (e *Pipeline) Update(ctx context.Context, t *config.Target, req job.Request, sink job.Sink) (job.State, string, error) {
	state, message := e.pipeline(ctx, t, sink, req)
	return state, message, nil
}

func (e *Pipeline) Check(ctx context.Context, t *config.Target) (wire.CheckResult, error) {
	var sink discardSink

	if res := e.docker.Validate(ctx, t, nil); res.Err != nil {
		return wire.CheckResult{}, fmt.Errorf("compose file validation failed: %w", res.Err)
	}

	before, res := e.docker.Containers(ctx, t, nil)
	if res.Err != nil {
		return wire.CheckResult{}, fmt.Errorf("could not inspect current containers: %w", res.Err)
	}

	desired := e.resolveImages(ctx, &sink, t, nil, before)

	pullCtx, cancel := context.WithTimeout(ctx, t.PullTimeout)
	defer cancel()
	if res := e.docker.Pull(pullCtx, t, nil, slices.Clone(t.Services)); res.Err != nil {
		return wire.CheckResult{}, fmt.Errorf("docker compose pull failed: %s", strings.TrimSpace(res.Output))
	}

	changed := e.changedServices(ctx, before, desired, t)
	if len(changed) == 0 {
		return wire.CheckResult{Message: "already running the latest images"}, nil
	}
	return wire.CheckResult{
		Available: true,
		Changed:   changed,
		Message:   "new image for " + strings.Join(changed, ", "),
	}, nil
}

type discardSink struct{}

func (discardSink) AddStep(job.Step)             {}
func (discardSink) SetBefore([]job.ServiceState) {}
func (discardSink) SetAfter([]job.ServiceState)  {}
func (discardSink) SetChanged([]string)          {}

func (e *Pipeline) pipeline(ctx context.Context, t *config.Target, sink job.Sink, req job.Request) (job.State, string) {
	env := tagEnv(t, req.Tag)

	if res := e.step(sink, "validate", func() compose.Result { return e.docker.Validate(ctx, t, env) }); res.Err != nil {
		return job.StateFailed, "compose file validation failed"
	}

	before, res := e.snapshotContainers(ctx, sink, "inspect-before", t, env)
	if res.Err != nil {
		return job.StateFailed, "could not inspect current containers: " + res.Err.Error()
	}
	sink.SetBefore(toServiceStates(before))

	updateServices := slices.Clone(t.Services)
	healthServices := healthTargets(t, before)

	if req.DryRun {
		return job.StateDryRun, dryRunMessage(t, before, updateServices, req.Tag)
	}

	desired := e.resolveImages(ctx, sink, t, env, before)

	if msg, ok := e.tagChangesMoreThanTheTag(ctx, t, req, desired); !ok {
		return job.StateFailed, msg
	}

	pullCtx, cancelPull := context.WithTimeout(ctx, t.PullTimeout)
	res = e.step(sink, "pull", func() compose.Result { return e.docker.Pull(pullCtx, t, env, updateServices) })
	cancelPull()
	if res.Err != nil {
		return job.StateFailed, "docker compose pull failed"
	}

	changed := e.changedServices(ctx, before, desired, t)
	sink.SetChanged(changed)

	if len(changed) == 0 && !req.Force && allSettled(before, healthServices) {
		sink.SetAfter(toServiceStates(before))
		return job.StateNoChange, "already running the latest images, nothing to do"
	}

	if msg, ok := e.runPreUpdate(ctx, t, sink, req, changed); !ok {
		sink.SetAfter(toServiceStates(before))
		return job.StateFailed, msg
	}

	res = e.step(sink, "up", func() compose.Result { return e.docker.Up(ctx, t, env, updateServices, false) })
	if res.Err != nil {
		return e.rollback(ctx, t, sink, before, updateServices, healthServices, "docker compose up failed")
	}

	if err := e.waitHealthy(ctx, t, sink, env, healthServices, "health"); err != nil {
		return e.rollback(ctx, t, sink, before, updateServices, healthServices, err.Error())
	}

	after, _ := e.snapshotContainers(ctx, sink, "inspect-after", t, env)
	sink.SetAfter(toServiceStates(after))

	if len(changed) == 0 {
		return job.StateSucceeded, "stack recreated, images unchanged"
	}
	return job.StateSucceeded, "updated " + strings.Join(changed, ", ")
}

func (e *Pipeline) runPreUpdate(ctx context.Context, t *config.Target, sink job.Sink, req job.Request, changed []string) (string, bool) {
	hook := t.PreUpdate
	if !hook.Configured() {
		return "", true
	}

	start := time.Now()
	hookCtx, cancel := context.WithTimeout(ctx, hook.Timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, hook.Command, hook.Args...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(),
		"DUP_STACK="+t.Name,
		"DUP_DIR="+t.Dir,
		"DUP_TAG="+req.Tag,
		"DUP_SERVICES="+strings.Join(changed, ","),
	)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	if hookCtx.Err() != nil && errors.Is(hookCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s: %w", hook.Timeout, hookCtx.Err())
	}

	step := job.Step{
		Name:       "pre-update",
		Command:    cmdString(hook.Command, hook.Args),
		StartedAt:  start.UTC(),
		DurationMS: time.Since(start).Milliseconds(),
		OK:         err == nil,
		Output:     truncateOutput(buf.String()),
	}
	if err != nil {
		step.Error = err.Error()
	}
	sink.AddStep(step)

	if err == nil {
		return "", true
	}
	if !hook.IsRequired() {
		return "", true
	}
	return "pre-update hook failed, nothing was changed: " + err.Error(), false
}

func truncateOutput(s string) string {
	const max = 32 << 10
	if len(s) <= max {
		return s
	}
	return "…[truncated]…\n" + s[len(s)-max:]
}

func (e *Pipeline) rollback(ctx context.Context, t *config.Target, sink job.Sink, before []compose.Container, updateServices, healthServices []string, cause string) (job.State, string) {
	if !t.RollbackEnabled() {
		after, _ := e.snapshotContainers(ctx, sink, "inspect-after", t, tagEnv(t, ""))
		sink.SetAfter(toServiceStates(after))
		return job.StateFailed, cause + " (rollback disabled)"
	}

	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.HealthTimeout+rollbackSlack)
	defer cancel()

	env := tagEnv(t, previousTag(t, before))
	desired := e.resolveImages(rbCtx, sink, t, env, before)

	var missing, retagFailed []string
	for _, c := range before {
		ref := imageRef(desired, c)
		if c.ImageID == "" || ref == "" {
			continue
		}
		if !e.docker.ImageExists(rbCtx, c.ImageID) {
			missing = append(missing, c.Service)
			continue
		}
		if cur, err := e.docker.ImageID(rbCtx, ref); err == nil && cur == c.ImageID {
			continue
		}
		if res := e.step(sink, "rollback-tag:"+c.Service, func() compose.Result {
			return e.docker.Tag(rbCtx, c.ImageID, ref)
		}); res.Err != nil {
			retagFailed = append(retagFailed, c.Service)
		}
	}

	res := e.step(sink, "rollback-up", func() compose.Result {
		return e.docker.Up(rbCtx, t, env, updateServices, true)
	})

	after, _ := e.snapshotContainers(rbCtx, sink, "inspect-after", t, env)
	sink.SetAfter(toServiceStates(after))

	detail := cause
	if len(missing) > 0 {
		detail += "; previous image no longer present locally for " + strings.Join(missing, ", ")
	}
	if len(retagFailed) > 0 {
		detail += "; could not restore the previous image for " + strings.Join(retagFailed, ", ") + ", so the new image may still be running"
	}
	if len(retagFailed) > 0 {
		return job.StateRollbackFailed, "update failed and the previous images could not be restored: " + detail
	}
	if res.Err != nil {
		return job.StateRollbackFailed, "update failed and rollback failed: " + detail
	}
	if err := e.waitHealthy(rbCtx, t, sink, env, healthServices, "rollback-health"); err != nil {
		return job.StateRollbackFailed, "update failed and rollback did not come healthy: " + detail
	}

	after, _ = e.snapshotContainers(rbCtx, sink, "inspect-after", t, env)
	sink.SetAfter(toServiceStates(after))
	return job.StateRolledBack, "update failed, rolled back to the previous images: " + detail
}

func (e *Pipeline) waitHealthy(ctx context.Context, t *config.Target, sink job.Sink, env, healthServices []string, stepName string) error {
	start := time.Now()
	deadline := start.Add(t.HealthTimeout)
	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	var (
		last         []compose.Container
		settledSince time.Time
	)
	for {
		containers, res := e.docker.Containers(ctx, t, env)
		if res.Err == nil {
			last = containers
			if ok, _ := settled(containers, healthServices); ok {
				if settledSince.IsZero() {
					settledSince = time.Now()
				}
				if time.Since(settledSince) >= t.StabilityWindow {
					e.record(sink, stepName, start, nil, describe(containers))
					return nil
				}
			} else {
				settledSince = time.Time{}
				if time.Since(start) > failFastGrace {
					if svc, ok := crashed(containers, healthServices); ok {
						err := fmt.Errorf("service %q is not running after update", svc)
						e.record(sink, stepName, start, err, describe(containers))
						return err
					}
				}
			}
		}

		if time.Now().After(deadline) {
			err := fmt.Errorf("timed out after %s waiting for services to become healthy", t.HealthTimeout)
			e.record(sink, stepName, start, err, describe(last))
			return err
		}

		select {
		case <-ctx.Done():
			err := fmt.Errorf("cancelled while waiting for services to become healthy: %w", ctx.Err())
			e.record(sink, stepName, start, err, describe(last))
			return err
		case <-ticker.C:
		}
	}
}

func (e *Pipeline) changedServices(ctx context.Context, before []compose.Container, desired map[string]string, t *config.Target) []string {
	var changed []string
	for _, c := range before {
		ref := imageRef(desired, c)
		if !wanted(t, c.Service) || ref == "" {
			continue
		}
		if current := taggedRef(c.Image); current != "" && current != ref {
			changed = append(changed, c.Service)
			continue
		}
		newID, err := e.docker.ImageID(ctx, ref)
		if err != nil || newID != c.ImageID {
			changed = append(changed, c.Service)
		}
	}
	return changed
}

func (e *Pipeline) resolveImages(ctx context.Context, sink job.Sink, t *config.Target, env []string, before []compose.Container) map[string]string {
	start := time.Now()
	desired, res := e.docker.DesiredImages(ctx, t, env)
	if res.Err != nil {
		desired = make(map[string]string, len(before))
		for _, c := range before {
			if ref := taggedRef(c.Image); ref != "" {
				desired[c.Service] = ref
			}
		}
	}
	e.record(sink, "resolve-images", start, res.Err, summariseImages(desired))
	return desired
}

func (e *Pipeline) tagChangesMoreThanTheTag(ctx context.Context, t *config.Target, req job.Request, desired map[string]string) (string, bool) {
	if t.ImageTagEnv == "" || req.Tag == "" {
		return "", true
	}

	base, res := e.docker.DesiredImages(ctx, t, nil)
	if res.Err != nil {
		return "refusing to apply a tag: this stack does not resolve without one, so the tag would be choosing the image itself rather than its version; pin the repository in the compose file as image: repo:${" + t.ImageTagEnv + "} instead of image: ${" + t.ImageTagEnv + "}", false
	}

	for svc, ref := range desired {
		if !wanted(t, svc) {
			continue
		}
		baseRef, ok := base[svc]
		if !ok {
			return fmt.Sprintf("refusing to apply tag %q: service %q has no image without it, so the tag would choose the image itself rather than its version", req.Tag, svc), false
		}
		if repository(baseRef) != repository(ref) {
			return fmt.Sprintf("refusing to apply tag %q: it moves service %q from repository %q to %q, which is more than a version change", req.Tag, svc, repository(baseRef), repository(ref)), false
		}
	}
	return "", true
}

func repository(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i]
	}
	return ref
}

func imageRef(desired map[string]string, c compose.Container) string {
	if ref := taggedRef(desired[c.Service]); ref != "" {
		return ref
	}
	return taggedRef(c.Image)
}

func taggedRef(ref string) string {
	if ref == "" || strings.HasPrefix(ref, "sha256:") {
		return ""
	}
	return ref
}

func summariseImages(desired map[string]string) string {
	if len(desired) == 0 {
		return "no images resolved"
	}
	services := make([]string, 0, len(desired))
	for svc := range desired {
		services = append(services, svc)
	}
	slices.Sort(services)

	var b strings.Builder
	for _, svc := range services {
		fmt.Fprintf(&b, "%s\t%s\n", svc, desired[svc])
	}
	return b.String()
}

func (e *Pipeline) snapshotContainers(ctx context.Context, sink job.Sink, name string, t *config.Target, env []string) ([]compose.Container, compose.Result) {
	start := time.Now()
	containers, res := e.docker.Containers(ctx, t, env)
	e.record(sink, name, start, res.Err, describe(containers))
	return containers, res
}

func (e *Pipeline) step(sink job.Sink, name string, fn func() compose.Result) compose.Result {
	start := time.Now()
	res := fn()

	step := job.Step{
		Name:       name,
		Command:    cmdString(e.docker.Bin, res.Args),
		StartedAt:  start.UTC(),
		DurationMS: time.Since(start).Milliseconds(),
		OK:         res.Err == nil,
		Output:     res.Output,
	}
	if res.Err != nil {
		step.Error = res.Err.Error()
	}
	sink.AddStep(step)
	return res
}

func (e *Pipeline) record(sink job.Sink, name string, start time.Time, err error, output string) {
	step := job.Step{
		Name:       name,
		StartedAt:  start.UTC(),
		DurationMS: time.Since(start).Milliseconds(),
		OK:         err == nil,
		Output:     output,
	}
	if err != nil {
		step.Error = err.Error()
	}
	sink.AddStep(step)
}

func wanted(t *config.Target, service string) bool {
	return len(t.Services) == 0 || slices.Contains(t.Services, service)
}

func healthTargets(t *config.Target, before []compose.Container) []string {
	if len(t.Services) > 0 {
		return slices.Clone(t.Services)
	}
	var out []string
	for _, c := range before {
		if c.Running() {
			out = append(out, c.Service)
		}
	}
	return out
}

func settled(containers []compose.Container, services []string) (bool, string) {
	if len(services) == 0 {
		for _, c := range containers {
			if c.Running() && c.Healthy() {
				return true, ""
			}
		}
		return false, "no container is running"
	}

	byService := indexByService(containers)
	for _, s := range services {
		c, ok := byService[s]
		if !ok {
			return false, s + " has no container"
		}
		if !c.Running() {
			return false, s + " is " + c.State
		}
		if !c.Healthy() {
			return false, s + " is " + c.Health
		}
	}
	return true, ""
}

func allSettled(containers []compose.Container, services []string) bool {
	ok, _ := settled(containers, services)
	return ok
}

func crashed(containers []compose.Container, services []string) (string, bool) {
	byService := indexByService(containers)
	for _, s := range services {
		c, ok := byService[s]
		if !ok {
			return s, true
		}
		if c.Terminal() {
			return s, true
		}
	}
	return "", false
}

func indexByService(containers []compose.Container) map[string]compose.Container {
	m := make(map[string]compose.Container, len(containers))
	for _, c := range containers {
		m[c.Service] = c
	}
	return m
}

func toServiceStates(containers []compose.Container) []job.ServiceState {
	out := make([]job.ServiceState, 0, len(containers))
	for _, c := range containers {
		out = append(out, job.ServiceState{
			Service:   c.Service,
			Container: c.Name,
			Image:     c.Image,
			ImageID:   c.ImageID,
			State:     c.State,
			Health:    c.Health,
		})
	}
	return out
}

func describe(containers []compose.Container) string {
	if len(containers) == 0 {
		return "no containers"
	}
	var b strings.Builder
	for _, c := range containers {
		health := c.Health
		if health == "" {
			health = "-"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", c.Service, c.State, health, c.Image)
	}
	return b.String()
}

func tagEnv(t *config.Target, tag string) []string {
	if t.ImageTagEnv == "" || tag == "" {
		return nil
	}
	return []string{t.ImageTagEnv + "=" + tag}
}

func previousTag(t *config.Target, before []compose.Container) string {
	if t.ImageTagEnv == "" {
		return ""
	}
	for _, c := range before {
		if !wanted(t, c.Service) {
			continue
		}
		if tag := refTag(c.Image); tag != "" {
			return tag
		}
	}
	return ""
}

func refTag(ref string) string {
	if ref == "" || strings.Contains(ref, "@") || strings.HasPrefix(ref, "sha256:") {
		return ""
	}
	name := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return ""
	}
	tag := name[i+1:]
	if !config.ValidImageTag(tag) {
		return ""
	}
	return tag
}

func dryRunMessage(t *config.Target, before []compose.Container, updateServices []string, tag string) string {
	scope := "the whole stack"
	if len(updateServices) > 0 {
		scope = strings.Join(updateServices, ", ")
	}
	msg := fmt.Sprintf("dry run: would pull and recreate %s in %s (%d container(s) running)", scope, t.Dir, len(before))
	if tag != "" && t.ImageTagEnv != "" {
		msg += fmt.Sprintf(" with %s=%s", t.ImageTagEnv, tag)
	}
	return msg
}

func cmdString(bin string, args []string) string {
	if len(args) == 0 {
		return ""
	}
	return bin + " " + strings.Join(args, " ")
}
