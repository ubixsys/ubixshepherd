package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

// twoRuns records a workspace with two lanes and a running run in each, in the store
// alone: enough for authorization, which runs before any handler touches git.
func twoRuns(t *testing.T, s *Server) (a, b store.Run, la, lb store.Lane) {
	t.Helper()
	ctx := context.Background()
	root, _ := paths.Canonical(t.TempDir())
	ws, err := s.Store.SaveWorkspace(ctx, store.Workspace{Name: "ws", Path: root}, []store.Repo{{Name: "app", Path: filepath.Join(root, "app")}})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := s.Store.Repos(ctx, ws.ID)
	mk := func(name string) (store.Run, store.Lane) {
		l, err := s.Store.CreateLane(ctx, store.Lane{RepoID: repos[0].ID, Name: name, Branch: name, Base: "main",
			Worktree: filepath.Join(root, name), Scope: []string{name + "/**"}, State: store.LaneOpen})
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Store.CreateRun(ctx, store.Run{LaneID: l.ID, Agent: "claude", Prompt: "x", State: store.RunRunning, Log: filepath.Join(root, name+".log")})
		if err != nil {
			t.Fatal(err)
		}
		return r, l
	}
	a, la = mk("a")
	b, lb = mk("b")
	return a, b, la, lb
}

func denied(code int) bool { return code == http.StatusUnauthorized || code == http.StatusForbidden }

