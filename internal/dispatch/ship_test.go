package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

type shipForge struct {
	mu      sync.Mutex
	created []string
}

func (f *shipForge) Name() string                                                 { return "fake" }
func (f *shipForge) MRForBranch(context.Context, string) (*forge.MR, error)       { return nil, nil }
func (f *shipForge) FailedJobs(context.Context, int64) ([]forge.Job, error)       { return nil, nil }
func (f *shipForge) RefPipeline(context.Context, string) (*forge.Pipeline, error) { return nil, nil }
func (f *shipForge) JobLog(context.Context, int64, int) (string, error)           { return "", nil }
func (f *shipForge) CreateMR(_ context.Context, source, target, title, body string) (*forge.MR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, source+" -> "+target+": "+title+"\n"+body)
	return &forge.MR{IID: 42, State: "opened", URL: "https://gl/mr/42"}, nil
}

func shipFixture(t *testing.T, mode, profile string) (*fixture, *shipForge) {
	t.Helper()
	f := newFixture(t, mode)
	cfg, err := config.Parse([]byte(profile))
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Config = cfg
	sf := &shipForge{}
	f.runner.ForgeFor = func(string) (forge.Forge, error) { return sf, nil }
	return f, sf
}

const pushes = "repos:\n  app:\n    gate: \"true\"\n    autonomy:\n      push: shepherd\n"

func originHas(t *testing.T, f *fixture, branch string) bool {
	out, _ := exec.Command("git", "-C", f.origin, "branch", "--list", branch).Output()
	return strings.TrimSpace(string(out)) != ""
}

// waitFeed waits for a feed line containing s.
func waitFeed(t *testing.T, f *fixture, s string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := f.st.Feed(context.Background(), 0, 500)
		for _, it := range items {
			if strings.Contains(it.Text, s) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	items, _ := f.st.Feed(context.Background(), 0, 500)
	var all []string
	for _, it := range items {
		all = append(all, it.Text)
	}
	t.Fatalf("no feed line with %q in:\n%s", s, strings.Join(all, "\n"))
}

func TestShipPushesAndOpensTheMR(t *testing.T) {
	f, sf := shipFixture(t, "ok", pushes)
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, run.ID)
	waitFeed(t, f, "opened !42 for you to review")
	if !originHas(t, f, "work") {
		t.Error("the lane was not pushed")
	}
	if len(sf.created) != 1 || !strings.HasPrefix(sf.created[0], "work -> main: agent work") || !strings.Contains(sf.created[0], "The gate, `true`, passed") || strings.Contains(sf.created[0], "claude") {
		t.Errorf("created = %q", sf.created)
	}
}

func TestShipHandsAFailedGateBack(t *testing.T) {
	f, sf := shipFixture(t, "ok", strings.Replace(pushes, `gate: "true"`, `gate: "echo gate says no; exit 1"`, 1))
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	f.wait(t, run.ID)
	waitFeed(t, f, "asked claude to fix it")
	if originHas(t, f, "work") || len(sf.created) != 0 {
		t.Error("pushed although the gate failed")
	}
	// The first fix continues the original run; a fast machine may have started the
	// second already, so find it by its parent rather than by being newest.
	var fix store.Run
	runs, _ := f.st.Runs(context.Background(), f.lane.ID, "", 5)
	for _, r := range runs {
		if r.Parent == run.ID {
			fix = r
		}
	}
	if fix.ID == 0 || !strings.Contains(fix.Prompt, "gate says no") {
		t.Errorf("fix run = %+v", fix)
	}
	f.wait(t, fix.ID)
	// The fix run commits again and the gate fails again: the second and last try.
	waitFeed(t, f, "try 2 of 2")
}

func TestShipOnlyWhenOptedInAndFinished(t *testing.T) {
	f, sf := shipFixture(t, "ok", "repos:\n  app:\n    gate: \"true\"\n")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	f.wait(t, run.ID)
	time.Sleep(300 * time.Millisecond)
	if originHas(t, f, "work") || len(sf.created) != 0 {
		t.Error("pushed for a repo that is not opted in")
	}

	g, gf := shipFixture(t, "stray", pushes)
	run, _ = g.runner.Start(context.Background(), StartRequest{LaneID: g.lane.ID, Agent: "claude", Prompt: "work"})
	g.wait(t, run.ID)
	waitFeed(t, g, "changed files outside its scope")
	if originHas(t, g, "work") || len(gf.created) != 0 {
		t.Error("pushed work outside the scope")
	}

	h, hf := shipFixture(t, "ok", pushes)
	run, _ = h.runner.Start(context.Background(), StartRequest{LaneID: h.lane.ID, Agent: "claude", Prompt: "work"})
	h.runner.Ask(context.Background(), store.Decision{RunID: run.ID, Question: "which?", Recommendation: "a"})
	h.wait(t, run.ID)
	time.Sleep(300 * time.Millisecond)
	if originHas(t, h, "work") || len(hf.created) != 0 {
		t.Error("pushed while the agent waits on a decision")
	}
}

