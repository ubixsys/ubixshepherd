package dispatch

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

// fakeAgent is a stand-in for an agent CLI: it commits a file in the scope, optionally
// one outside it, then tries to push and records whether that worked.
const fakeAgent = `#!/bin/sh
if [ "$1" = create-chat ]; then echo "11111111-2222-4333-8444-555555555555"; exit 0; fi
echo "fake agent in $(pwd), run $SHEPHERD_RUN, lane $SHEPHERD_LANE"
echo "ARGS: $(printf '%s ' "$@" | tr '\n' ' ')"
case "$MODE" in quick) [ -n "$CREDITS" ] && echo "AI Credits $CREDITS (13s)"; [ -n "$COST" ] && echo "{\"type\":\"result\",\"subtype\":\"success\",\"total_cost_usd\":$COST}"; echo "copilot --resume=cop-$SHEPHERD_RUN-session"; exit 0 ;; esac
case "$MODE" in sleep) sleep 30 ;; esac
case "$MODE" in limit) echo "ActionRequiredError: You've hit your usage limit. Upgrade to continue."; exit 1 ;; esac
case "$MODE" in mention) echo "fixing: You've hit your usage limit in the error text"; seq 1 30; exit 1 ;; esac
mkdir -p src && echo "work $SHEPHERD_RUN" >> src/work.txt
git add src/work.txt && git commit -q -m "agent work"
if [ "$MODE" = stray ]; then echo x > stray.txt && git add stray.txt && git commit -q -m stray; fi
if git push -q --no-verify origin HEAD 2>/dev/null; then echo "PUSH WORKED"; else echo "push blocked"; fi
echo "token glpat-AbCdEfGhIjKlMnOpQrStUv in output"
[ "$MODE" = fail ] && exit 3
exit 0
`

type fixture struct {
	runner *Runner
	st     store.Store
	lane   store.Lane
	origin string
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent is a shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("MODE", mode)
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
	w, _ := st.SaveWorkspace(context.Background(), store.Workspace{Name: "ws", Path: ws}, []store.Repo{{Name: "app", Path: filepath.Join(ws, "app")}})
	repos, _ := st.Repos(context.Background(), w.ID)
	cfg := config.Default()
	f := &fold.Fold{Store: st, Config: cfg}
	opened, err := f.Open(context.Background(), fold.OpenRequest{RepoID: repos[0].ID, Name: "work", Scope: []string{"src/**"}})
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "fake-agent")
	os.WriteFile(agent, []byte(fakeAgent), 0o755)
	r := &Runner{
		Store: st, Config: cfg, Dir: filepath.Join(root, "runs"),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookPath: func(string) (string, error) { return agent, nil },
	}
	// Runs, and what they set off, can outlive a test's last check: let them finish
	// before the repo is removed (cleanups run last-registered first).
	t.Cleanup(func() {
		for _, run := range mustRuns(st) {
			if run.State == store.RunRunning {
				r.Stop(context.Background(), run.ID)
			}
		}
		r.Wait()
	})
	return &fixture{runner: r, st: st, lane: opened.Lane, origin: origin}
}

func (f *fixture) wait(t *testing.T, id int64) store.Run {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		run, err := f.st.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != store.RunRunning {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run did not end")
	return store.Run{}
}

func TestRunRecordsOutcomeAndBlocksPush(t *testing.T) {
	f := newFixture(t, "ok")
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "do the work"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	if done.State != store.RunSucceeded || done.Commits != 1 || len(done.Outside) != 0 || *done.ExitCode != 0 {
		t.Errorf("run = %+v", done)
	}
	log, _ := os.ReadFile(done.Log)
	for _, want := range []string{"fake agent in " + f.lane.Worktree, "lane work", "push blocked", "[REDACTED]", "succeeded (exit 0) with 1 commit"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(string(log), "glpat-") {
		t.Error("log not redacted")
	}
	if out, _ := exec.Command("git", "-C", f.origin, "branch", "--list", "work").Output(); len(out) > 0 {
		t.Error("the agent's push reached origin")
	}
}

func TestRunFlagsWorkOutsideScopeAndFailures(t *testing.T) {
	f := newFixture(t, "stray")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "copilot", Prompt: "x"})
	done := f.wait(t, run.ID)
	if done.Commits != 2 || len(done.Outside) != 1 || done.Outside[0] != "stray.txt" {
		t.Errorf("stray run = %+v", done)
	}

	f2 := newFixture(t, "fail")
	run, _ = f2.runner.Start(context.Background(), StartRequest{LaneID: f2.lane.ID, Agent: "cursor", Prompt: "x"})
	if done := f2.wait(t, run.ID); done.State != store.RunFailed || *done.ExitCode != 3 {
		t.Errorf("failing run = %+v", done)
	}
}

