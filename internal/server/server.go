package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
	"github.com/9technologygroup/docker-updater/internal/selfupdate"
)

const (
	// MaxWait is how long the API will hold an update request open. The CLI
	// clamps --wait to it so a longer wait is refused up front rather than
	// silently shortened here.
	MaxWait = 5 * time.Minute
	// MaxJobLimit bounds ?limit on the job listing.
	MaxJobLimit = 200

	defaultWait = 0
)

type Server struct {
	cfg      *config.Config
	exec     *job.Manager
	log      *slog.Logger
	host     string
	version  string
	commit   string
	throttle authThrottle

	updateStatus func() (selfupdate.Status, bool)
	pending      func(target string) (time.Time, []string, bool)
	timing       func(target string) (last, next time.Time)
}

// WithBuild records the commit reported by GET /v1/version.
func (s *Server) WithBuild(commit string) *Server {
	s.commit = commit
	return s
}

// WithUpdateStatus supplies the newest known release. It must read a cache and
// never make a network call, so an authenticated caller cannot make dup poll
// GitHub on demand.
func (s *Server) WithUpdateStatus(fn func() (selfupdate.Status, bool)) *Server {
	s.updateStatus = fn
	return s
}

// WithTiming supplies when a target was last checked for a new image and when it
// will be checked next.
func (s *Server) WithTiming(fn func(target string) (last, next time.Time)) *Server {
	s.timing = fn
	return s
}

// WithPending supplies the soak state for a target: when the update was first
// seen, what changed, and whether anything is waiting at all.
func (s *Server) WithPending(fn func(target string) (time.Time, []string, bool)) *Server {
	s.pending = fn
	return s
}

func New(cfg *config.Config, exec *job.Manager, log *slog.Logger, host, version string) *Server {
	return &Server{cfg: cfg, exec: exec, log: log, host: host, version: version}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /v1/targets", s.protected(s.handleListTargets))
	mux.Handle("POST /v1/targets/{target}/update", s.protected(s.handleUpdate))
	mux.Handle("GET /v1/targets/{target}/status", s.protected(s.handleTargetStatus))
	mux.Handle("GET /v1/jobs", s.protected(s.handleListJobs))
	mux.Handle("GET /v1/version", s.protected(s.handleVersion))
	mux.Handle("GET /v1/jobs/{id}", s.protected(s.handleGetJob))
	return s.recoverer(s.accessLog(s.restrictSource(s.cors(mux))))
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, ctx requestContext)

type requestContext struct {
	Trigger string
	Body    []byte
}