func TestShipEnforcesCommitRules(t *testing.T) {
	// The fake agent's commit message is "agent work"; forbid "agent".
	f, sf := shipFixture(t, "ok", pushes+"    forbid: [\"(?i)agent work\"]\n    brief: Sign nothing.\n")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	if a := stdinOf(t, f.wait(t, run.ID)); !strings.Contains(a, "This repo's rules: Sign nothing.") {
		t.Errorf("brief lacks the repo's rules: %s", a)
	}
	waitFeed(t, f, "the repo's commit rules failed in lane work; asked claude to fix it")
	if originHas(t, f, "work") || len(sf.created) != 0 {
		t.Error("pushed a forbidden commit message")
	}
	runs, _ := f.st.Runs(context.Background(), f.lane.ID, "", 5)
	found := false
	for _, r := range runs {
		if r.Parent == run.ID && strings.Contains(r.Prompt, `contains "agent work"`) && strings.Contains(r.Prompt, "Amend") {
			found = true
		}
	}
	if !found {
		t.Errorf("no fix run asking to amend among %d runs", len(runs))
	}
}

// commitInLane commits a file in the lane's worktree, as work done outside a run.
func commitInLane(t *testing.T, f *fixture, file, msg string) {
	t.Helper()
	path := filepath.Join(f.lane.Worktree, file)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(msg+"\n"), 0o644)
	for _, args := range [][]string{{"add", file}, {"commit", "-q", "-m", msg}} {
		if out, err := exec.Command("git", append([]string{"-C", f.lane.Worktree}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestShipLaneShipsExistingCommits(t *testing.T) {
	f, sf := shipFixture(t, "ok", pushes)
	ctx := context.Background()
	commitInLane(t, f, "src/a.txt", "feat: work done before")
	res, err := f.runner.Ship(ctx, f.lane.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || res.MR != 42 || res.Commits != 1 || !strings.Contains(res.Message, "opened !42") || !originHas(t, f, "work") {
		t.Errorf("ship = %+v", res)
	}
	if len(sf.created) != 1 || !strings.Contains(sf.created[0], "feat: work done before") || !strings.Contains(sf.created[0], "(shepherd lane ship)") {
		t.Errorf("created = %q", sf.created)
	}
	// Everything is pushed now: nothing more to ship.
	if _, err := f.runner.Ship(ctx, f.lane.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "nothing to ship") {
		t.Errorf("second ship: %v", err)
	}
}

func TestShipLaneRefuses(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, f *fixture, sf *shipForge, want string) {
		t.Helper()
		_, err := f.runner.Ship(ctx, f.lane.ID)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) {
			t.Errorf("ship: %v, want a refusal with %q", err, want)
		}
		if originHas(t, f, "work") || len(sf.created) != 0 {
			t.Error("pushed although refused")
		}
	}
	t.Run("not opted in", func(t *testing.T) {
		f, sf := shipFixture(t, "ok", "repos:\n  app:\n    gate: \"true\"\n")
		commitInLane(t, f, "src/a.txt", "feat: a")
		refused(t, f, sf, "app has not opted in to Shepherd pushing")
	})
	t.Run("nothing committed", func(t *testing.T) {
		f, sf := shipFixture(t, "ok", pushes)
		refused(t, f, sf, "nothing to ship")
	})
	t.Run("uncommitted changes", func(t *testing.T) {
		f, sf := shipFixture(t, "ok", pushes)
		commitInLane(t, f, "src/a.txt", "feat: a")
		os.WriteFile(filepath.Join(f.lane.Worktree, "src", "a.txt"), []byte("changed"), 0o644)
		refused(t, f, sf, "uncommitted changes")
	})
	t.Run("outside the scope", func(t *testing.T) {
		f, sf := shipFixture(t, "ok", pushes)
		commitInLane(t, f, "stray.txt", "feat: stray")
		refused(t, f, sf, "outside its scope (stray.txt)")
	})
	t.Run("commit rules", func(t *testing.T) {
		f, sf := shipFixture(t, "ok", pushes+"    forbid: [\"(?i)wip\"]\n")
		commitInLane(t, f, "src/a.txt", "WIP: a")
		refused(t, f, sf, `contains "WIP"`)
	})
	t.Run("gate fails", func(t *testing.T) {
		f, sf := shipFixture(t, "ok", strings.Replace(pushes, `gate: "true"`, `gate: "echo gate says no; exit 1"`, 1))
		commitInLane(t, f, "src/a.txt", "feat: a")
		refused(t, f, sf, "gate says no")
		if runs, _ := f.st.Runs(ctx, f.lane.ID, "", 5); len(runs) != 0 {
			t.Errorf("an explicit ship started %d runs; it should only refuse", len(runs))
		}
	})
}

// endedRun records a finished run in the lane, as if an agent had made its commits.
func endedRun(t *testing.T, f *fixture) store.Run {
	t.Helper()
	run, err := f.st.CreateRun(context.Background(), store.Run{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", Session: "S", State: store.RunSucceeded, Log: "l"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestShipNotHeldByHandoffs(t *testing.T) {
	f, sf := shipFixture(t, "ok", pushes)
	ctx := context.Background()
	commitInLane(t, f, "src/a.txt", "feat: a")
	run := endedRun(t, f)
	f.st.CreateRequest(ctx, store.Request{FromRun: run.ID, Kind: KindHandoff, Lane: "elsewhere", Message: "do x", State: store.RequestNeedsRouting})
	f.runner.ship(ctx, run, f.lane)
	if !originHas(t, f, "work") || len(sf.created) != 1 {
		t.Error("a handoff to another lane held the push")
	}
}

func TestShipHeldByAQuestionUntilItSettles(t *testing.T) {
	f, sf := shipFixture(t, "ok", pushes)
	ctx := context.Background()
	commitInLane(t, f, "src/a.txt", "feat: a")
	run := endedRun(t, f)
	q, _ := f.st.CreateRequest(ctx, store.Request{FromRun: run.ID, Kind: KindQuestion, Lane: "elsewhere", Message: "which?", State: store.RequestNeedsRouting})
	f.runner.ship(ctx, run, f.lane)
	waitFeed(t, f, fmt.Sprintf("not pushing lane work yet: run %d waits on the answer to its question, request %d", run.ID, q.ID))
	if originHas(t, f, "work") {
		t.Fatal("pushed while the run waits on its question")
	}
	// The question fails: no answer is coming, so the lane ships.
	q.State = store.RequestFailed
	f.st.UpdateRequest(ctx, q)
	f.runner.settled(ctx, q)
	waitFeed(t, f, "opened !42")
	if len(sf.created) != 1 {
		t.Errorf("created = %q", sf.created)
	}
}

func TestShipShipsTheLaneNotJustTheRun(t *testing.T) {
	// A run that continues held-back work (a reply, a fix) may add no commits itself.
	f, sf := shipFixture(t, "quick", pushes)
	commitInLane(t, f, "src/a.txt", "feat: a")
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "carry on"})
	if done := f.wait(t, run.ID); done.Commits != 0 {
		t.Fatalf("run = %+v", done)
	}
	waitFeed(t, f, "opened !42")
	if !originHas(t, f, "work") || len(sf.created) != 1 {
		t.Error("the lane's earlier commits were not shipped")
	}
}

func TestShipPostsGateKinds(t *testing.T) {
	f, _ := shipFixture(t, "ok", pushes)
	run, _ := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "work"})
	f.wait(t, run.ID)
	for _, s := range []string{"running the gate `true`", "gate `true` passed in lane work", "(gate `true` passed) and opened"} {
		want := store.FeedGate
		if strings.Contains(s, "opened") {
			want = store.FeedMR
		}
		if k := feedKind(t, f, s); k != want {
			t.Errorf("%q kind = %q, want %q", s, k, want)
		}
	}

	g, _ := shipFixture(t, "ok", strings.Replace(pushes, `gate: "true"`, `gate: "exit 1"`, 1))
	run, _ = g.runner.Start(context.Background(), StartRequest{LaneID: g.lane.ID, Agent: "claude", Prompt: "work"})
	g.wait(t, run.ID)
	if k := feedKind(t, g, "asked claude to fix it"); k != store.FeedGate {
		t.Errorf("failed gate kind = %q", k)
	}
	items, _ := g.st.Feed(context.Background(), 0, 500)
	for _, it := range items {
		if it.Kind == store.FeedPipeline {
			t.Errorf("the gate posted a pipeline item: %s", it.Text)
		}
	}
}