func TestRunRefusals(t *testing.T) {
	f := newFixture(t, "sleep")
	ctx := context.Background()
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "gpt-9", Prompt: "x"}); !errors.Is(err, ErrRefused) {
		t.Errorf("unknown agent: %v", err)
	}
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "  "}); !errors.Is(err, ErrRefused) {
		t.Errorf("empty task: %v", err)
	}
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "copilot", Prompt: "y"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "one agent per lane") {
		t.Errorf("second run in lane: %v", err)
	}
	// The lane cannot be closed, nor pushed from, while the agent runs.
	fo := &fold.Fold{Store: f.st, Config: f.runner.Config}
	if _, err := fo.Close(ctx, f.lane.ID, true); !errors.Is(err, fold.ErrRefused) {
		t.Errorf("close during run: %v", err)
	}
	if v, _ := fo.CheckPush(ctx, &f.lane, nil, f.lane.Worktree, nil); v.OK {
		t.Error("push allowed during a run")
	}

	if err := f.runner.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(t, run.ID); done.State != store.RunStopped {
		t.Errorf("stopped run = %+v", done)
	}
}

func TestMaxRuns(t *testing.T) {
	f := newFixture(t, "sleep")
	f.runner.Config.Daemon.MaxRuns = 1
	ctx := context.Background()
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.runner.Stop(ctx, run.ID); f.wait(t, run.ID) }()
	// A second lane, so only the machine-wide limit can refuse it.
	other, err := (&fold.Fold{Store: f.st, Config: f.runner.Config}).Open(ctx, fold.OpenRequest{RepoID: f.lane.RepoID, Name: "other", Scope: []string{"docs/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: other.ID, Agent: "claude", Prompt: "y"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "max_runs") {
		t.Errorf("over the limit: %v", err)
	}
}

func TestSetConfigWhileRunsGo(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	// SetConfig wins over the field, and may come while runs start and end.
	one := config.Default()
	one.Daemon.MaxRuns = 1
	f.runner.SetConfig(one)
	busy, _ := f.st.CreateRun(ctx, store.Run{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", State: store.RunRunning, Log: "l"})
	other := openLane(t, f, "other", "docs/**")
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: other.ID, Agent: "claude", Prompt: "y"}); !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "max_runs") {
		t.Errorf("SetConfig's limit not applied: %v", err)
	}
	busy.State = store.RunSucceeded
	f.st.UpdateRun(ctx, busy)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			c := config.Default()
			c.Daemon.MaxRuns = 2 + i%2
			f.runner.SetConfig(c)
		}
	}()
	for i := 0; i < 3; i++ {
		run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "z"})
		if err != nil {
			t.Fatal(err)
		}
		f.wait(t, run.ID)
	}
	<-done
}

func TestShutdownInterruptsAndSetsNothingOff(t *testing.T) {
	f := newFixture(t, "sleep")
	ctx := context.Background()
	pushes, _ := config.Parse([]byte("repos:\n  app:\n    gate: \"true\"\n    autonomy:\n      push: shepherd\n"))
	f.runner.SetConfig(pushes)
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: run.ID, Kind: KindReview, Message: "review me"})
	time.Sleep(200 * time.Millisecond) // the agent is asleep in its run
	stop, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f.runner.Shutdown(stop)

	got, _ := f.st.Run(ctx, run.ID)
	if got.State != store.RunInterrupted || !strings.Contains(got.Error, "daemon stopped") {
		t.Errorf("run after shutdown = %+v", got)
	}
	// The run's end routed nothing: the review still waits on the asker's first turn.
	if r, _ := f.st.Request(ctx, q.ID); r.State != store.RequestPending || strings.Contains(r.Note, "stopping") {
		t.Errorf("request after shutdown = %+v", r)
	}
	items, _ := f.st.Feed(ctx, 0, 100)
	for _, it := range items {
		if strings.Contains(it.Text, "gate") {
			t.Errorf("shutdown set a ship off: %s", it.Text)
		}
	}
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "y"}); !errors.Is(err, ErrHeld) {
		t.Errorf("start while stopping: %v", err)
	}
}

