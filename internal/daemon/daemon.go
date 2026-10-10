// Package daemon is Shepherd's long-running process. It owns the store and serves the
// HTTP API; nothing else touches the state.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/desk"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/version"
)

// Server serves the API over a store.
type Server struct {
	Store store.Store
	// Config is the config the daemon started with; read LiveConfig, which a reload
	// (SIGHUP) replaces.
	Config     config.Config
	ConfigPath string
	Token      string
	Log        *slog.Logger
	Fold       *fold.Fold
	Runner     *dispatch.Runner
	// Level is the level Log writes at, when it was opened with this one; a reload sets
	// it from daemon.log_level.
	Level     *slog.LevelVar
	started   time.Time
	stop      chan struct{}
	stopOnce  sync.Once
	sessMu    sync.Mutex
	sessLocks map[string]*sync.Mutex
	live      atomic.Pointer[config.Config]
	reloadMu  sync.Mutex
	// DeskAgent runs the front desk's turns; nil is Claude Code. DeskGrace is how long
	// the desk still wakes after its last client leaves; 0 is the default.
	DeskAgent desk.Agent
	DeskGrace time.Duration
	// What runs in the background (see background): the front desk, the feed watcher,
	// and the streams following them.
	bgOnce, closeOnce        sync.Once
	quit, bgDone             chan struct{}
	desks                    *desk.Manager
	hub                      *feedHub
	deskStreams, feedStreams streamSlots
	// tokens are the scoped tokens minted since start; addr is where the API listens.
	tokens tokens
	addr   atomic.Pointer[string]
	// web holds browser sign-in codes and sessions (web.go).
	web webAuth
}

// NewServer returns a Server with a fresh random token.
func NewServer(st store.Store, cfg config.Config, cfgPath string, log *slog.Logger) (*Server, error) {
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	return &Server{
		Store: st, Config: cfg, ConfigPath: cfgPath, Token: tok, Log: log,
		Fold:    &fold.Fold{Store: st, Config: cfg, ForgeFor: forge.For},
		started: time.Now().UTC(), stop: make(chan struct{}),
	}, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LogLevelEnv names the environment variable that overrides daemon.log_level: debug,
// info, warn or error.
const LogLevelEnv = "SHEPHERD_LOG_LEVEL"

// LogLevel is the level the daemon logs at: LogLevelEnv's when it names one, else
// configured (daemon.log_level), else info.
func LogLevel(configured string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv(LogLevelEnv))); err == nil {
		return l
	}
	if err := l.UnmarshalText([]byte(configured)); err == nil {
		return l
	}
	return slog.LevelInfo
}

// NewLogger returns a logger whose output is redacted before it is written, at the level
// LogLevelEnv sets, else info.
func NewLogger() *slog.Logger {
	return newLogger(os.Stderr, LogLevel(""))
}

