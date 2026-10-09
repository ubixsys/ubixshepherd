package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := NewServer(st, config.Default(), "/cfg", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func call(t *testing.T, ts *httptest.Server, token, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestAuth(t *testing.T) {
	s, ts := newServer(t)
	if code := call(t, ts, "wrong", "GET", api.PathStatus, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code := call(t, ts, s.Token, "GET", api.PathStatus, nil, nil); code != http.StatusOK {
		t.Errorf("right token: %d", code)
	}
}

func TestSaveAndStatusAndResolve(t *testing.T) {
	s, ts := newServer(t)
	root, _ := paths.Canonical(t.TempDir())
	app := filepath.Join(root, "app")
	os.MkdirAll(filepath.Join(app, "src"), 0o755)

	var saved api.WorkspaceDetail
	code := call(t, ts, s.Token, "POST", api.PathWorkspaces, api.SaveWorkspace{
		Name: "git", Path: root, Repos: []store.Repo{{Name: "app", Path: app, Stacks: []string{"go"}}},
	}, &saved)
	if code != http.StatusOK || len(saved.Repos) != 1 {
		t.Fatalf("save: %d %+v", code, saved)
	}

	var st api.Status
	call(t, ts, s.Token, "GET", api.PathStatus, nil, &st)
	if len(st.Workspaces) != 1 || st.Workspaces[0].Repos != 1 || st.Store != "sqlite" {
		t.Errorf("status = %+v", st)
	}

	var res api.Resolution
	call(t, ts, s.Token, "GET", api.PathResolve+"?path="+filepath.Join(app, "src"), nil, &res)
	if res.Workspace == nil || res.Repo == nil || res.Repo.Name != "app" || res.Profile == nil || res.Profile.BaseBranch != "main" {
		t.Errorf("resolve in repo = %+v", res)
	}

	res = api.Resolution{}
	call(t, ts, s.Token, "GET", api.PathResolve+"?path="+root, nil, &res)
	if res.Workspace == nil || res.Repo != nil {
		t.Errorf("resolve at root = %+v", res)
	}

	if code := call(t, ts, s.Token, "GET", api.PathResolve+"?path=relative", nil, nil); code != http.StatusBadRequest {
		t.Errorf("relative path: %d", code)
	}
}

func TestSaveRejects(t *testing.T) {
	s, ts := newServer(t)
	root, _ := paths.Canonical(t.TempDir())
	other, _ := paths.Canonical(t.TempDir())
	cases := map[string]api.SaveWorkspace{
		"repo outside":  {Name: "w", Path: root, Repos: []store.Repo{{Name: "x", Path: other}}},
		"repo is root":  {Name: "w", Path: root, Repos: []store.Repo{{Name: ".", Path: root}}},
		"wrong name":    {Name: "w", Path: root, Repos: []store.Repo{{Name: "y", Path: filepath.Join(root, "x")}}},
		"relative":      {Name: "w", Path: "git"},
		"slash in name": {Name: "a/b", Path: root},
		"empty name":    {Path: root},
	}
	for name, req := range cases {
		var e api.Error
		if code := call(t, ts, s.Token, "POST", api.PathWorkspaces, req, &e); code != http.StatusBadRequest || e.Error == "" {
			t.Errorf("%s: %d %q", name, code, e.Error)
		}
	}
}

// laneStore adds a lane to an otherwise empty store, since lanes are opened in M2.
type laneStore struct {
	store.Store
	ws    store.Workspace
	repos []store.Repo
	lanes []store.Lane
}

func (l laneStore) Workspaces(context.Context) ([]store.Workspace, error) {
	return []store.Workspace{l.ws}, nil
}
func (l laneStore) Repos(context.Context, int64) ([]store.Repo, error) { return l.repos, nil }
func (l laneStore) Lanes(_ context.Context, repoID int64) ([]store.Lane, error) {
	var out []store.Lane
	for _, ln := range l.lanes {
		if ln.RepoID == repoID {
			out = append(out, ln)
		}
	}
	return out, nil
}

func TestResolveLaneAndNesting(t *testing.T) {
	root, _ := paths.Canonical(t.TempDir())
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	st := laneStore{
		ws: store.Workspace{ID: 1, Name: "git", Path: root},
		repos: []store.Repo{
			{ID: 1, Name: "group", Path: j("group")},
			{ID: 2, Name: "group/lib", Path: j("group", "lib")},
		},
		lanes: []store.Lane{{ID: 1, RepoID: 2, Name: "fix", Branch: "fix/x", Worktree: j("group", "lib-worktrees", "fix"), State: "open"}},
	}
	cfg, err := config.Parse([]byte("repos:\n  group/lib:\n    base_branch: dev\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	res, _ := Resolve(ctx, st, cfg, j("group", "lib", "pkg"))
	if res.Repo == nil || res.Repo.Name != "group/lib" || res.Profile.BaseBranch != "dev" {
		t.Errorf("deepest repo should win: %+v", res.Repo)
	}
	res, _ = Resolve(ctx, st, cfg, j("group", "lib-worktrees", "fix", "src"))
	if res.Lane == nil || res.Lane.Name != "fix" || res.Repo.Name != "group/lib" {
		t.Errorf("lane worktree: %+v %+v", res.Lane, res.Repo)
	}
	res, _ = Resolve(ctx, st, cfg, filepath.Dir(root))
	if res.Workspace != nil {
		t.Errorf("outside the workspace resolved to %+v", res.Workspace)
	}
}

func TestRunWritesAndRemovesRuntime(t *testing.T) {
	s, _ := newServer(t)
	rtPath := filepath.Join(t.TempDir(), "daemon.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, rtPath) }()

	var rt api.Runtime
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		if rt, err = ReadRuntime(rtPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime file never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rt.Token != s.Token || !strings.HasPrefix(rt.Addr, "127.0.0.1:") {
		t.Errorf("runtime = %+v", rt)
	}
	if fi, _ := os.Stat(rtPath); fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("runtime file mode %v is readable by others", fi.Mode().Perm())
	}

	// A second daemon on the same home refuses to start.
	s2, _ := newServer(t)
	if err := s2.Run(context.Background(), rtPath); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second daemon: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if _, err := os.Stat(rtPath); !os.IsNotExist(err) {
		t.Error("runtime file left behind")
	}
}

func TestShutdownEndsRun(t *testing.T) {
	s, ts := newServer(t)
	rtPath := filepath.Join(t.TempDir(), "daemon.json")
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), rtPath) }()
	for i := 0; i < 100; i++ {
		if _, err := ReadRuntime(rtPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := call(t, ts, s.Token, "POST", api.PathShutdown, nil, nil); code != http.StatusAccepted {
		t.Fatalf("shutdown: %d", code)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	// A second shutdown request must not panic on the closed channel.
	call(t, ts, s.Token, "POST", api.PathShutdown, nil, nil)
}

func TestRequestLog(t *testing.T) {
	s, ts := newServer(t)
	var buf bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ts.Config.Handler = s.Handler()

	call(t, ts, s.Token, "GET", api.PathStatus, nil, nil)
	req, _ := http.NewRequest("GET", ts.URL+api.PathLanes+"?workspace_id=1", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set(api.ClientHeader, "mcp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	call(t, ts, "wrong", "GET", api.PathLanes, nil, nil)

	log := buf.String()
	if strings.Contains(log, api.PathStatus) {
		t.Errorf("status probe logged:\n%s", log)
	}
	if !strings.Contains(log, "client=mcp method=GET path=/v1/lanes status=200") {
		t.Errorf("lanes request not logged with its client:\n%s", log)
	}
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "status=401") {
		t.Errorf("rejected request not logged as a warning:\n%s", log)
	}
}

// At the default level a successful read leaves no trace, a change is info, and a
// failure is a warning or an error.
func TestRequestLevels(t *testing.T) {
	for _, c := range []struct {
		method string
		code   int
		took   time.Duration
		want   slog.Level
	}{
		{"GET", 200, time.Millisecond, slog.LevelDebug},
		{"GET", 304, time.Millisecond, slog.LevelDebug},
		{"POST", 200, time.Millisecond, slog.LevelInfo},
		{"PUT", 202, time.Millisecond, slog.LevelInfo},
		{"DELETE", 200, time.Millisecond, slog.LevelInfo},
		{"GET", 401, time.Millisecond, slog.LevelWarn},
		{"POST", 409, time.Millisecond, slog.LevelWarn},
		{"GET", 500, time.Millisecond, slog.LevelError},
		{"POST", 503, time.Millisecond, slog.LevelError},
		{"GET", 200, 3 * time.Second, slog.LevelWarn},
		{"POST", 200, 3 * time.Second, slog.LevelWarn},
		{"GET", 200, 2 * time.Second, slog.LevelDebug},
	} {
		if got := requestLevel(c.method, c.code, c.took); got != c.want {
			t.Errorf("%s %d in %v: %v, want %v", c.method, c.code, c.took, got, c.want)
		}
	}

	s, ts := newServer(t)
	var buf bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&buf, nil))
	ts.Config.Handler = s.Handler()
	call(t, ts, s.Token, "GET", api.PathWorkspaces, nil, nil)
	call(t, ts, s.Token, "GET", api.PathFeed+"?after=0", nil, nil)
	if buf.Len() != 0 {
		t.Errorf("successful reads logged at the default level:\n%s", buf.String())
	}
	call(t, ts, s.Token, "POST", api.PathLanes, nil, nil) // refused: no lane to open
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("failed change not logged:\n%s", buf.String())
	}
}

func TestLogLevelEnv(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "debug": slog.LevelDebug, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn,
		"error": slog.LevelError, "loud": slog.LevelInfo,
	} {
		t.Setenv(LogLevelEnv, in)
		if got := LogLevel(""); got != want {
			t.Errorf("%s=%q: %v, want %v", LogLevelEnv, in, got, want)
		}
	}
	// The variable overrides daemon.log_level; unset or unrecognised, the config wins.
	t.Setenv(LogLevelEnv, "error")
	if got := LogLevel("debug"); got != slog.LevelError {
		t.Errorf("env over config: %v", got)
	}
	for _, env := range []string{"", "loud"} {
		t.Setenv(LogLevelEnv, env)
		if got := LogLevel("warn"); got != slog.LevelWarn {
			t.Errorf("%s=%q, config warn: %v", LogLevelEnv, env, got)
		}
	}
}

// A server with no Runner still reports spend from the store, and refuses what needs
// agents instead of panicking.
func TestNoRunner(t *testing.T) {
	s, ts := newServer(t)
	ctx := context.Background()
	if err := s.Store.AddSpend(ctx, store.Spend{Day: time.Now().Format("2006-01-02"), Source: "run", USD: 1.25}); err != nil {
		t.Fatal(err)
	}
	var sp api.SpendToday
	if code := call(t, ts, s.Token, "GET", api.PathSpend, nil, &sp); code != http.StatusOK || sp.USD != 1.25 {
		t.Errorf("spend: %d %+v", code, sp)
	}
	var e struct{ Error string }
	if code := call(t, ts, s.Token, "POST", api.PathRuns, map[string]any{}, &e); code != http.StatusServiceUnavailable || !strings.Contains(e.Error, "does not run agents") {
		t.Errorf("start run: %d %+v", code, e)
	}
}

// seedLane saves a workspace with one repo and opens a lane in the store directly.
func seedLane(t *testing.T, s *Server, name string) (store.Workspace, store.Lane) {
	t.Helper()
	ctx := context.Background()
	root, _ := paths.Canonical(t.TempDir())
	ws, err := s.Store.SaveWorkspace(ctx, store.Workspace{Name: "git", Path: root},
		[]store.Repo{{Name: "app", Path: filepath.Join(root, "app")}})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := s.Store.Repos(ctx, ws.ID)
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos: %v %v", repos, err)
	}
	l, err := s.Store.CreateLane(ctx, store.Lane{RepoID: repos[0].ID, Name: name, Branch: name, Base: "main",
		Worktree: filepath.Join(root, name), State: store.LaneOpen})
	if err != nil {
		t.Fatal(err)
	}
	return ws, l
}

func TestLaneViewForge(t *testing.T) {
	s, ts := newServer(t)
	ctx := context.Background()
	ws, none := seedLane(t, s, "no-mr")
	repoID := none.RepoID
	mk := func(name string, lf store.LaneForge) store.Lane {
		l, err := s.Store.CreateLane(ctx, store.Lane{RepoID: repoID, Name: name, Branch: name, Base: "main",
			Worktree: filepath.Join(t.TempDir(), name), State: store.LaneOpen})
		if err != nil {
			t.Fatal(err)
		}
		lf.LaneID = l.ID
		if err := s.Store.PutLaneForge(ctx, lf); err != nil {
			t.Fatal(err)
		}
		return l
	}
	mk("failing", store.LaneForge{MR: 34, MRState: "opened", MRURL: "https://x/34", Pipeline: 9, PipelineStatus: "failed"})
	mk("odd", store.LaneForge{MR: 35, MRState: "locked", Pipeline: 10, PipelineStatus: "manual"})
	mk("merged-nopipe", store.LaneForge{MR: 36, MRState: "merged"})

	var lanes []api.LaneView
	if code := call(t, ts, s.Token, "GET", fmt.Sprintf("%s?workspace_id=%d", api.PathLanes, ws.ID), nil, &lanes); code != 200 {
		t.Fatalf("lanes: %d", code)
	}
	by := map[string]api.LaneView{}
	for _, l := range lanes {
		by[l.Name] = l
	}
	if v := by["no-mr"]; v.MR != 0 || v.MRState != "" || v.MRURL != "" || v.Pipeline != 0 || v.PipelineStatus != "" {
		t.Errorf("lane without MR has forge fields: %+v", v)
	}
	if v := by["failing"]; v.MR != 34 || v.MRState != api.MRStateOpen || v.MRURL != "https://x/34" || v.Pipeline != 9 || v.PipelineStatus != api.PipelineFailed {
		t.Errorf("failing = %+v", v)
	}
	if v := by["odd"]; v.MRState != api.MRStateUnknown || v.PipelineStatus != api.PipelineUnknown {
		t.Errorf("unknown states = %+v", v)
	}
	if v := by["merged-nopipe"]; v.MRState != api.MRStateMerged || v.PipelineStatus != "" {
		t.Errorf("merged = %+v", v)
	}

	// The wire shape: a lane without an MR carries no forge keys at all.
	var raw []map[string]any
	call(t, ts, s.Token, "GET", fmt.Sprintf("%s?workspace_id=%d", api.PathLanes, ws.ID), nil, &raw)
	for _, m := range raw {
		if m["name"] == "no-mr" {
			for _, k := range []string{"mr", "mr_state", "mr_url", "pipeline", "pipeline_status"} {
				if _, ok := m[k]; ok {
					t.Errorf("no-mr lane has key %q", k)
				}
			}
		}
	}
}

func TestFeedCarriesEvents(t *testing.T) {
	s, ts := newServer(t)
	ctx := context.Background()
	for _, k := range []string{store.FeedRunStarted, store.FeedRequestStuck, store.FeedSession, "mystery", FeedConfig} {
		s.Store.AddFeed(ctx, k, "x", 0)
	}
	var f api.Feed
	call(t, ts, s.Token, "GET", api.PathFeed+"?after=0", nil, &f)
	want := []string{api.EventRunStarted, api.EventRequestAttention, api.EventInfo, api.EventInfo, api.EventConfig}
	if len(f.Events) != len(want) {
		t.Fatalf("events = %v", f.Events)
	}
	for i, w := range want {
		if f.Events[i] != w || f.Event(i) != w {
			t.Errorf("event %d (%s) = %q, want %q", i, f.Items[i].Kind, f.Events[i], w)
		}
	}
	var empty api.Feed
	call(t, ts, s.Token, "GET", api.PathFeed+"?after=latest", nil, &empty)
	if empty.Items == nil || empty.Events == nil {
		t.Error("an empty feed must send [] not null")
	}
}

// desk.model is a setting the chat may store; a read carries config.yaml's value under it.
func TestDeskModelSetting(t *testing.T) {
	s, ts := newServer(t)
	cfg := s.LiveConfig()
	cfg.Desk.Model = "sonnet"
	s.applyConfig(cfg)
	path := api.PathSettings + "/desk.model"
	var got api.Setting
	if code := call(t, ts, s.Token, "GET", path, nil, &got); code != http.StatusOK || got.Value != "" || got.Configured != "sonnet" {
		t.Errorf("unset: %d %+v", code, got)
	}
	if code := call(t, ts, s.Token, "PUT", path, api.Setting{Value: " opus "}, nil); code != http.StatusOK {
		t.Errorf("set: %d", code)
	}
	got = api.Setting{}
	if call(t, ts, s.Token, "GET", path, nil, &got); got.Value != "opus" || got.Configured != "sonnet" {
		t.Errorf("set: %+v", got)
	}
	if code := call(t, ts, s.Token, "PUT", path, api.Setting{Value: "my model"}, nil); code != http.StatusBadRequest {
		t.Errorf("a name with a space: %d", code)
	}
	if code := call(t, ts, s.Token, "GET", api.PathSettings+"/desk.nope", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown key: %d", code)
	}
}