func TestOutOfQuotaHoldsTheAgent(t *testing.T) {
	f := newFixture(t, "limit")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "cursor", Prompt: "x"})
	done := f.wait(t, run.ID)
	if done.State != store.RunFailed || !strings.Contains(done.Error, "out of quota: ActionRequiredError: You've hit your usage limit") {
		t.Errorf("run = %+v", done)
	}
	waitFeed(t, f, fmt.Sprintf("run %d: cursor is out of quota", run.ID))
	// The next cursor run is refused with the reason, at once; another agent still goes.
	if _, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "cursor", Prompt: "y"}); !errors.Is(err, ErrHeld) ||
		!strings.Contains(err.Error(), fmt.Sprintf("cursor is out of quota (run %d", run.ID)) {
		t.Errorf("cursor after its limit: %v", err)
	}
	other, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "y"})
	if err != nil {
		t.Fatalf("claude held for cursor's limit: %v", err)
	}
	f.wait(t, other.ID)
	// The hold lifts when the limit does.
	f.runner.mu.Lock()
	q := f.runner.outOf["cursor"]
	q.Until = time.Now().Add(-time.Second)
	f.runner.outOf["cursor"] = q
	f.runner.mu.Unlock()
	f.runner.mu.Lock()
	why := f.runner.quotaHeld("cursor")
	f.runner.mu.Unlock()
	if why != "" {
		t.Errorf("hold outlived its limit: %s", why)
	}
}

func TestUsageLimitWordsInTheWorkDoNotCount(t *testing.T) {
	f := newFixture(t, "mention")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "cursor", Prompt: "x"})
	if done := f.wait(t, run.ID); strings.Contains(done.Error, "quota") {
		t.Errorf("a failed run that only mentioned a limit is out of quota: %+v", done)
	}
	if _, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "cursor", Prompt: "y"}); errors.Is(err, ErrHeld) {
		t.Errorf("cursor held: %v", err)
	}
}

func TestLimitParsers(t *testing.T) {
	ev := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":4102444800,"rateLimitType":"five_hour"}}`
	if l, ok := claudeLimit(ev); !ok || !l.Sure || l.Until.Unix() != 4102444800 || !strings.Contains(l.Text, "five hour") {
		t.Errorf("claude event = %+v %v", l, ok)
	}
	if _, ok := claudeLimit(strings.Replace(ev, "rejected", "allowed_warning", 1)); ok {
		t.Error("a warning is not a limit")
	}
	if l, ok := claudeLimit(`{"type":"result","is_error":true,"result":"Claude AI usage limit reached|4102444800"}`); !ok || l.Sure || l.Until.Unix() != 4102444800 {
		t.Errorf("claude words = %+v %v", l, ok)
	}
	if l, ok := claudeLimit(`{"type":"result","is_error":true,"result":"You've hit your limit · resets 3pm"}`); !ok || !strings.HasPrefix(l.Text, "You've hit your limit") {
		t.Errorf("claude words = %+v %v", l, ok)
	}
	c, _ := AdapterFor("copilot")
	if _, ok := c.Limit("Error: You have exceeded your premium requests allowance."); !ok {
		t.Error("copilot premium requests")
	}
	if _, ok := c.Limit("Total usage est: 1 Premium request"); ok {
		t.Error("copilot's usage line is not a limit")
	}
}