// Every route, and which roles it lets through. Allowed means past authorization: the
// handler may still refuse the request on its merits (a missing body, no runner).
func TestRoleEndpointMatrix(t *testing.T) {
	s, ts := newServer(t)
	a, b, la, _ := twoRuns(t, s)
	worker, err := s.MintWorker(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	desk, _ := s.MintDesk()

	type want struct{ operator, desk, worker bool }
	all, op, opDesk := want{true, true, true}, want{true, false, false}, want{true, true, false}
	opWorker := want{true, false, true}
	run, other := fmt.Sprint(a.ID), fmt.Sprint(b.ID)
	reserveOwn := api.Reserve{RepoID: la.RepoID, LaneID: la.ID, Bump: "patch"}
	cases := []struct {
		method, path string
		body         any
		want         want
	}{
		{"GET", api.PathStatus, nil, all},
		{"GET", api.PathWorkspaces, nil, opDesk},
		{"POST", api.PathWorkspaces, map[string]any{}, op},
		{"GET", api.PathResolve + "?path=/x", nil, all},
		{"GET", api.PathLanes + "?workspace_id=1", nil, opDesk},
		{"POST", api.PathLanes, map[string]any{}, opDesk},
		{"POST", api.PathLanes + "/1/close", map[string]any{}, opDesk},
		{"POST", api.PathLanes + "/1/scope", map[string]any{}, op},
		{"POST", api.PathLanes + "/1/ship", nil, opDesk},
		{"GET", api.PathFoldGC + "?workspace_id=1", nil, opDesk},
		{"POST", api.PathPrePush, map[string]any{}, opWorker},
		{"POST", "/v1/repos/1/hook", map[string]any{}, op},
		{"POST", api.PathRuns, map[string]any{}, opDesk},
		{"GET", api.PathRuns, nil, opDesk},
		{"GET", api.PathRuns + "/" + run, nil, all},
		{"GET", api.PathRuns + "/" + other, nil, opDesk},
		{"GET", api.PathRuns + "/" + run + "/log", nil, all},
		{"POST", api.PathRuns + "/" + run + "/stop", nil, opDesk},
		{"POST", api.PathRuns + "/" + run + "/events", map[string]any{}, opWorker},
		{"POST", api.PathRuns + "/" + other + "/events", map[string]any{}, op},
		{"GET", api.PathRuns + "/" + run + "/events", nil, all},
		{"POST", api.PathRuns + "/" + run + "/decisions", map[string]any{}, opWorker},
		{"POST", api.PathRuns + "/" + other + "/decisions", map[string]any{}, op},
		{"POST", api.PathRuns + "/" + run + "/requests", map[string]any{}, opWorker},
		{"POST", api.PathRuns + "/" + other + "/requests", map[string]any{}, op},
		{"GET", api.PathDecisions, nil, opDesk},
		// The desk only in a turn the person started; there is none here.
		{"POST", api.PathDecisions + "/1/answer", api.Answer{Answer: "yes"}, op},
		{"GET", api.PathFeed, nil, opDesk},
		{"POST", api.PathFoldImport, map[string]any{}, op},
		{"POST", api.PathFoldView, map[string]any{}, op},
		{"POST", api.PathFoldReview, map[string]any{}, opDesk},
		{"POST", api.PathFoldRetire, map[string]any{}, op},
		{"POST", api.PathSessionsImport, map[string]any{}, op},
		{"GET", api.PathSessions, nil, opDesk},
		{"POST", api.PathSessions + "/x/ask", map[string]any{}, opDesk},
		{"GET", api.PathTags + "?repo_id=1", nil, opDesk},
		{"POST", api.PathTagsReserve, reserveOwn, all},
		{"POST", api.PathTagsRelease, api.ReleaseTag{RepoID: la.RepoID, Tag: "v9.9.9"}, op},
		{"GET", api.PathSpend, nil, opDesk},
		{"POST", api.PathSpend, map[string]any{}, op},
		{"GET", api.PathSettings + "/desk.model", nil, op},
		{"PUT", api.PathSettings + "/desk.model", api.Setting{Value: "m"}, op},
		{"GET", api.PathRequests, nil, opDesk},
		{"POST", api.PathRequests + "/1/route", map[string]any{}, opDesk},
		{"POST", api.PathRequests + "/1/close", map[string]any{}, opDesk},
		{"POST", api.PathShutdown, nil, op},
	}
	for _, c := range cases {
		for _, role := range []struct {
			name, token string
			allow       bool
		}{{"desk", desk, c.want.desk}, {"worker", worker, c.want.worker}, {"operator", s.Token, c.want.operator}} {
			if role.name == "operator" && (c.path == api.PathShutdown || c.path == api.PathSessionsImport) {
				continue // leave the server running, and the person's own sessions unread
			}
			code := call(t, ts, role.token, c.method, c.path, c.body, nil)
			if denied(code) == role.allow {
				t.Errorf("%s %s as %s: %d, want allowed %v", c.method, c.path, role.name, code, role.allow)
			}
		}
	}
}

func TestDeskAnswersOnlyInAHumanTurn(t *testing.T) {
	s, ts := newServer(t)
	desk, _ := s.MintDesk()
	human := false
	f := func() bool { return human }
	s.deskHuman.Store(&f)
	if code := call(t, ts, desk, "POST", api.PathDecisions+"/1/answer", api.Answer{Answer: "yes"}, nil); code != http.StatusForbidden {
		t.Errorf("system turn: %d, want 403", code)
	}
	human = true
	if code := call(t, ts, desk, "POST", api.PathDecisions+"/1/answer", api.Answer{Answer: "yes"}, nil); denied(code) {
		t.Errorf("human turn: %d, want allowed", code)
	}
}

func TestWorkerTokenIsRunScoped(t *testing.T) {
	s, ts := newServer(t)
	a, b, la, lb := twoRuns(t, s)
	tok, _ := s.MintWorker(a.ID)
	// Run B's resources, with run A's token.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", api.PathRuns + fmt.Sprintf("/%d", b.ID), nil},
		{"POST", api.PathRuns + fmt.Sprintf("/%d/events", b.ID), store.Event{Kind: dispatch.EventReport, Status: "done", Text: "x"}},
		{"POST", api.PathTagsReserve, api.Reserve{RepoID: lb.RepoID, LaneID: lb.ID, Bump: "patch"}},
		{"POST", api.PathTagsReserve, api.Reserve{RepoID: la.RepoID, Bump: "patch"}}, // no lane
	} {
		if code := call(t, ts, tok, c.method, c.path, c.body, nil); code != http.StatusForbidden {
			t.Errorf("%s %s with run %d's token: %d, want 403", c.method, c.path, a.ID, code)
		}
	}
	// A reservation of lane B's cannot be released with lane A's token.
	s.Store.CreateReservation(context.Background(), store.Reservation{RepoID: lb.RepoID, LaneID: lb.ID, Tag: "v1.0.1", State: "reserved"})
	if code := call(t, ts, tok, "POST", api.PathTagsRelease, api.ReleaseTag{RepoID: lb.RepoID, Tag: "v1.0.1"}, nil); code != http.StatusForbidden {
		t.Errorf("release another lane's tag: %d, want 403", code)
	}
	s.Revoke(a.ID)
	if code := call(t, ts, tok, "GET", api.PathStatus, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d, want 401", code)
	}
	// A new daemon knows none of the old one's tokens.
	s2, err := NewServer(s.Store, config.Default(), "/cfg", s.Log)
	if err != nil {
		t.Fatal(err)
	}
	tok2, _ := s.MintWorker(a.ID)
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	if code := call(t, ts2, tok2, "GET", api.PathStatus, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("token from before a restart: %d, want 401", code)
	}
}

