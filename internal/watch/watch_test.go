package watch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

// fakeForge answers with whatever the test sets.
type fakeForge struct {
	mr   *forge.MR
	jobs []forge.Job
	// pipes are the pipelines by ref, for RefPipeline.
	pipes map[string]*forge.Pipeline
	// err, when set, is what MRForBranch and RefPipeline return; calls counts them.
	err   error
	calls int
}

func (f *fakeForge) Name() string { return "fake" }
func (f *fakeForge) MRForBranch(context.Context, string) (*forge.MR, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.mr == nil {
		return nil, nil
	}
	c := *f.mr
	return &c, nil
}
func (f *fakeForge) FailedJobs(context.Context, int64) ([]forge.Job, error) { return f.jobs, nil }
func (f *fakeForge) RefPipeline(_ context.Context, ref string) (*forge.Pipeline, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.pipes[ref], nil
}
func (f *fakeForge) CreateMR(context.Context, string, string, string, string) (*forge.MR, error) {
	return &forge.MR{IID: 1, State: "opened"}, nil
}
func (f *fakeForge) JobLog(_ context.Context, id int64, _ int) (string, error) {
	return "--- FAIL: TestParse (job log)", nil
}

const agent = `#!/bin/sh
echo "ARGS: $(printf '%s ' "$@" | tr '\n' ' ')"
echo fixed >> fix.txt && git add fix.txt && git commit -q -m fix
`

type fixture struct {
	w     *Watcher
	f     *fakeForge
	st    store.Store
	lane  store.Lane
	run   *dispatch.Runner
	ctx   context.Context
	agent string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent is a shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e"} {
		t.Setenv(k, v)
	}
	root, _ := paths.Canonical(t.TempDir())
	g := func(dir string, args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	origin := filepath.Join(root, "origin.git")
	g(root, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	g(root, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644)
	g(seed, "add", ".")
	g(seed, "commit", "-q", "-m", "init")
	g(seed, "push", "-q", "origin", "HEAD:main")
	ws := filepath.Join(root, "ws")
	os.MkdirAll(ws, 0o755)
	g(ws, "clone", "-q", origin, filepath.Join(ws, "app"))

	st, err := sqlite.Open(context.Background(), filepath.Join(root, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	w, _ := st.SaveWorkspace(ctx, store.Workspace{Name: "ws", Path: ws}, []store.Repo{{Name: "app", Path: filepath.Join(ws, "app"), Remote: "git@gl.example.com:g/app.git"}})
	repos, _ := st.Repos(ctx, w.ID)
	cfg := config.Default()
	fo := &fold.Fold{Store: st, Config: cfg}
	opened, err := fo.Open(ctx, fold.OpenRequest{RepoID: repos[0].ID, Name: "feat/x", Scope: []string{"**"}})
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "agent")
	os.WriteFile(bin, []byte(agent), 0o755)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &dispatch.Runner{Store: st, Config: cfg, Dir: filepath.Join(root, "runs"), Log: log}
	dispatch.SetLookPath(r, func(string) (string, error) { return bin, nil })
	ff := &fakeForge{}
	wa := &Watcher{Store: st, Fold: fo, Runner: r, Log: log, Interval: time.Hour,
		ForgeFor: func(string) (forge.Forge, error) { return ff, nil }}
	t.Cleanup(r.Wait) // let runs and what they set off finish before the repo is removed
	return &fixture{w: wa, f: ff, st: st, lane: opened.Lane, run: r, ctx: ctx, agent: bin}
}

func (f *fixture) feed(t *testing.T) string {
	t.Helper()
	items, _ := f.st.Feed(f.ctx, 0, 500)
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.Kind + ": " + it.Text + "\n")
	}
	return b.String()
}

func (f *fixture) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := f.st.Runs(f.ctx, 0, store.RunRunning, 1); len(r) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("runs did not end")
}

func TestNoMRMeansNothing(t *testing.T) {
	f := newFixture(t)
	f.w.Check(f.ctx)
	if got := f.feed(t); strings.Contains(got, "mr:") {
		t.Errorf("feed = %s", got)
	}
}