// Handler is the API, behind token or browser-session authentication, and the web UI.
// Each route says which scoped roles may call it (see auth.go); the operator token may
// call every one. Every request must name a loopback host (web.go).
func (s *Server) Handler() http.Handler {
	s.background()
	mux := http.NewServeMux()
	handle := func(pattern string, a access, h http.HandlerFunc) { mux.HandleFunc(pattern, guard(a, h)) }
	const desk, worker, own, web = forDesk, forWorker, ownRun, forWeb
	handle("GET "+api.PathStatus, desk|worker, s.status)
	handle("GET "+api.PathWorkspaces, desk, s.listWorkspaces)
	handle("POST "+api.PathWorkspaces, operatorOnly, s.saveWorkspace)
	handle("GET "+api.PathResolve, desk|worker, s.resolve)
	handle("POST "+api.PathShutdown, operatorOnly, s.shutdown)
	handle("GET "+api.PathLanes, desk, s.listLanes)
	handle("POST "+api.PathLanes, desk, s.openLane)
	handle("POST "+api.PathLanes+"/{id}/close", desk, s.closeLane)
	handle("POST "+api.PathLanes+"/{id}/scope", operatorOnly, s.rescopeLane)
	handle("POST "+api.PathLanes+"/{id}/ship", desk, s.withRunner(s.shipLane))
	handle("GET "+api.PathFoldGC, desk, s.foldGC)
	handle("POST "+api.PathPrePush, worker, s.prePush)
	handle("POST /v1/repos/{id}/hook", operatorOnly, s.repoHook)
	handle("POST "+api.PathRuns, desk, s.withRunner(s.startRun))
	handle("GET "+api.PathRuns, desk, s.listRuns)
	handle("GET "+api.PathRuns+"/{id}", desk|own, s.getRun)
	handle("GET "+api.PathRuns+"/{id}/log", desk|own, s.runLog)
	handle("POST "+api.PathRuns+"/{id}/stop", desk, s.withRunner(s.stopRun))
	handle("POST "+api.PathRuns+"/{id}/events", own, s.withRunner(s.addEvent))
	handle("GET "+api.PathRuns+"/{id}/events", desk|own, s.runEvents)
	handle("POST "+api.PathRuns+"/{id}/decisions", own, s.withRunner(s.addDecision))
	handle("POST "+api.PathRuns+"/{id}/requests", own, s.withRunner(s.addRequest))
	handle("GET "+api.PathDecisions, desk, s.listDecisions)
	// The desk may answer only in a turn the person started: answerDecision checks.
	handle("POST "+api.PathDecisions+"/{id}/answer", desk, s.personsAnswer(s.withRunner(s.answerDecision)))
	handle("GET "+api.PathFeed, desk, s.feed)
	handle("GET "+api.PathFeedStream, desk, s.feedStream)
	handle("POST "+api.PathFoldImport, operatorOnly, s.foldImport)
	handle("POST "+api.PathFoldView, operatorOnly, s.foldView)
	handle("POST "+api.PathFoldReview, desk, s.foldReview)
	handle("POST "+api.PathFoldRetire, operatorOnly, s.foldRetire)
	handle("POST "+api.PathSessionsImport, operatorOnly, s.importSessions)
	handle("GET "+api.PathSessions, desk, s.listSessions)
	handle("POST "+api.PathSessions+"/{id}/ask", desk, s.askSession)
	handle("GET "+api.PathTags, desk, s.listTags)
	// A worker reserves and releases for its own lane only: the handlers check.
	handle("POST "+api.PathTagsReserve, desk|worker, s.reserveTag)
	handle("POST "+api.PathTagsRelease, worker, s.releaseTag)
	handle("GET "+api.PathSpend, desk, s.spendToday)
	handle("POST "+api.PathSpend, operatorOnly, s.withRunner(s.addSpend))
	handle("GET "+api.PathSettings+"/{key}", operatorOnly, s.getSetting)
	handle("PUT "+api.PathSettings+"/{key}", operatorOnly, s.putSetting)
	handle("GET "+api.PathRequests, desk, s.listRequests)
	handle("POST "+api.PathRequests+"/{id}/route", desk, s.withRunner(s.routeRequest))
	handle("POST "+api.PathRequests+"/{id}/close", desk, s.withRunner(s.closeRequest))
	// The daemon's front desk: the person's alone, in the terminal or a browser
	// (api/desk.go).
	handle("POST "+api.PathDeskTurn, web, s.deskTurn)
	handle("GET "+api.PathDeskStream, web, s.deskStream)
	handle("GET "+api.PathDeskHistory, web, s.deskHistory)
	handle("GET "+api.PathDeskStatus, web, s.deskStatus)
	handle("POST "+api.PathDeskInterrupt, web, s.deskInterrupt)
	handle("POST "+api.PathDeskNew, web, s.deskNew)
	// Browser sign-in and sessions (api/web.go, web.go). The link's exchange, a GET with
	// no token, is browserGate's.
	handle("POST "+api.PathWebSignin, operatorOnly, s.webSignin)
	handle("GET "+api.PathWebSession, web, s.webSessionInfo)
	handle("POST "+api.PathWebSignout, web, s.webSignout)
	handle("DELETE "+api.PathWebSessions, operatorOnly, s.webEndAll)
	// Anything else under /v1 is an API 404 in JSON, never the UI's HTML.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Errorf("no endpoint %s %s", r.Method, r.URL.Path))
	})
	return s.logRequests(s.browserGate(s.auth(mux)))
}

// withRunner refuses a request that needs agents when this server has no Runner (a
// server built for tests, or embedded without dispatch), rather than panicking.
func (s *Server) withRunner(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Runner == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("this daemon does not run agents"))
			return
		}
		h(w, r)
	}
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
	// role is the caller's role, once auth knows it.
	role string
}

// Flush lets a streaming handler flush through the writer.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// slowRequest is how long a request may take before it is logged as a warning.
const slowRequest = 2 * time.Second