func TestRecoverMarksInterrupted(t *testing.T) {
	f := newFixture(t, "ok")
	ctx := context.Background()
	stale, _ := f.st.CreateRun(ctx, store.Run{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", State: store.RunRunning, Log: "l"})
	if err := f.runner.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.Run(ctx, stale.ID); got.State != store.RunInterrupted || got.Ended == nil {
		t.Errorf("after recover = %+v", got)
	}
}

func TestAdapterArgs(t *testing.T) {
	a, _ := AdapterFor("claude")
	args := a.Args(Opts{Prompt: "PROMPT", Model: "opus", Gate: "make check", Worktree: "/w", Session: "S1"})
	if args[0] != "-p" || args[1] != "PROMPT" {
		t.Errorf("claude: the prompt must follow -p before the tool lists: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--disallowedTools Bash(git push:*)", "Bash(make check:*)", "--model opus", "--permission-mode auto", "--session-id S1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args lack %q: %v", want, args)
		}
	}
	if j := strings.Join(a.Args(Opts{Prompt: "P", Worktree: "/w", Session: "S1", Resume: true}), " "); !strings.Contains(j, "--resume S1") || strings.Contains(j, "--session-id") {
		t.Errorf("claude resume args: %s", j)
	}
	c, _ := AdapterFor("copilot")
	if j := strings.Join(c.Args(Opts{Prompt: "P", Gate: "make check", Worktree: "/w"}), " "); !strings.Contains(j, "--deny-tool shell(git push)") || !strings.Contains(j, "--allow-all-tools") || strings.Contains(j, "--resume") {
		t.Errorf("copilot args: %s", j)
	}
	if j := strings.Join(c.Args(Opts{Prompt: "P", Gate: "make check", Worktree: "/w", Mode: config.PermAcceptEdits}), " "); !strings.Contains(j, "--deny-tool shell(git push)") || !strings.Contains(j, "shell(make)") || strings.Contains(j, "--allow-all-tools") {
		t.Errorf("copilot args: %s", j)
	}
	if j := strings.Join(c.Args(Opts{Prompt: "P", Worktree: "/w", Session: "S2", Resume: true}), " "); !strings.Contains(j, "--resume=S2") {
		t.Errorf("copilot resume args: %s", j)
	}
	if got := c.SessionIn("Resume     copilot --resume=4dcd900f-729e-4b4f"); got != "4dcd900f-729e-4b4f" {
		t.Errorf("copilot session from output = %q", got)
	}
	cu, _ := AdapterFor("cursor")
	if j := strings.Join(cu.Args(Opts{Prompt: "P", Worktree: "/w", Session: "C1"}), " "); !strings.Contains(j, "--workspace /w") || !strings.Contains(j, "--resume C1") {
		t.Errorf("cursor args: %s", j)
	}
	if id, _ := newUUID(); len(id) != 36 || id[14] != '4' {
		t.Errorf("uuid = %q", id)
	}
	if b := Brief("fix it", "l", "r", "l", "main", "/w", []string{"src/**"}, "make check", Powers{}, false, ""); !strings.Contains(b, "Do not push") || !strings.Contains(b, "Do not merge") || !strings.Contains(b, "src/**") || !strings.HasSuffix(b, "fix it\n") {
		t.Errorf("brief:\n%s", b)
	}
}

func TestAgentPowersArgsAndBrief(t *testing.T) {
	a, _ := AdapterFor("claude")
	j := strings.Join(a.Args(Opts{Prompt: "P", Mode: config.PermAcceptEdits, Push: true, Merge: true}), " ")
	for _, want := range []string{"--permission-mode acceptEdits", "Bash(git push:*)", "Bash(glab mr merge:*)"} {
		if !strings.Contains(j, want) {
			t.Errorf("claude args lack %q: %s", want, j)
		}
	}
	if strings.Contains(j, "--disallowedTools") {
		t.Errorf("claude denies push to an agent allowed to: %s", j)
	}
	if j := strings.Join(a.Args(Opts{Prompt: "P"}), " "); strings.Contains(j, "glab") {
		t.Errorf("claude may merge without the repo allowing it: %s", j)
	}
	c, _ := AdapterFor("copilot")
	if j := strings.Join(c.Args(Opts{Prompt: "P", Push: true}), " "); strings.Contains(j, "--deny-tool") {
		t.Errorf("copilot denies push to an agent allowed to: %s", j)
	}

	shep := Brief("t", "l", "r", "feat/x", "dev", "/w", []string{"x"}, "make check", Powers{Push: config.Shepherd}, false, "")
	if !strings.Contains(shep, "Shepherd runs the gate itself") || !strings.Contains(shep, "Do not push") {
		t.Errorf("shepherd-push brief:\n%s", shep)
	}
	own := Brief("t", "l", "r", "feat/x", "dev", "/w", []string{"x"}, "", Powers{Push: config.Agent, Merge: true, GitLab: true, Forbid: []string{"(?i)co-authored-by"}}, false, "")
	for _, want := range []string{"git push -u origin feat/x -o merge_request.create -o merge_request.target=dev", "co-authored-by",
		"glab mr merge <iid> --when-pipeline-succeeds --sha", "Never approve"} {
		if !strings.Contains(own, want) {
			t.Errorf("agent-push brief lacks %q:\n%s", want, own)
		}
	}
	if strings.Contains(own, "Do not push") || strings.Contains(own, "Do not merge") {
		t.Errorf("agent-push brief forbids what the repo allows:\n%s", own)
	}
	if p := powers(config.Profile{Autonomy: config.Autonomy{Push: config.Agent, Merge: config.Agent}}, "git@github.com:o/r.git"); p.Merge || p.GitLab {
		t.Errorf("merge armed off GitLab: %+v", p)
	}
	if p := powers(config.Profile{Autonomy: config.Autonomy{Merge: config.Agent}}, "git@gitlab.example.com:o/r.git"); !p.Merge || !p.GitLab {
		t.Errorf("merge on GitLab: %+v", p)
	}
}

func TestAgentPushLiftsTheBlock(t *testing.T) {
	f := newFixture(t, "ok")
	p := f.runner.Config.Defaults
	p.Autonomy.Push = config.Agent
	f.runner.Config.Defaults = p
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "do the work"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	log, _ := os.ReadFile(done.Log)
	if !strings.Contains(string(log), "PUSH WORKED") || strings.Contains(string(log), "--disallowedTools") {
		t.Errorf("agent push was blocked:\n%s", log)
	}
}

func argsOf(t *testing.T, run store.Run) string {
	t.Helper()
	b, _ := os.ReadFile(run.Log)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "ARGS: ") {
			return line
		}
	}
	t.Fatalf("no ARGS line in:\n%s", b)
	return ""
}