func (s *Server) protected(next handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.throttle.allow(s.throttleKey(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many failed authentication attempts, try again shortly")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, config.MaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}

		trigger, ok := s.authenticate(r, body)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dup"`)
			writeError(w, http.StatusUnauthorized, "unauthorised")
			return
		}
		next(w, r, requestContext{Trigger: trigger, Body: body})
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request, _ requestContext) {
	out := map[string]any{"current": s.version}
	if s.commit != "" {
		out["commit"] = s.commit
	}
	if s.updateStatus != nil {
		if st, ok := s.updateStatus(); ok && st.Latest.Tag != "" {
			out["latest"] = st.Latest.Tag
			out["update_available"] = st.Newer
			if !st.Latest.PublishedAt.IsZero() {
				out["latest_released"] = st.Latest.PublishedAt
			}
			if !st.CheckedAt.IsZero() {
				out["checked_at"] = st.CheckedAt
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListTargets(w http.ResponseWriter, _ *http.Request, _ requestContext) {
	type targetView struct {
		Name           string     `json:"name"`
		Dir            string     `json:"dir"`
		Services       []string   `json:"services,omitempty"`
		ImageTagEnv    string     `json:"image_tag_env,omitempty"`
		Rollback       bool       `json:"rollback"`
		HealthTimeout  string     `json:"health_timeout"`
		Busy           bool       `json:"busy"`
		RunningJob     string     `json:"running_job,omitempty"`
		PendingSince   *time.Time `json:"pending_since,omitempty"`
		PendingApplies *time.Time `json:"pending_applies_at,omitempty"`
		PendingChanged []string   `json:"pending_changed,omitempty"`
		AutoUpdate     bool       `json:"auto_update"`
		LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
		NextCheckAt    *time.Time `json:"next_check_at,omitempty"`
	}

	views := make([]targetView, 0, len(s.cfg.Targets))
	for _, t := range s.cfg.Targets {
		v := targetView{
			Name:          t.Name,
			Dir:           t.Dir,
			Services:      t.Services,
			ImageTagEnv:   t.ImageTagEnv,
			Rollback:      t.RollbackEnabled(),
			HealthTimeout: t.HealthTimeout.String(),
		}
		if running, busy := s.exec.Store().Running(t.Name); busy {
			v.Busy = true
			v.RunningJob = running.ID()
		}
		v.AutoUpdate = t.AutoUpdate
		if s.timing != nil {
			if last, next := s.timing(t.Name); !last.IsZero() || !next.IsZero() {
				if !last.IsZero() {
					v.LastCheckedAt = &last
				}
				if !next.IsZero() {
					v.NextCheckAt = &next
				}
			}
		}
		if s.pending != nil {
			if since, changed, ok := s.pending(t.Name); ok {
				applies := since.Add(t.SoakWindow())
				v.PendingSince = &since
				v.PendingApplies = &applies
				v.PendingChanged = changed
			}
		}
		views = append(views, v)
	}
	// The scheduler's clock is the one that decides when anything happens, so it
	// is reported rather than left for the caller to assume.
	writeJSON(w, http.StatusOK, map[string]any{"targets": views, "now": time.Now()})
}

func (s *Server) handleTargetStatus(w http.ResponseWriter, r *http.Request, _ requestContext) {
	name := r.PathValue("target")
	if _, ok := s.cfg.Target(name); !ok {
		writeError(w, http.StatusNotFound, "unknown target")
		return
	}

	resp := map[string]any{"target": name, "busy": false}
	if running, busy := s.exec.Store().Running(name); busy {
		resp["busy"] = true
		resp["running_job"] = running.Snapshot()
	}
	if history := s.exec.Store().List(name, 10); len(history) > 0 {
		resp["recent"] = history
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request, _ requestContext) {
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= MaxJobLimit {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": s.exec.Store().List(r.URL.Query().Get("target"), limit),
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request, _ requestContext) {
	j, ok := s.exec.Store().Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	writeJSON(w, http.StatusOK, j.Snapshot())
}

type updateRequest struct {
	Tag    string `json:"tag"`
	Reason string `json:"reason"`
	DryRun bool   `json:"dry_run"`
	Force  bool   `json:"force"`
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, rc requestContext) {
	name := r.PathValue("target")
	target, ok := s.cfg.Target(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown target")
		return
	}

	req, ignored, err := s.buildRequest(target, r, rc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ignored != "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": ignored})
		return
	}

	j, err := s.exec.Start(target, req)
	if errors.Is(err, job.ErrBusy) {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":       job.ErrBusy.Error(),
			"running_job": j.Snapshot(),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.log.Info("update accepted",
		"job", j.ID(), "target", target.Name, "trigger", req.Trigger,
		"tag", req.Tag, "dry_run", req.DryRun, "force", req.Force)

	wait := parseWait(r.URL.Query().Get("wait"))
	if wait <= 0 {
		writeJSON(w, http.StatusAccepted, j.Snapshot())
		return
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.Done():
		snap := j.Snapshot()
		status := http.StatusOK
		if !snap.State.OK() {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, snap)
	case <-timer.C:
		writeJSON(w, http.StatusAccepted, j.Snapshot())
	case <-r.Context().Done():
	}
}

func (s *Server) buildRequest(t *config.Target, r *http.Request, rc requestContext) (job.Request, string, error) {
	req := job.Request{Trigger: rc.Trigger}

	if event := r.Header.Get("X-GitHub-Event"); event != "" {
		return s.githubRequest(t, event, rc.Body)
	}

	if len(strings.TrimSpace(string(rc.Body))) > 0 {
		var body updateRequest
		dec := json.NewDecoder(bytes.NewReader(rc.Body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			return req, "", errors.New("invalid JSON body: " + err.Error())
		}
		req.Tag = strings.TrimSpace(body.Tag)
		req.Reason = body.Reason
		req.DryRun = body.DryRun
		req.Force = body.Force
	}

	if err := validateTag(t, req.Tag); err != nil {
		return req, "", err
	}
	return req, "", nil
}

type githubEvent struct {
	Action  string `json:"action"`
	Release struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	} `json:"release"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (s *Server) githubRequest(t *config.Target, event string, body []byte) (job.Request, string, error) {
	req := job.Request{Trigger: "github"}

	switch event {
	case "ping":
		return req, "ping event", nil
	case "release":
	default:
		return req, "unsupported event: " + event, nil
	}

	var ev githubEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return req, "", errors.New("invalid GitHub payload: " + err.Error())
	}
	if ev.Action != "published" {
		return req, "release action is " + ev.Action, nil
	}
	if ev.Release.Draft {
		return req, "release is a draft", nil
	}
	if ev.Release.Prerelease && !t.AllowPrerelease {
		return req, "release is a pre-release and allow_prerelease is off", nil
	}

	tag := strings.TrimPrefix(strings.TrimSpace(ev.Release.TagName), "v")
	if t.ImageTagEnv != "" {
		if !config.ValidImageTag(tag) {
			return req, "", errors.New("release tag is not a usable image tag")
		}
		req.Tag = tag
	}
	req.Reason = "GitHub release " + ev.Release.TagName + " (" + ev.Repository.FullName + ")"
	return req, "", nil
}

func validateTag(t *config.Target, tag string) error {
	if tag == "" {
		return nil
	}
	if t.ImageTagEnv == "" {
		return errors.New("this target does not accept a tag; set image_tag_env in its config first")
	}
	if !config.ValidImageTag(tag) {
		return errors.New("tag contains characters that are not allowed in a container image tag")
	}
	return nil
}

func parseWait(v string) time.Duration {
	if v == "" {
		return defaultWait
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if n, convErr := strconv.Atoi(v); convErr == nil {
			d = time.Duration(n) * time.Second
		} else {
			return defaultWait
		}
	}
	if d < 0 {
		return defaultWait
	}
	if d > MaxWait {
		return MaxWait
	}
	return d
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.written = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