// requestLevel is how loudly a finished request is logged: a server error is an error,
// a client error or a slow request a warning, a successful change (POST, PUT, DELETE)
// info, and a successful read debug, since a client polling the feed makes several a
// second.
func requestLevel(method string, code int, took time.Duration) slog.Level {
	switch {
	case code >= 500:
		return slog.LevelError
	case code >= 400 || took > slowRequest:
		return slog.LevelWarn
	case method == http.MethodGet || method == http.MethodHead:
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// logRequests logs every request but the status probe each command makes first, so the
// daemon's log shows what each client asked and how it went. See requestLevel for what
// reaches the log at the default level.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if r.Method == http.MethodGet && r.URL.Path == api.PathStatus {
			return
		}
		took := time.Since(start)
		s.Log.Log(r.Context(), requestLevel(r.Method, sw.code, took), "request", "client", clientName(r),
			"method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", took.Milliseconds(), "role", sw.role)
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wss, err := s.Store.Workspaces(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.Status{
		Version: version.Version, PID: os.Getpid(), Started: s.started,
		Store: s.Store.Driver(), Config: s.ConfigPath, Workspaces: []api.WorkspaceSummary{},
	}
	for _, ws := range wss {
		repos, err := s.Store.Repos(ctx, ws.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		sum := api.WorkspaceSummary{Workspace: ws, Repos: len(repos)}
		for _, rp := range repos {
			lanes, err := s.Store.Lanes(ctx, rp.ID)
			if err != nil {
				s.fail(w, err)
				return
			}
			sum.Lanes += len(lanes)
		}
		out.Workspaces = append(out.Workspaces, sum)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	wss, err := s.Store.Workspaces(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.WorkspaceDetail{}
	for _, ws := range wss {
		repos, err := s.Store.Repos(r.Context(), ws.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, api.WorkspaceDetail{Workspace: ws, Repos: repos})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) saveWorkspace(w http.ResponseWriter, r *http.Request) {
	var req api.SaveWorkspace
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateSave(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ws, err := s.Store.SaveWorkspace(r.Context(), store.Workspace{Name: req.Name, Path: req.Path}, req.Repos)
	if err != nil {
		s.fail(w, err)
		return
	}
	repos, err := s.Store.Repos(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("workspace saved", "name", ws.Name, "path", ws.Path, "repos", len(repos))
	writeJSON(w, http.StatusOK, api.WorkspaceDetail{Workspace: ws, Repos: repos})
}

// validateSave checks a SaveWorkspace and canonicalises its paths. Every repo must sit
// inside the workspace, and its name must be its path relative to the workspace.
func validateSave(req *api.SaveWorkspace) error {
	if req.Name == "" || strings.ContainsAny(req.Name, `/\`) {
		return fmt.Errorf("workspace name %q: must be non-empty with no slashes", req.Name)
	}
	if !filepath.IsAbs(req.Path) {
		return fmt.Errorf("workspace path %q: must be absolute", req.Path)
	}
	root, err := paths.Canonical(req.Path)
	if err != nil {
		return err
	}
	req.Path = root
	for i := range req.Repos {
		rp := &req.Repos[i]
		if !filepath.IsAbs(rp.Path) {
			return fmt.Errorf("repo path %q: must be absolute", rp.Path)
		}
		if rp.Path, err = paths.Canonical(rp.Path); err != nil {
			return err
		}
		if rp.Path == root || !paths.Within(root, rp.Path) {
			return fmt.Errorf("repo %q is not inside workspace %s", rp.Path, root)
		}
		rel, _ := filepath.Rel(root, rp.Path)
		if rp.Name != filepath.ToSlash(rel) {
			return fmt.Errorf("repo name %q: must be its path in the workspace, %q", rp.Name, filepath.ToSlash(rel))
		}
	}
	return nil
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path %q: must be absolute", p))
		return
	}
	res, err := Resolve(r.Context(), s.Store, s.LiveConfig(), p)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Resolve says which workspace, repo and lane a path belongs to, the way git finds
// .git: the deepest match wins at each level.
func Resolve(ctx context.Context, st store.Store, cfg config.Config, p string) (api.Resolution, error) {
	p, err := paths.Canonical(p)
	if err != nil {
		return api.Resolution{}, err
	}
	res := api.Resolution{Path: p}
	wss, err := st.Workspaces(ctx)
	if err != nil {
		return res, err
	}
	for i := range wss {
		ws := wss[i]
		if paths.Within(ws.Path, p) && (res.Workspace == nil || len(ws.Path) > len(res.Workspace.Path)) {
			res.Workspace = &ws
		}
	}
	if res.Workspace == nil {
		return res, nil
	}
	repos, err := st.Repos(ctx, res.Workspace.ID)
	if err != nil {
		return res, err
	}
	// A lane's worktree usually sits outside its repo, so check lanes of every repo.
	for i := range repos {
		rp := repos[i]
		lanes, err := st.Lanes(ctx, rp.ID)
		if err != nil {
			return res, err
		}
		for j := range lanes {
			l := lanes[j]
			if l.Worktree != "" && paths.Within(l.Worktree, p) && (res.Lane == nil || len(l.Worktree) > len(res.Lane.Worktree)) {
				res.Lane, res.Repo = &l, &rp
			}
		}
	}
	if res.Lane == nil {
		for i := range repos {
			rp := repos[i]
			if paths.Within(rp.Path, p) && (res.Repo == nil || len(rp.Path) > len(res.Repo.Path)) {
				res.Repo = &rp
			}
		}
	}
	if res.Repo != nil {
		prof := cfg.Profile(res.Repo.Name)
		res.Profile = &prof
	}
	return res, nil
}

func (s *Server) listLanes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wsID, err1 := strconv.ParseInt(r.URL.Query().Get("workspace_id"), 10, 64)
	if err1 != nil {
		writeError(w, http.StatusBadRequest, errors.New("workspace_id is required"))
		return
	}
	repoID, _ := strconv.ParseInt(r.URL.Query().Get("repo_id"), 10, 64)
	repos, err := s.Store.Repos(ctx, wsID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.LaneView{}
	for _, rp := range repos {
		if repoID != 0 && rp.ID != repoID {
			continue
		}
		lanes, err := s.Store.Lanes(ctx, rp.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, l := range lanes {
			v := api.LaneView{Lane: l, Repo: rp.Name}
			// The store holds the last the forge said, so an unreachable forge keeps it.
			if lf, err := s.Store.LaneForge(ctx, l.ID); err == nil && lf.MR != 0 {
				v.MR, v.MRURL, v.MRState = lf.MR, lf.MRURL, api.MRState(lf.MRState)
				v.Pipeline, v.PipelineStatus = lf.Pipeline, api.PipelineStatus(lf.PipelineStatus)
			}
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) openLane(w http.ResponseWriter, r *http.Request) {
	var req fold.OpenRequest
	if !decode(w, r, &req) {
		return
	}
	req.Origin = s.fillOrigin(r, req.Origin)
	lane, err := s.Fold.Open(r.Context(), req)
	if err != nil {
		s.foldError(w, err)
		return
	}
	s.Log.Info("lane opened", "lane", lane.Name, "repo_id", lane.RepoID, "worktree", lane.Worktree, "origin", lane.Origin.String())
	s.Store.AddFeed(r.Context(), store.FeedLaneOpened, fmt.Sprintf("lane %s opened via %s (scope %s)", lane.Name, lane.Origin.Surface(), strings.Join(lane.Scope, ", ")), lane.ID)
	s.refreshView(lane.RepoID)
	writeJSON(w, http.StatusOK, lane)
}

// fillOrigin completes what a client said about itself with what the daemon knows: the
// client its request named, the agent and session of the run it came from, and the
// front desk's session (the desk is resumed per turn, so it is the one in use). It never
// guesses: what nothing says stays empty.
func (s *Server) fillOrigin(r *http.Request, o store.Origin) store.Origin {
	if o.Via == "" {
		o.Via = r.Header.Get(api.ClientHeader)
	}
	ctx := r.Context()
	if o.Run != 0 {
		if run, err := s.Store.Run(ctx, o.Run); err == nil {
			if o.Agent == "" {
				o.Agent = run.Agent
			}
			if o.Session == "" {
				o.Session = run.Session
			}
		} else {
			o.Run = 0 // not a run this daemon knows: do not point at one
		}
	}
	if p := principalOf(ctx); p.Role == RoleDesk {
		// The daemon's own desk: its token says which session it is.
		o.Via = store.OriginDesk
		if o.Session == "" {
			o.Session = p.Session
		}
	}
	if o.Via == store.OriginDesk && o.Session == "" {
		o.Session, _ = s.Store.Setting(ctx, "desk.session")
	}
	return o
}

func (s *Server) closeLane(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req api.CloseLane
	if !decode(w, r, &req) {
		return
	}
	res, err := s.Fold.Close(r.Context(), id, req.Force)
	if err != nil {
		s.foldError(w, err)
		return
	}
	s.Log.Info("lane closed", "lane", res.Lane.Name, "force", req.Force, "branch_deleted", res.BranchDeleted)
	s.Store.AddFeed(r.Context(), store.FeedLaneClosed, fmt.Sprintf("lane %s closed", res.Lane.Name), res.Lane.ID)
	s.refreshView(res.Lane.RepoID)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) rescopeLane(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req api.Rescope
	if !decode(w, r, &req) {
		return
	}
	lane, err := s.Fold.Rescope(r.Context(), id, req.Add, req.Remove)
	if err != nil {
		s.foldError(w, err)
		return
	}
	s.Store.AddFeed(r.Context(), store.FeedLaneOpened, fmt.Sprintf("lane %s scope is now %s", lane.Name, strings.Join(lane.Scope, ", ")), lane.ID)
	s.refreshView(lane.RepoID)
	writeJSON(w, http.StatusOK, lane)
}

func (s *Server) shipLane(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.Runner.Ship(r.Context(), id)
	if err != nil {
		s.foldError(w, err)
		return
	}
	s.Log.Info("lane ship", "lane", res.Lane, "pushed", res.Pushed, "mr", res.MR)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) foldGC(w http.ResponseWriter, r *http.Request) {
	wsID, err := strconv.ParseInt(r.URL.Query().Get("workspace_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("workspace_id is required"))
		return
	}
	stale, err := s.Fold.GC(r.Context(), wsID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if stale == nil {
		stale = []fold.Stale{}
	}
	writeJSON(w, http.StatusOK, stale)
}

func (s *Server) prePush(w http.ResponseWriter, r *http.Request) {
	var req api.PrePush
	if !decode(w, r, &req) {
		return
	}
	if !filepath.IsAbs(req.Path) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path %q: must be absolute", req.Path))
		return
	}
	res, err := Resolve(r.Context(), s.Store, s.LiveConfig(), req.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	v, err := s.Fold.CheckPushTo(r.Context(), res.Lane, res.Repo, req.Path, req.Remote, req.Refs)
	if err != nil {
		s.fail(w, err)
		return
	}
	if v.Lane != "" {
		s.Log.Info("pre-push checked", "lane", v.Lane, "ok", v.OK, "problems", len(v.Problems))
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) repoHook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req api.RepoHook
	if !decode(w, r, &req) {
		return
	}
	repo, err := s.Store.Repo(r.Context(), id)
	if err != nil {
		s.foldError(w, err)
		return
	}
	var st fold.HookState
	switch req.Action {
	case "status":
		st, err = fold.Hook(r.Context(), repo.Path)
	case "install":
		st, err = fold.InstallHook(r.Context(), repo.Path, s.Fold.Exe)
	case "uninstall":
		st, err = fold.UninstallHook(r.Context(), repo.Path)
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("action %q: want install, uninstall or status", req.Action))
		return
	}
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req dispatch.StartRequest
	if !decode(w, r, &req) {
		return
	}
	run, err := s.Runner.Start(r.Context(), req)
	if err != nil {
		s.foldError(w, err)
		return
	}
	view, err := s.runView(r.Context(), run)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	laneID, _ := strconv.ParseInt(q.Get("lane_id"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	runs, err := s.Store.Runs(r.Context(), laneID, q.Get("state"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.RunView{}
	for _, run := range runs {
		v, err := s.runView(r.Context(), run)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	v, err := s.runView(r.Context(), run)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) runLog(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	data, next, err := dispatch.ReadLog(run.Log, offset, 64<<10)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.fail(w, err)
		return
	}
	done := run.State != store.RunRunning && len(data) < 64<<10
	writeJSON(w, http.StatusOK, api.RunLog{Data: string(data), Offset: next, Done: done})
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	if err := s.Runner.Stop(r.Context(), run.ID); err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

func (s *Server) addEvent(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	var e store.Event
	if !decode(w, r, &e) {
		return
	}
	e.RunID = run.ID
	e, err := s.Runner.Record(r.Context(), e)
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	events, err := s.Store.Events(r.Context(), run.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	all, err := s.Store.Decisions(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.RunEvents{Events: nonNilEvents(events), Decisions: []store.Decision{}}
	for _, d := range all {
		if d.RunID == run.ID {
			out.Decisions = append(out.Decisions, d)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func nonNilEvents(e []store.Event) []store.Event {
	if e == nil {
		return []store.Event{}
	}
	return e
}

func (s *Server) addDecision(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	var d store.Decision
	if !decode(w, r, &d) {
		return
	}
	d.RunID = run.ID
	d, err := s.Runner.Ask(r.Context(), d)
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) addRequest(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pathRun(w, r)
	if !ok {
		return
	}
	var q store.Request
	if !decode(w, r, &q) {
		return
	}
	q.FromRun = run.ID
	q, err := s.Runner.RequestHelp(r.Context(), q)
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	var states []string
	if st := r.URL.Query().Get("state"); st != "" {
		states = strings.Split(st, ",")
	}
	reqs, err := s.Store.Requests(r.Context(), states...)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.RequestView{}
	for _, q := range reqs {
		run, err := s.Store.Run(r.Context(), q.FromRun)
		if err != nil {
			s.fail(w, err)
			return
		}
		v, err := s.runView(r.Context(), run)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, api.RequestView{Request: q, FromAgent: run.Agent, FromLane: v.Lane, Repo: v.Repo})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) routeRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req api.Route
	if !decode(w, r, &req) {
		return
	}
	q, err := s.Runner.RouteRequest(r.Context(), id, req.Lane, req.Agent)
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) closeRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req api.CloseRequest
	if !decode(w, r, &req) {
		return
	}
	q, err := s.Runner.CloseRequest(r.Context(), id, req.Why)
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) foldImport(w http.ResponseWriter, r *http.Request) {
	var req api.FoldImport
	if !decode(w, r, &req) {
		return
	}
	plan, err := s.Fold.ImportAdopt(r.Context(), req.RepoID, req.File, req.Apply, req.Adopt)
	if err != nil {
		s.foldError(w, err)
		return
	}
	if req.Apply {
		n := 0
		for _, it := range plan.Items {
			if it.Action == "import" {
				n++
			}
		}
		s.Store.AddFeed(r.Context(), store.FeedLaneOpened, fmt.Sprintf("imported %d lane(s) from the coordination file", n), req.RepoID)
		s.refreshView(req.RepoID)
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) foldReview(w http.ResponseWriter, r *http.Request) {
	var req api.FoldImport
	if !decode(w, r, &req) {
		return
	}
	reviews, err := s.Fold.ReviewRepo(r.Context(), req.RepoID)
	if err != nil {
		s.foldError(w, err)
		return
	}
	if reviews == nil {
		reviews = []fold.Review{}
	}
	writeJSON(w, http.StatusOK, reviews)
}

func (s *Server) foldRetire(w http.ResponseWriter, r *http.Request) {
	var req api.FoldRetire
	if !decode(w, r, &req) {
		return
	}
	rev, err := s.Fold.Retire(r.Context(), req.RepoID, req.Worktree)
	if err != nil {
		s.foldError(w, err)
		return
	}
	s.Log.Info("worktree retired", "worktree", rev.Worktree, "branch", rev.Branch)
	s.Store.AddFeed(r.Context(), store.FeedLaneClosed, fmt.Sprintf("retired worktree %s (%s): %s; branch kept", filepath.Base(rev.Worktree), rev.Branch, rev.Why), req.RepoID)
	writeJSON(w, http.StatusOK, rev)
}

func (s *Server) foldView(w http.ResponseWriter, r *http.Request) {
	var req api.FoldView
	if !decode(w, r, &req) {
		return
	}
	repo, err := s.Store.Repo(r.Context(), req.RepoID)
	if err != nil {
		s.foldError(w, err)
		return
	}
	view, err := s.Fold.RenderView(r.Context(), repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.FoldView{RepoID: repo.ID, View: view, File: s.LiveConfig().Profile(repo.Name).CoordFile}
	if req.Write {
		if out.File == "" {
			writeError(w, http.StatusConflict, fmt.Errorf("repo %s has no coord_file in its profile", repo.Name))
			return
		}
		if err := s.Fold.WriteView(r.Context(), repo.ID); err != nil {
			s.fail(w, err)
			return
		}
		out.Write = true
	}
	writeJSON(w, http.StatusOK, out)
}

// refreshView rewrites a repo's generated coordination view, when it has one, in the
// background: a view that cannot be written is logged, never a failed request.
func (s *Server) refreshView(repoID int64) {
	go func() {
		if err := s.Fold.WriteView(context.Background(), repoID); err != nil {
			s.Log.Error("write coordination view", "repo_id", repoID, "err", err)
		}
	}()
}

func (s *Server) importSessions(w http.ResponseWriter, r *http.Request) {
	var req api.SessionImport
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	wss, err := s.Store.Workspaces(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.SessionView{}
	for _, ws := range wss {
		repos, err := s.Store.Repos(ctx, ws.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, repo := range repos {
			if req.RepoID != 0 && repo.ID != req.RepoID {
				continue
			}
			roots := []string{repo.Path, repo.Path + "-worktrees"}
			if wr := s.LiveConfig().Profile(repo.Name).WorktreeRoot; wr != "" {
				if !filepath.IsAbs(wr) {
					wr = filepath.Join(filepath.Dir(repo.Path), wr)
				}
				roots = append(roots, wr)
			}
			found, err := convo.Scan(convo.ClaudeHome(), roots)
			if err != nil {
				s.fail(w, err)
				return
			}
			for _, f := range found {
				c := store.Conversation{ID: f.ID, Agent: f.Agent, RepoID: repo.ID, Dir: f.Dir, Title: f.Title,
					Branches: f.Branches, File: f.File, Started: f.Started, Last: f.Last}
				if err := s.Store.PutConversation(ctx, c); err != nil {
					s.fail(w, err)
					return
				}
				out = append(out, api.SessionView{Conversation: c, Repo: repo.Name, InUse: convo.InUse(f.File)})
			}
			if len(found) > 0 {
				s.Store.AddFeed(ctx, store.FeedSession, fmt.Sprintf("adopted %d conversation(s) from %s", len(found), repo.Name), repo.ID)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	repoID, _ := strconv.ParseInt(r.URL.Query().Get("repo_id"), 10, 64)
	cs, err := s.Store.Conversations(r.Context(), repoID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.SessionView{}
	for _, c := range cs {
		v := api.SessionView{Conversation: c, InUse: convo.InUse(c.File)}
		if repo, err := s.Store.Repo(r.Context(), c.RepoID); err == nil {
			v.Repo = repo.Name
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) askSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.Ask
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, errors.New("ask a question"))
		return
	}
	cs, err := s.Store.Conversations(r.Context(), 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	var c *store.Conversation
	for i := range cs {
		if cs[i].ID == id {
			c = &cs[i]
		}
	}
	if c == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no adopted conversation %s", id))
		return
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("claude is not on the daemon's PATH"))
		return
	}
	// One question at a time per conversation: two at once would interleave in the session.
	lock := s.sessionLock(c.ID)
	if !lock.TryLock() {
		writeError(w, http.StatusConflict, fmt.Errorf("conversation %s is already answering a question", c.ID[:8]))
		return
	}
	defer lock.Unlock()
	a, err := convo.Ask(r.Context(), bin, *c, req.Question)
	if a.USD > 0 && s.Runner != nil {
		s.Runner.Spend(r.Context(), store.Spend{Source: "session", USD: a.USD})
	} else if a.USD > 0 {
		s.Store.AddSpend(r.Context(), store.Spend{Day: dispatch.Today(), Source: "session", USD: a.USD})
	}
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.Store.AddFeed(r.Context(), store.FeedSession, fmt.Sprintf("asked conversation %s (%s): %s", c.ID[:8], clipText(c.Title, 50), clipText(req.Question, 120)), 0)
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) sessionLock(id string) *sync.Mutex {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if s.sessLocks == nil {
		s.sessLocks = map[string]*sync.Mutex{}
	}
	if s.sessLocks[id] == nil {
		s.sessLocks[id] = &sync.Mutex{}
	}
	return s.sessLocks[id]
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

func (s *Server) reserveTag(w http.ResponseWriter, r *http.Request) {
	var req api.Reserve
	if !decode(w, r, &req) {
		return
	}
	if lane, err := s.workerLane(r); err != nil {
		s.foldError(w, err)
		return
	} else if lane != nil && (req.LaneID != lane.ID || req.RepoID != lane.RepoID) {
		writeError(w, http.StatusForbidden, fmt.Errorf("an agent's token reserves only for its own lane, %s", lane.Name))
		return
	}
	res, err := s.Fold.Reserve(r.Context(), req.RepoID, req.LaneID, req.Bump)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		s.foldError(w, err)
		return
	}
	who := "no lane"
	if l, err := s.Store.Lane(r.Context(), res.LaneID); err == nil {
		who = "lane " + l.Name
	}
	s.Store.AddFeed(r.Context(), store.FeedTag, fmt.Sprintf("%s reserved for %s", res.Tag, who), res.ID)
	s.refreshView(res.RepoID)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	repoID, err := strconv.ParseInt(r.URL.Query().Get("repo_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("repo_id is required"))
		return
	}
	rs, err := s.Store.Reservations(r.Context(), repoID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.ReservationView{}
	for _, res := range rs {
		v := api.ReservationView{Reservation: res}
		if l, err := s.Store.Lane(r.Context(), res.LaneID); err == nil {
			v.Lane = l.Name
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) releaseTag(w http.ResponseWriter, r *http.Request) {
	var req api.ReleaseTag
	if !decode(w, r, &req) {
		return
	}
	if lane, err := s.workerLane(r); err != nil {
		s.foldError(w, err)
		return
	} else if lane != nil && !s.reservedFor(r.Context(), req.RepoID, req.Tag, lane.ID) {
		writeError(w, http.StatusForbidden, fmt.Errorf("an agent's token releases only its own lane's reservations, and %s is not one of lane %s's", req.Tag, lane.Name))
		return
	}
	if err := s.Fold.ReleaseTag(r.Context(), req.RepoID, req.Tag); err != nil {
		s.foldError(w, err)
		return
	}
	s.Store.AddFeed(r.Context(), store.FeedTag, req.Tag+" released", 0)
	s.refreshView(req.RepoID)
	writeJSON(w, http.StatusOK, req)
}

// reservedFor says whether tag is reserved in the repo for the lane.
func (s *Server) reservedFor(ctx context.Context, repoID int64, tag string, laneID int64) bool {
	rs, err := s.Store.Reservations(ctx, repoID)
	if err != nil {
		return false
	}
	for _, res := range rs {
		if res.Tag == tag && res.LaneID == laneID {
			return true
		}
	}
	return false
}

func (s *Server) spendToday(w http.ResponseWriter, r *http.Request) {
	cfg := s.LiveConfig()
	total, by, err := dispatch.Spent(r.Context(), s.Store, cfg)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.SpendToday{Day: dispatch.Today(), USD: total, BySource: by}
	if b := cfg.Daemon.Budget; b != nil {
		out.Budget = *b
	}
	if c := cfg.Daemon.CreditUSD; c != nil {
		out.CreditUSD = *c
	}
	writeJSON(w, http.StatusOK, out)
}

// addSpend records what a client spent outside a run: the front desk's turns.
func (s *Server) addSpend(w http.ResponseWriter, r *http.Request) {
	var sp store.Spend
	if !decode(w, r, &sp) {
		return
	}
	if sp.Source == "" || sp.USD < 0 || sp.Credits < 0 {
		writeError(w, http.StatusBadRequest, errors.New("spend needs a source and amounts that are not negative"))
		return
	}
	if err := s.Runner.Spend(r.Context(), sp); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	if q.Get("after") == "latest" {
		last, err := s.Store.LastFeed(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, api.Feed{Items: []store.FeedItem{}, Events: []string{}, Last: last})
		return
	}
	items, err := s.Store.Feed(r.Context(), after, 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.Feed{Items: []store.FeedItem{}, Events: []string{}, Last: after}
	if items != nil {
		out.Items = items
		out.Last = items[len(items)-1].ID
		for _, it := range items {
			out.Events = append(out.Events, api.EventKind(it.Kind))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// Settings the daemon keeps for clients, by name; anything else is refused.
var settingKeys = map[string]bool{"desk.session": true, "desk.agent": true, "desk.model": true}

func (s *Server) getSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !settingKeys[key] {
		writeError(w, http.StatusNotFound, fmt.Errorf("no setting %q", key))
		return
	}
	v, err := s.Store.Setting(r.Context(), key)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.Setting{Value: v}
	if key == "desk.model" {
		out.Configured = s.LiveConfig().Desk.Model
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !settingKeys[key] {
		writeError(w, http.StatusNotFound, fmt.Errorf("no setting %q", key))
		return
	}
	var v api.Setting
	if !decode(w, r, &v) {
		return
	}
	v.Value = strings.TrimSpace(v.Value)
	if key == "desk.model" && strings.ContainsAny(v.Value, " \t\n") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not a model name", v.Value))
		return
	}
	v.Configured = ""
	if err := s.Store.SetSetting(r.Context(), key, v.Value); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Store.Decisions(r.Context(), r.URL.Query().Get("state"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []api.DecisionView{}
	for _, d := range ds {
		run, err := s.Store.Run(r.Context(), d.RunID)
		if err != nil {
			s.fail(w, err)
			return
		}
		v, err := s.runView(r.Context(), run)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, api.DecisionView{Decision: d, Agent: run.Agent, Lane: v.Lane, Repo: v.Repo})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) answerDecision(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req api.Answer
	if !decode(w, r, &req) {
		return
	}
	d, err := s.Runner.Answer(r.Context(), id, req.Answer)
	if err != nil {
		s.foldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) pathRun(w http.ResponseWriter, r *http.Request) (store.Run, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return store.Run{}, false
	}
	run, err := s.Store.Run(r.Context(), id)
	if err != nil {
		s.foldError(w, err)
		return run, false
	}
	return run, true
}

func (s *Server) runView(ctx context.Context, run store.Run) (api.RunView, error) {
	lane, err := s.Store.Lane(ctx, run.LaneID)
	if err != nil {
		return api.RunView{}, err
	}
	repo, err := s.Store.Repo(ctx, lane.RepoID)
	if err != nil {
		return api.RunView{}, err
	}
	return api.RunView{Run: run, Lane: lane.Name, Repo: repo.Name, Worktree: lane.Worktree}, nil
}

// foldError answers a refusal with 409 and its reason, a missing lane or repo with 404,
// and anything else as a failure.
func (s *Server) foldError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fold.ErrRefused), errors.Is(err, dispatch.ErrRefused):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	default:
		s.fail(w, err)
	}
}

// decode reads a JSON body strictly, answering 400 itself when it cannot.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// shutdown asks Run to stop. The daemon then exits cleanly, which a service manager
// configured to restart on failure only leaves stopped.
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	s.Log.Info("shutdown requested")
	writeJSON(w, http.StatusAccepted, struct{}{})
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, err)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Error: redact.String(err.Error())})
}

// Run takes the home's lock, listens, writes the runtime file, serves until ctx is done
// or a shutdown is requested, then removes the runtime file. The lock makes a second
// daemon on the same home refuse to start, even when two start at the same moment.
//
// A SIGHUP reloads the config (ReloadConfig). Agents the daemon started are its to end:
// a stop ends them (Runner.Shutdown) and records their runs interrupted. A daemon that
// crashed or was killed could not, so the next one stops any it left running
// (ReapOrphans) before marking their runs interrupted (Runner.Recover).
func (s *Server) Run(ctx context.Context, runtimePath string) error {
	lock, err := acquireLock(filepath.Join(filepath.Dir(runtimePath), "daemon.lock"))
	if err != nil {
		if rt, rerr := ReadRuntime(runtimePath); rerr == nil {
			return fmt.Errorf("a daemon is already running (pid %d at %s)", rt.PID, rt.Addr)
		}
		return fmt.Errorf("a daemon is already running (%v)", err)
	}
	defer lock.Close()

	ln, err := net.Listen("tcp", s.LiveConfig().Daemon.Listen)
	if err != nil {
		return err
	}
	rt := api.Runtime{
		Addr: ln.Addr().String(), PID: os.Getpid(), Token: s.Token,
		Version: version.Version, Started: s.started,
	}
	// Listen for SIGHUP before the runtime file says where the daemon is, so a reload
	// sent as soon as it answers cannot kill it.
	reloadCtx, stopReload := context.WithCancel(ctx)
	reloadDone := watchReload(reloadCtx, s)
	defer func() { stopReload(); <-reloadDone }()
	if err := writeRuntime(runtimePath, rt); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(runtimePath)

	s.addr.Store(&rt.Addr)
	if s.Runner != nil {
		// Each run's agent gets its own worker token, never the operator's.
		s.Runner.SetCredentials(s)
		s.ReapOrphans(ctx)
		if err := s.Runner.Recover(ctx); err != nil {
			return err
		}
		go s.Runner.Route(context.Background()) // requests left waiting by a previous daemon
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s.Runner.Shutdown(stopCtx)
		}()
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	defer s.Close()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.Log.Info("shepherd daemon listening", "addr", rt.Addr, "version", rt.Version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	case <-s.stop:
	}
	s.Log.Info("shepherd daemon stopping")
	// End the streams and the desk's turn first: a stream would hold Shutdown open.
	s.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// ReadRuntime reads a daemon's runtime file.
func ReadRuntime(path string) (api.Runtime, error) {
	var rt api.Runtime
	b, err := os.ReadFile(path)
	if err != nil {
		return rt, err
	}
	return rt, json.Unmarshal(b, &rt)
}

// writeRuntime writes the file readable by its owner only, since it holds the token, and
// atomically, so a client never reads half of it.
func writeRuntime(path string, rt api.Runtime) error {
	b, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Alive reports whether the daemon described by rt answers.
func Alive(rt api.Runtime) bool {
	c := http.Client{Timeout: time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+rt.Addr+api.PathStatus, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+rt.Token)
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
