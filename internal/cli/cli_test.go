package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/daemon"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

type harness struct {
	env      Env
	out, err *bytes.Buffer
	srv      *daemon.Server
}

// newHarness starts a daemon on an httptest server and points a CLI Env at it.
func newHarness(t *testing.T, stdin string, interactive bool) *harness {
	t.Helper()
	home := t.TempDir()
	st, err := sqlite.Open(context.Background(), filepath.Join(home, "shepherd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := daemon.NewServer(st, config.Default(), "cfg", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Runner = &dispatch.Runner{Store: st, Config: config.Default(), Dir: filepath.Join(home, "runs"), Log: srv.Log}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	l := paths.Layout{Home: home}
	rt, _ := json.Marshal(api.Runtime{Addr: strings.TrimPrefix(ts.URL, "http://"), Token: srv.Token})
	if err := os.WriteFile(l.Runtime(), rt, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, srv: srv}
	h.env = Env{Stdin: strings.NewReader(stdin), Stdout: h.out, Stderr: h.err, Interactive: interactive, Layout: l, Cwd: home}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.err.Reset()
	return Run(context.Background(), h.env, args)
}

func makeWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, _ := paths.Canonical(t.TempDir())
	for name, remote := range map[string]string{"app": "git@example.com:t/app.git", "lib": "https://example.com/t/lib", "scratch": ""} {
		dir := filepath.Join(root, name)
		os.MkdirAll(dir, 0o755)
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
		if remote != "" {
			exec.Command("git", "-C", dir, "remote", "add", "origin", remote).Run()
		}
	}
	return root
}

func TestInitYesThenWhereAndStatus(t *testing.T) {
	h := newHarness(t, "", false)
	root := makeWorkspace(t)

	if code := h.run("init", root, "--yes"); code != 0 {
		t.Fatalf("init: %d %s", code, h.err)
	}
	if !strings.Contains(h.out.String(), "2 repos managed") || strings.Contains(h.out.String(), "scratch") {
		t.Errorf("init output:\n%s", h.out)
	}

	h.env.Cwd = filepath.Join(root, "app")
	if code := h.run("where"); code != 0 || !strings.Contains(h.out.String(), "repo       app") {
		t.Errorf("where: %d\n%s%s", code, h.out, h.err)
	}
	if code := h.run("where", "--json", root); code != 0 {
		t.Fatalf("where --json: %d %s", code, h.err)
	}
	var res api.Resolution
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil || res.Workspace == nil || res.Repo != nil {
		t.Errorf("where --json at root: %+v %v", res, err)
	}
	if code := h.run("status"); code != 0 || !strings.Contains(h.out.String(), "2 repos, 0 lanes") {
		t.Errorf("status: %d\n%s", code, h.out)
	}
}

func TestInitInteractiveToggle(t *testing.T) {
	// Repos sort as app, lib, scratch. Untick app, tick scratch, then accept.
	h := newHarness(t, "1 3\n\n", true)
	root := makeWorkspace(t)
	if code := h.run("init", root, "--name", "mine"); code != 0 {
		t.Fatalf("init: %d %s", code, h.err)
	}
	out := h.out.String()
	tail := out[strings.LastIndex(out, "Workspace mine"):]
	if !strings.Contains(tail, "lib") || !strings.Contains(tail, "scratch") || strings.Contains(tail, "app") {
		t.Errorf("selection wrong:\n%s", tail)
	}
}

func TestInitOnlyAndRerunKeepsManaged(t *testing.T) {
	h := newHarness(t, "", false)
	root := makeWorkspace(t)
	if code := h.run("init", root, "--only", "scratch"); code != 0 {
		t.Fatalf("init --only: %d %s", code, h.err)
	}
	if code := h.run("init", root, "--only", "nope"); code != 1 || !strings.Contains(h.err.String(), `no repo "nope"`) {
		t.Errorf("unknown --only: %d %s", code, h.err)
	}
	// Running --yes later keeps scratch, which is managed although not suggested.
	if code := h.run("init", root, "--yes"); code != 0 || !strings.Contains(h.out.String(), "3 repos managed") {
		t.Errorf("rerun: %d\n%s%s", code, h.out, h.err)
	}
}

func TestInitRefusesWithoutTerminal(t *testing.T) {
	h := newHarness(t, "", false)
	root := makeWorkspace(t)
	if code := h.run("init", root); code != 1 || !strings.Contains(h.err.String(), "not a terminal") {
		t.Errorf("non-interactive init: %d %s", code, h.err)
	}
}

func TestUsageAndErrors(t *testing.T) {
	h := newHarness(t, "", false)
	if code := h.run(); code != 0 || !strings.Contains(h.out.String(), "Commands:") {
		t.Errorf("no args: %d", code)
	}
	if code := h.run("bogus"); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	if code := h.run("init", "--yes", "--all"); code != 2 {
		t.Errorf("conflicting flags: %d", code)
	}
	if code := h.run("status", "--nope"); code != 2 {
		t.Errorf("unknown flag: %d", code)
	}
	if code := h.run("version"); code != 0 || !strings.HasPrefix(h.out.String(), "shepherd ") {
		t.Errorf("version: %d %q", code, h.out)
	}
}

func TestNoDaemon(t *testing.T) {
	env := Env{Stdout: io.Discard, Stderr: &bytes.Buffer{}, Layout: paths.Layout{Home: t.TempDir()}, Cwd: t.TempDir()}
	if code := Run(context.Background(), env, []string{"status"}); code != 1 ||
		!strings.Contains(env.Stderr.(*bytes.Buffer).String(), "not running") {
		t.Errorf("status without daemon: %d", code)
	}
}

func TestParseSelection(t *testing.T) {
	got, err := parseSelection("3 1-2, 5", 5)
	if err != nil || len(got) != 4 || got[0] != 2 || got[1] != 0 || got[3] != 4 {
		t.Errorf("parseSelection = %v, %v", got, err)
	}
	for _, bad := range []string{"0", "6", "x", "3-1", "1-x"} {
		if _, err := parseSelection(bad, 5); err == nil {
			t.Errorf("parseSelection(%q) accepted", bad)
		}
	}
}

// laneWorkspace is a workspace with one repo, app, cloned from a bare origin with a
// commit on main.
func laneWorkspace(t *testing.T) (root, app string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, _ := paths.Canonical(t.TempDir())
	g := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	origin := filepath.Join(base, "origin.git")
	g(base, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(base, "seed")
	g(base, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "f"), []byte("x"), 0o644)
	g(seed, "add", ".")
	g(seed, "commit", "-q", "-m", "init")
	g(seed, "push", "-q", "origin", "HEAD:main")
	root = filepath.Join(base, "ws")
	os.MkdirAll(root, 0o755)
	app = filepath.Join(root, "app")
	g(root, "clone", "-q", origin, app)
	return root, app
}

func TestLaneOpenListCloseFromCLI(t *testing.T) {
	h := newHarness(t, "", false)
	root, app := laneWorkspace(t)
	if code := h.run("init", root, "--yes"); code != 0 {
		t.Fatalf("init: %s", h.err)
	}

	h.env.Cwd = app
	if code := h.run("lane", "open", "feat/x", "--scope", "src/**,docs/*.md"); code != 0 {
		t.Fatalf("lane open: %d %s", code, h.err)
	}
	wt := filepath.Join(root, "app-worktrees", "feat-x")
	if !strings.Contains(h.out.String(), "cd "+wt) {
		t.Errorf("open output:\n%s", h.out)
	}
	if code := h.run("lane", "open", "feat/x", "--scope", "x"); code != 1 || !strings.Contains(h.err.String(), "already") {
		t.Errorf("duplicate open: %d %s", code, h.err)
	}
	if code := h.run("lane", "open", "nope"); code != 1 || !strings.Contains(h.err.String(), "needs a scope") {
		t.Errorf("open without scope: %d %s", code, h.err)
	}

	// From the workspace root, list covers every repo; from the worktree, where knows the lane.
	h.env.Cwd = root
	if code := h.run("lane", "list"); code != 0 || !strings.Contains(h.out.String(), "feat/x") || !strings.Contains(h.out.String(), "src/**, docs/*.md") {
		t.Errorf("list: %d\n%s%s", code, h.out, h.err)
	}
	h.env.Cwd = wt
	if code := h.run("where"); code != 0 || !strings.Contains(h.out.String(), "lane       feat/x") {
		t.Errorf("where in lane:\n%s%s", h.out, h.err)
	}
	if code := h.run("lane", "list"); code != 0 || !strings.Contains(h.out.String(), "*feat/x") {
		t.Errorf("list marks the current lane:\n%s", h.out)
	}

	// No commits: the branch is trivially in origin/main, so close needs no force.
	if code := h.run("lane", "close"); code != 0 || !strings.Contains(h.out.String(), "deleted local branch feat/x") {
		t.Errorf("close: %d\n%s%s", code, h.out, h.err)
	}
	h.env.Cwd = root
	if code := h.run("lane", "list"); code != 0 || !strings.Contains(h.out.String(), "No open lanes") {
		t.Errorf("list after close:\n%s", h.out)
	}
	if code := h.run("fold", "gc"); code != 0 || !strings.Contains(h.out.String(), "No stale worktrees") {
		t.Errorf("gc: %d\n%s%s", code, h.out, h.err)
	}
}