// fakeTokenAgent prints the token it was given, as a careless agent might, saves it for
// the test, and exits.
const fakeTokenAgent = `#!/bin/sh
if [ "$1" = create-chat ]; then echo "11111111-2222-4333-8444-555555555555"; exit 0; fi
cat >/dev/null
echo "my token is $SHEPHERD_TOKEN"
printf '%s\n%s\n' "$SHEPHERD_TOKEN" "$SHEPHERD_URL" > "$TOKEN_OUT"
f=$(git rev-parse --path-format=absolute --git-path shepherd-run.json)
echo "$f" >> "$TOKEN_OUT"
grep -q "$SHEPHERD_TOKEN" "$f" && echo FILE_OK >> "$TOKEN_OUT"
exit 0
`

// A real run: the agent gets a worker token for its run, the token works while it runs
// and not after, and it is in neither the run log nor the daemon's.
func TestRunGetsAWorkerTokenRevokedAtItsEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent is a shell script")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e"} {
		t.Setenv(k, v)
	}
	t.Setenv(dispatch.EnvToken, "inherited-operator-token-must-not-leak")
	root, _ := paths.Canonical(t.TempDir())
	g := func(dir string, args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	origin := filepath.Join(root, "origin.git")
	g(root, "init", "-q", "--bare", origin)
	ws := filepath.Join(root, "ws")
	app := filepath.Join(ws, "app")
	g(root, "clone", "-q", origin, app)
	os.WriteFile(filepath.Join(app, "README.md"), []byte("hi\n"), 0o644)
	g(app, "add", ".")
	g(app, "commit", "-q", "-m", "init")
	g(app, "push", "-q", "origin", "HEAD:main")

	st, err := sqlite.Open(context.Background(), filepath.Join(root, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := NewServer(st, config.Default(), "/cfg", log)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := st.SaveWorkspace(context.Background(), store.Workspace{Name: "ws", Path: ws}, []store.Repo{{Name: "app", Path: app}})
	repos, _ := st.Repos(context.Background(), w.ID)
	opened, err := s.Fold.Open(context.Background(), fold.OpenRequest{RepoID: repos[0].ID, Name: "work", Scope: []string{"src/**"}})
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "agent")
	os.WriteFile(agent, []byte(fakeTokenAgent), 0o755)
	out := filepath.Join(root, "token.out")
	t.Setenv("TOKEN_OUT", out)
	s.Runner = &dispatch.Runner{Store: st, Config: config.Default(), Dir: filepath.Join(root, "runs"), Log: log}
	dispatch.SetLookPath(s.Runner, func(string) (string, error) { return agent, nil })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	s.addr.Store(&host)
	s.Runner.SetCredentials(s)

	var rv api.RunView
	if code := call(t, ts, s.Token, "POST", api.PathRuns, dispatch.StartRequest{LaneID: opened.Lane.ID, Agent: "cursor", Prompt: "go"}, &rv); code != http.StatusOK {
		t.Fatalf("start: %d", code)
	}
	s.Runner.Wait()
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != 4 || got[3] != "FILE_OK" || len(got[0]) != 64 || got[0] == s.Token || got[1] != ts.URL {
		t.Fatalf("agent's token and URL = %q", got)
	}
	tok, runFile := got[0], got[2]
	if code := call(t, ts, tok, "GET", api.PathStatus, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("token after its run ended: %d, want 401", code)
	}
	if _, err := os.Stat(runFile); !os.IsNotExist(err) {
		t.Errorf("run file %s left behind: %v", runFile, err)
	}
	run, _ := st.Run(context.Background(), rv.ID)
	runLog, _ := os.ReadFile(run.Log)
	if strings.Contains(string(runLog), tok) || !strings.Contains(string(runLog), "my token is [REDACTED]") {
		t.Errorf("run log shows the token:\n%s", runLog)
	}
	for _, secret := range []string{tok, s.Token, "inherited-operator-token-must-not-leak"} {
		if strings.Contains(logBuf.String(), secret) {
			t.Errorf("daemon log shows a token:\n%s", logBuf.String())
		}
	}
}