func TestFailedPipelineGoesBackToTheAgent(t *testing.T) {
	f := newFixture(t)
	// The lane has an agent conversation.
	first, err := f.run.Start(f.ctx, dispatch.StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "build it"})
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t)

	f.f.mr = &forge.MR{IID: 7, State: "opened", URL: "https://gl/mr/7", Pipeline: &forge.Pipeline{ID: 100, Status: "running"}}
	f.w.Check(f.ctx)
	if !strings.Contains(f.feed(t), "!7 opened for lane feat/x") {
		t.Fatalf("feed = %s", f.feed(t))
	}

	f.f.mr.Pipeline = &forge.Pipeline{ID: 100, Status: "failed", URL: "https://gl/p/100"}
	f.f.jobs = []forge.Job{{ID: 1, Name: "go-check", URL: "https://gl/j/1"}}
	f.w.Check(f.ctx)
	f.settle(t)
	runs, _ := f.st.Runs(f.ctx, f.lane.ID, "", 10)
	fix := runs[0]
	if fix.Parent != first.ID || !strings.Contains(fix.Prompt, "--- FAIL: TestParse (job log)") || !strings.Contains(fix.Prompt, "!7 failed") {
		t.Errorf("fix run = %+v", fix)
	}
	if !strings.Contains(f.feed(t), "pipeline: pipeline ") || !strings.Contains(f.feed(t), "asked claude to fix it") {
		t.Errorf("feed = %s", f.feed(t))
	}
	// The same failed pipeline seen again changes nothing.
	f.w.Check(f.ctx)
	if n, _ := f.st.Runs(f.ctx, f.lane.ID, "", 10); len(n) != 2 {
		t.Errorf("acted twice on one pipeline: %d runs", len(n))
	}
	// A second failure gets the second try; a third goes to the person.
	f.f.mr.Pipeline = &forge.Pipeline{ID: 101, Status: "failed"}
	f.w.Check(f.ctx)
	f.settle(t)
	f.f.mr.Pipeline = &forge.Pipeline{ID: 102, Status: "failed"}
	f.w.Check(f.ctx)
	if n, _ := f.st.Runs(f.ctx, f.lane.ID, "", 10); len(n) != 3 {
		t.Errorf("runs = %d, want 3 (two fixes)", len(n))
	}
	if !strings.Contains(f.feed(t), "lane feat/x is yours now") {
		t.Errorf("feed = %s", f.feed(t))
	}
	if lf, _ := f.st.LaneForge(f.ctx, f.lane.ID); lf.FixTries != 2 {
		t.Errorf("fix tries = %d", lf.FixTries)
	}
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestMergeClosesTheLaneOnProof(t *testing.T) {
	f := newFixture(t)
	head := headOf(t, f.lane.Worktree)
	f.f.mr = &forge.MR{IID: 7, State: "opened", SHA: head}
	f.w.Check(f.ctx)
	f.f.mr = &forge.MR{IID: 7, State: "merged", SHA: head, MergeSHA: "abcdef1234567"}
	f.w.Check(f.ctx)
	lane, _ := f.st.Lane(f.ctx, f.lane.ID)
	if lane.State != store.LaneClosed {
		t.Fatalf("lane = %+v", lane)
	}
	if _, err := os.Stat(f.lane.Worktree); !os.IsNotExist(err) {
		t.Error("worktree left behind")
	}
	if !strings.Contains(f.feed(t), "!7 merged at abcdef123; lane feat/x closed, local branch deleted") {
		t.Errorf("feed = %s", f.feed(t))
	}
}

func TestMergeLeavesUncommittedWorkAlone(t *testing.T) {
	f := newFixture(t)
	head := headOf(t, f.lane.Worktree)
	f.f.mr = &forge.MR{IID: 8, State: "opened", SHA: head}
	f.w.Check(f.ctx)
	os.WriteFile(filepath.Join(f.lane.Worktree, "wip.txt"), []byte("mine"), 0o644)
	f.f.mr = &forge.MR{IID: 8, State: "merged", SHA: head, MergeSHA: "abc"}
	f.w.Check(f.ctx)
	if lane, _ := f.st.Lane(f.ctx, f.lane.ID); lane.State != store.LaneOpen {
		t.Error("closed a lane with uncommitted work")
	}
	if !strings.Contains(f.feed(t), "cannot close") || !strings.Contains(f.feed(t), "uncommitted") {
		t.Errorf("feed = %s", f.feed(t))
	}
	// Without a merge commit there is no proof yet.
	g := newFixture(t)
	g.f.mr = &forge.MR{IID: 9, State: "opened"}
	g.w.Check(g.ctx)
	g.f.mr = &forge.MR{IID: 9, State: "merged"}
	g.w.Check(g.ctx)
	if lane, _ := g.st.Lane(g.ctx, g.lane.ID); lane.State != store.LaneOpen {
		t.Error("closed without a merge commit")
	}
}

// The incident of 2026-10-02: imported lanes whose requests had merged weeks before were
// closed on the first poll, removing other sessions' worktrees.
func TestAlreadyMergedOnFirstSightIsReportedNotClosed(t *testing.T) {
	f := newFixture(t)
	head := headOf(t, f.lane.Worktree)
	f.f.mr = &forge.MR{IID: 107, State: "merged", SHA: head, MergeSHA: "abc123"}
	f.w.Check(f.ctx)
	f.w.Check(f.ctx)
	if lane, _ := f.st.Lane(f.ctx, f.lane.ID); lane.State != store.LaneOpen {
		t.Fatal("closed a lane whose request was merged before Shepherd watched it")
	}
	if _, err := os.Stat(f.lane.Worktree); err != nil {
		t.Fatal("the worktree is gone")
	}
	if !strings.Contains(f.feed(t), "was already merged when Shepherd first looked") {
		t.Errorf("feed = %s", f.feed(t))
	}
}

func TestMergeLeavesWorkBeyondTheRequestAlone(t *testing.T) {
	f := newFixture(t)
	head := headOf(t, f.lane.Worktree)
	f.f.mr = &forge.MR{IID: 10, State: "opened", SHA: head}
	f.w.Check(f.ctx)
	// A commit after the request's head: work the merge does not cover.
	cmd := exec.Command("sh", "-c", "echo more > more.txt && git add more.txt && git commit -q -m more")
	cmd.Dir = f.lane.Worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	f.f.mr = &forge.MR{IID: 10, State: "merged", SHA: head, MergeSHA: "abc"}
	f.w.Check(f.ctx)
	if lane, _ := f.st.Lane(f.ctx, f.lane.ID); lane.State != store.LaneOpen {
		t.Fatal("closed a lane with commits beyond its merged request")
	}
	if !strings.Contains(f.feed(t), "commits beyond its merged request") {
		t.Errorf("feed = %s", f.feed(t))
	}
}
