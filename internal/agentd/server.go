package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/PatchMon/docker-updater/internal/compose"
	"github.com/PatchMon/docker-updater/internal/config"
	"github.com/PatchMon/docker-updater/internal/discover"
	"github.com/PatchMon/docker-updater/internal/job"
	"github.com/PatchMon/docker-updater/internal/wire"
)

const (
	socketMode    = 0o660
	maxConcurrent = 3
)

type connKey struct{}

type Server struct {
	cfg        *config.Config
	pipeline   job.Backend
	checker    Checker
	docker     *compose.Runner
	log        *slog.Logger
	allowedUID uint32
	checkPeer  bool

	slots chan struct{}

	mu      sync.Mutex
	running map[string]bool
}

type Checker interface {
	Check(ctx context.Context, t *config.Target) (wire.CheckResult, error)
}

func NewServer(cfg *config.Config, backend job.Backend, log *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		pipeline: backend,
		log:      log,
		running:  make(map[string]bool),
		slots:    make(chan struct{}, maxConcurrent),
	}
	if c, ok := backend.(Checker); ok {
		s.checker = c
	}
	return s
}

func (s *Server) WithDocker(r *compose.Runner) *Server {
	s.docker = r
	return s
}

func PeerCredSupported() bool { return peerCredSupported }

func (s *Server) RequirePeerUID(uid uint32) {
	s.allowedUID = uid
	s.checkPeer = true
}

func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

func (s *Server) authorisePeer(w http.ResponseWriter, r *http.Request) bool {
	if !s.checkPeer {
		return true
	}
	conn, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		writeError(w, http.StatusForbidden, "peer identity unavailable")
		return false
	}
	uid, ok := peerUID(conn)
	if !ok {
		if !peerCredSupported {
			return true
		}
		s.log.Warn("agent could not read peer credentials, refusing the connection")
		writeError(w, http.StatusForbidden, "could not verify caller identity")
		return false
	}
	if uid != 0 && uid != s.allowedUID {
		s.log.Warn("agent rejected connection from unexpected uid", "uid", uid)
		writeError(w, http.StatusForbidden, "caller is not permitted to use the update agent")
		return false
	}
	return true
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.ExecPath, s.handleExec)
	mux.HandleFunc("POST "+wire.CheckPath, s.handleCheck)
	mux.HandleFunc("GET "+wire.DiscoverPath, s.handleDiscover)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

func Listen(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("agent socket path must be absolute")
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace %s: it exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat agent socket: %w", err)
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, socketMode); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod agent socket: %w", err)
	}
	return listener, nil
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !s.authorisePeer(w, r) {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req wire.ExecRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	target, ok := s.cfg.Target(req.Target)
	if !ok {
		s.log.Warn("agent rejected unknown target", "target", clip(req.Target))
		writeError(w, http.StatusNotFound, "unknown target")
		return
	}
	if err := validateTag(target, req.Tag); err != nil {
		s.log.Warn("agent rejected tag", "target", target.Name, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !s.acquire(target.Name) {
		writeError(w, http.StatusConflict, "an update is already running for this target")
		return
	}
	defer s.release(target.Name)

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "the update agent is already running its maximum number of concurrent updates")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sink := &streamSink{enc: json.NewEncoder(w), flusher: flusher}
	s.log.Info("agent executing update", "target", target.Name, "tag", req.Tag, "dry_run", req.DryRun, "force", req.Force)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), target.JobTimeout)
	defer cancel()

	state, message, _ := s.pipeline.Update(ctx, target, job.Request{
		Tag:    req.Tag,
		DryRun: req.DryRun,
		Force:  req.Force,
	}, sink)

	sink.emit(wire.Event{Type: wire.EventResult, State: state, Message: message})
	s.log.Info("agent finished update", "target", target.Name, "state", string(state), "message", message)
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authorisePeer(w, r) {
		return
	}
	if s.checker == nil {
		writeError(w, http.StatusNotImplemented, "this agent cannot check for updates")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req wire.CheckRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	target, ok := s.cfg.Target(req.Target)
	if !ok {
		s.log.Warn("agent rejected unknown target", "target", clip(req.Target))
		writeError(w, http.StatusNotFound, "unknown target")
		return
	}

	if !s.acquire(target.Name) {
		writeError(w, http.StatusConflict, "an update is already running for this target")
		return
	}
	defer s.release(target.Name)

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "the update agent is busy")
		return
	}

	result, err := s.checker.Check(r.Context(), target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.authorisePeer(w, r) {
		return
	}
	if s.docker == nil {
		writeError(w, http.StatusNotImplemented, "this agent cannot enumerate docker state")
		return
	}

	result := discover.Run(r.Context(), s.docker, s.cfg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) acquire(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[target] {
		return false
	}
	s.running[target] = true
	return true
}

func (s *Server) release(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, target)
}

func validateTag(t *config.Target, tag string) error {
	if tag == "" {
		return nil
	}
	if t.ImageTagEnv == "" {
		return errors.New("this target does not accept a tag")
	}
	if !config.ValidImageTag(tag) {
		return errors.New("tag is not a valid container image tag")
	}
	return nil
}

type streamSink struct {
	mu      sync.Mutex
	enc     *json.Encoder
	flusher http.Flusher
}

func (s *streamSink) emit(e wire.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(e); err != nil {
		return
	}
	s.flusher.Flush()
}

func (s *streamSink) AddStep(step job.Step) { s.emit(wire.Event{Type: wire.EventStep, Step: &step}) }

func (s *streamSink) SetBefore(states []job.ServiceState) {
	s.emit(wire.Event{Type: wire.EventBefore, States: states})
}

func (s *streamSink) SetAfter(states []job.ServiceState) {
	s.emit(wire.Event{Type: wire.EventAfter, States: states})
}

func (s *streamSink) SetChanged(services []string) {
	s.emit(wire.Event{Type: wire.EventChanged, Changed: services})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func clip(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