func TestLaneKeepsItsConversation(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	first, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "first task"})
	if err != nil {
		t.Fatal(err)
	}
	first = f.wait(t, first.ID)
	if first.Session == "" || first.Parent != 0 {
		t.Fatalf("first run = %+v", first)
	}
	if a := argsOf(t, first); !strings.Contains(a, "--session-id "+first.Session) || !strings.Contains(a, "Do not push") {
		t.Errorf("first run args: %s", a)
	}

	// The next run in the lane with the same agent continues the session, without the brief.
	second, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "now the tests"})
	second = f.wait(t, second.ID)
	if second.Session != first.Session || second.Parent != first.ID {
		t.Errorf("second run = %+v", second)
	}
	if a := argsOf(t, second); !strings.Contains(a, "--resume "+first.Session) || strings.Contains(a, "Do not push") {
		t.Errorf("second run args: %s", a)
	}

	// new_session starts fresh; continue names the run.
	fresh, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", NewSession: true})
	fresh = f.wait(t, fresh.ID)
	if fresh.Session == first.Session || fresh.Parent != 0 {
		t.Errorf("new session run = %+v", fresh)
	}
	again, err := f.runner.Start(ctx, StartRequest{Continue: first.ID, Prompt: "back to the first one"})
	if err != nil {
		t.Fatal(err)
	}
	if again = f.wait(t, again.ID); again.Session != first.Session || again.Agent != "claude" || again.Parent != first.ID {
		t.Errorf("explicit continue = %+v", again)
	}
	if _, err := f.runner.Start(ctx, StartRequest{Continue: first.ID, Agent: "copilot", Prompt: "x"}); !errors.Is(err, ErrRefused) {
		t.Errorf("continue with another agent: %v", err)
	}
}

func TestSessionLearnedFromOutput(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "copilot", Prompt: "x"})
	run = f.wait(t, run.ID)
	want := fmt.Sprintf("cop-%d-session", run.ID)
	if run.Session != want {
		t.Fatalf("copilot session = %q, want %q", run.Session, want)
	}
	next, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "copilot", Prompt: "y"})
	next = f.wait(t, next.ID)
	if a := argsOf(t, next); !strings.Contains(a, "--resume="+want) {
		t.Errorf("copilot continuation args: %s", a)
	}

	cur, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "cursor", Prompt: "z"})
	if cur = f.wait(t, cur.ID); cur.Session != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("cursor session from create-chat = %q", cur.Session)
	}
}

func TestContinueRefusals(t *testing.T) {
	f := newFixture(t, "sleep")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if _, err := f.runner.Start(ctx, StartRequest{Continue: run.ID, Prompt: "y"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "still going") {
		t.Errorf("continue a running run: %v", err)
	}
	f.runner.Stop(ctx, run.ID)
	f.wait(t, run.ID)
	old, _ := f.st.CreateRun(ctx, store.Run{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", State: store.RunSucceeded, Log: "l"})
	if _, err := f.runner.Start(ctx, StartRequest{Continue: old.ID, Prompt: "y"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no session") {
		t.Errorf("continue without a session: %v", err)
	}
}

func TestWorkerToolsInjected(t *testing.T) {
	a, _ := AdapterFor("claude")
	j := strings.Join(a.Args(Opts{Prompt: "P", Worktree: "/w", Worker: "/bin/shepherd"}), " ")
	for _, want := range []string{"--strict-mcp-config", `"command":"/bin/shepherd"`, `"args":["mcp","--worker"]`, "mcp__shepherd"} {
		if !strings.Contains(j, want) {
			t.Errorf("claude worker args lack %q: %s", want, j)
		}
	}
	if j := strings.Join(a.Args(Opts{Prompt: "P"}), " "); strings.Contains(j, "mcp") {
		t.Errorf("claude without worker mentions mcp: %s", j)
	}
	c, _ := AdapterFor("copilot")
	if j := strings.Join(c.Args(Opts{Prompt: "P", Worker: "/bin/shepherd"}), " "); !strings.Contains(j, "--additional-mcp-config") || !strings.Contains(j, `"tools":["*"]`) || !strings.Contains(j, "--allow-tool shepherd") {
		t.Errorf("copilot worker args: %s", j)
	}
	if b := Brief("t", "l", "r", "l", "main", "/w", []string{"x"}, "", Powers{}, true, ""); !strings.Contains(b, "ask_human") || !strings.Contains(b, "ask_shepherd") {
		t.Errorf("brief with tools:\n%s", b)
	}
}

func TestAnswerContinuesTheAsker(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	run = f.wait(t, run.ID)

	d, err := f.runner.Ask(ctx, store.Decision{RunID: run.ID, Question: "Raise the price?", Options: []string{"yes", "no"}, Recommendation: "no"})
	if err != nil || d.State != store.DecisionOpen {
		t.Fatalf("ask: %+v %v", d, err)
	}
	d, err = f.runner.Answer(ctx, d.ID, "no, keep it")
	if err != nil || d.AnswerRun == 0 {
		t.Fatalf("answer: %+v %v", d, err)
	}
	next := f.wait(t, d.AnswerRun)
	if next.Parent != run.ID || next.Session != run.Session {
		t.Errorf("answer run = %+v", next)
	}
	if a := argsOf(t, next); !strings.Contains(a, "no, keep it") || !strings.Contains(a, "--resume "+run.Session) {
		t.Errorf("answer prompt: %s", a)
	}
	if _, err := f.runner.Answer(ctx, d.ID, "again"); !errors.Is(err, ErrRefused) {
		t.Errorf("answering twice: %v", err)
	}
}

func TestAnswerWaitsForTheRunToEnd(t *testing.T) {
	f := newFixture(t, "sleep")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	d, _ := f.runner.Ask(ctx, store.Decision{RunID: run.ID, Question: "Delete the table?", Recommendation: "no"})
	d, err := f.runner.Answer(ctx, d.ID, "no")
	if err != nil || d.AnswerRun != 0 {
		t.Fatalf("answer during run: %+v %v", d, err)
	}
	os.Setenv("MODE", "quick") // the continuation should not sleep
	f.runner.Stop(ctx, run.ID)
	f.wait(t, run.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := f.st.Decision(ctx, d.ID); got.AnswerRun != 0 {
			f.wait(t, got.AnswerRun)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the held answer was never delivered")
}

func TestRecordValidates(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "w"})
	f.wait(t, run.ID)
	if _, err := f.runner.Record(ctx, store.Event{RunID: run.ID, Kind: EventReport, Status: "finished", Text: "x"}); !errors.Is(err, ErrRefused) {
		t.Errorf("bad status: %v", err)
	}
	e, err := f.runner.Record(ctx, store.Event{RunID: run.ID, Kind: EventReport, Status: "done", Text: "added tests, token glpat-AbCdEfGhIjKlMnOpQrStUv"})
	if err != nil || strings.Contains(e.Text, "glpat-") {
		t.Errorf("record: %+v %v", e, err)
	}
	events, _ := f.st.Events(ctx, run.ID)
	if len(events) != 1 || events[0].Status != "done" {
		t.Errorf("events = %+v", events)
	}
}

func TestSetupCursorKeepsOtherServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"mine":{"command":"x"}},"other":1}`), 0o644)
	if CursorWorkerReady() {
		t.Fatal("ready before setup")
	}
	changed, err := SetupCursor("/bin/shepherd")
	if err != nil || !changed || !CursorWorkerReady() {
		t.Fatalf("setup: %v %v", changed, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if !strings.Contains(string(b), `"mine"`) || !strings.Contains(string(b), `"other": 1`) {
		t.Errorf("other settings lost:\n%s", b)
	}
	if changed, _ := SetupCursor("/bin/shepherd"); changed {
		t.Error("second setup changed the file")
	}
	os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte("not json"), 0o644)
	if _, err := SetupCursor("/bin/shepherd"); err == nil {
		t.Error("setup overwrote an unreadable config")
	}
}

func mustRuns(st store.Store) []store.Run {
	runs, _ := st.Runs(context.Background(), 0, store.RunRunning, 100)
	return runs
}

// feedKind waits for a feed line containing s and returns its kind.
func feedKind(t *testing.T, f *fixture, s string) string {
	t.Helper()
	waitFeed(t, f, s)
	items, _ := f.st.Feed(context.Background(), 0, 500)
	for _, it := range items {
		if strings.Contains(it.Text, s) {
			return it.Kind
		}
	}
	return ""
}

func TestRunEndPostsOutcomeAndCommitKinds(t *testing.T) {
	f := newFixture(t, "ok")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	f.wait(t, run.ID)
	if k := feedKind(t, f, fmt.Sprintf("run %d: claude in lane work succeeded", run.ID)); k != store.FeedRunPassed {
		t.Errorf("passed run kind = %q", k)
	}
	if k := feedKind(t, f, fmt.Sprintf("run %d: 1 commit(s) in lane work, latest: ", run.ID)); k != store.FeedCommit {
		t.Errorf("commit kind = %q", k)
	}

	f2 := newFixture(t, "fail")
	run, _ = f2.runner.Start(context.Background(), StartRequest{LaneID: f2.lane.ID, Agent: "cursor", Prompt: "x"})
	f2.wait(t, run.ID)
	if k := feedKind(t, f2, fmt.Sprintf("run %d: cursor in lane work failed", run.ID)); k != store.FeedRunFailed {
		t.Errorf("failed run kind = %q", k)
	}
}

func TestRunQuotaAndInterruptedKinds(t *testing.T) {
	f := newFixture(t, "limit")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "cursor", Prompt: "x"})
	f.wait(t, run.ID)
	if k := feedKind(t, f, fmt.Sprintf("run %d: cursor is out of quota", run.ID)); k != store.FeedRunQuota {
		t.Errorf("quota kind = %q", k)
	}

	g := newFixture(t, "sleep")
	run, _ = g.runner.Start(context.Background(), StartRequest{LaneID: g.lane.ID, Agent: "claude", Prompt: "x"})
	time.Sleep(200 * time.Millisecond)
	stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	g.runner.Shutdown(stop)
	if k := feedKind(t, g, fmt.Sprintf("run %d: claude in lane work interrupted", run.ID)); k != store.FeedRunInterrupted {
		t.Errorf("interrupted kind = %q", k)
	}
}

// A run's model: the one asked for, else the repo's agent.model for its agent (a repo
// entry over the defaults'), else none, which leaves the CLI's default.
func TestModelFromProfile(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	start := func(agent, model string) store.Run {
		t.Helper()
		run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: agent, Model: model, Prompt: "x", NewSession: true})
		if err != nil {
			t.Fatal(err)
		}
		return f.wait(t, run.ID)
	}
	if run := start("claude", ""); run.Model != "" || strings.Contains(argsOf(t, run), "--model") {
		t.Errorf("no model anywhere: %q %s", run.Model, argsOf(t, run))
	}
	cfg := config.Default()
	cfg.Defaults.Agent.Model = map[string]string{"claude": "sonnet", "copilot": "gpt-5"}
	cfg.Repos = map[string]config.Profile{"app": {Agent: config.AgentOpts{Model: map[string]string{"claude": "opus"}}}}
	f.runner.SetConfig(cfg)
	if run := start("claude", ""); run.Model != "opus" || !strings.Contains(argsOf(t, run), "--model opus") {
		t.Errorf("repo's agent.model: %q %s", run.Model, argsOf(t, run))
	}
	if run := start("claude", "haiku"); run.Model != "haiku" {
		t.Errorf("an explicit model must win: %q", run.Model)
	}
	if got := cfg.Profile("app").Agent.Model["copilot"]; got != "gpt-5" {
		t.Errorf("the defaults' other agents are kept: %q", got)
	}
}
