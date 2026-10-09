package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/convo"

	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// waitRequest waits for a request to reach a state.
func waitRequest(t *testing.T, f *fixture, id int64, state string) store.Request {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		q, err := f.st.Request(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if q.State == state {
			return q
		}
		if q.State == store.RequestFailed {
			t.Fatalf("request failed: %s", q.Note)
		}
		time.Sleep(50 * time.Millisecond)
	}
	q, _ := f.st.Request(context.Background(), id)
	t.Fatalf("request %d stuck in %s (%s), want %s", id, q.State, q.Note, state)
	return q
}

func openLane(t *testing.T, f *fixture, name, glob string) store.Lane {
	t.Helper()
	o, err := (&fold.Fold{Store: f.st, Config: f.runner.Config}).Open(context.Background(),
		fold.OpenRequest{RepoID: f.lane.RepoID, Name: name, Scope: []string{glob}})
	if err != nil {
		t.Fatal(err)
	}
	return o.Lane
}

func TestQuestionRoutedAndReplyReturned(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	other := openLane(t, f, "api", "api/**")
	// The api lane has a copilot conversation going.
	prior, _ := f.runner.Start(ctx, StartRequest{LaneID: other.ID, Agent: "copilot", Prompt: "build the api"})
	prior = f.wait(t, prior.ID)

	asker, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "build the client"})
	q, err := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindQuestion, Lane: "api", Message: "What does GET /users return?"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.Request(ctx, q.ID); got.State != store.RequestPending {
		t.Errorf("routed while the asker was still running: %+v", got)
	}
	f.wait(t, asker.ID)

	q = waitRequest(t, f, q.ID, store.RequestReplied)
	target, _ := f.st.Run(ctx, q.TargetRun)
	// It continued the api lane's copilot conversation (the fake reports a new id per run).
	if target.LaneID != other.ID || target.Agent != "copilot" || target.Parent != prior.ID {
		t.Errorf("question went to %+v, want copilot's session in lane api", target)
	}
	if a := argsOf(t, target); !strings.Contains(a, "What does GET /users return?") || !strings.Contains(a, "from claude in lane work") {
		t.Errorf("target prompt: %s", a)
	}
	back := f.wait(t, q.ReplyRun)
	if back.Session != asker.Session || back.Parent != asker.ID {
		t.Errorf("reply went to %+v, want the asker's session", back)
	}
	if a := argsOf(t, back); !strings.Contains(a, "Reply to your question") || !strings.Contains(a, "fake agent in") {
		t.Errorf("reply prompt: %s", a)
	}
	if q.Depth != 1 {
		t.Errorf("depth = %d", q.Depth)
	}
}

func TestReviewGoesToAnotherProvider(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	author, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "write it"})
	f.wait(t, author.ID)
	q, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: author.ID, Kind: KindReview, Message: "Review my lane"})
	q = waitRequest(t, f, q.ID, store.RequestReplied)
	rev, _ := f.st.Run(ctx, q.TargetRun)
	if rev.Agent != "copilot" || rev.LaneID != f.lane.ID || rev.Parent != 0 {
		t.Errorf("reviewer run = %+v, want a fresh copilot session in the author's lane", rev)
	}
	if a := argsOf(t, rev); !strings.Contains(a, "Do not change any files") {
		t.Errorf("review prompt: %s", a)
	}
}

func TestRequestsShepherdCannotRoute(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	asker, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	f.wait(t, asker.ID)

	noLane, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindHandoff, Message: "someone add docs"})
	waitRequest(t, f, noLane.ID, store.RequestNeedsRouting)
	missing, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindQuestion, Lane: "nope", Message: "?"})
	if q := waitRequest(t, f, missing.ID, store.RequestNeedsRouting); !strings.Contains(q.Note, "no open lane nope") {
		t.Errorf("note = %q", q.Note)
	}
	empty := openLane(t, f, "docs", "docs/**")
	idle, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindHandoff, Lane: "docs", Message: "write docs"})
	if q := waitRequest(t, f, idle.ID, store.RequestNeedsRouting); !strings.Contains(q.Note, "no agent yet") {
		t.Errorf("note = %q", q.Note)
	}

	// The front desk routes it, naming the agent.
	q, err := f.runner.RouteRequest(ctx, idle.ID, "docs", "cursor")
	if err != nil {
		t.Fatal(err)
	}
	q = waitRequest(t, f, q.ID, store.RequestReplied)
	if run, _ := f.st.Run(ctx, q.TargetRun); run.Agent != "cursor" || run.LaneID != empty.ID {
		t.Errorf("routed run = %+v", run)
	}
	if _, err := f.runner.RouteRequest(ctx, idle.ID, "docs", ""); !errors.Is(err, ErrRefused) {
		t.Errorf("routing a replied request: %v", err)
	}
	if _, err := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: "gossip", Message: "x"}); !errors.Is(err, ErrRefused) {
		t.Errorf("bad kind: %v", err)
	}
}

func TestChainDepthIsCapped(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	run, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	run = f.wait(t, run.ID)
	// Pretend each hop's target run asks in turn.
	from := run.ID
	for depth := 1; depth <= MaxDepth; depth++ {
		q, err := f.runner.RequestHelp(ctx, store.Request{FromRun: from, Kind: KindReview, Message: "again"})
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		q = waitRequest(t, f, q.ID, store.RequestReplied)
		if q.Depth != depth {
			t.Fatalf("depth = %d, want %d", q.Depth, depth)
		}
		from = q.TargetRun
	}
	if _, err := f.runner.RequestHelp(ctx, store.Request{FromRun: from, Kind: KindReview, Message: "once more"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "ask_human") {
		t.Errorf("past the cap: %v", err)
	}
}

func TestQuestionToALaneAnsweredByItsConversation(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	pay := openLane(t, f, "feat/payments", "pay/**")
	// A conversation the person had by hand on that branch, long since quiet.
	f.st.PutConversation(ctx, store.Conversation{ID: "71ffa009-0000", Agent: "claude", RepoID: pay.RepoID, Dir: f.lane.Worktree,
		Title: "Stripe integration", Branches: []string{"dev", "feat/payments"}, Last: time.Now().Add(-time.Hour)})
	var asked string
	f.runner.AskConversation = func(_ context.Context, c store.Conversation, q string) (convo.Answer, error) {
		asked = c.ID + ": " + q
		return convo.Answer{Text: "The webhook secret is in Vault at payments/stripe.", USD: 0.5}, nil
	}
	asker, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	f.wait(t, asker.ID)
	q, err := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindQuestion, Lane: "feat/payments", Message: "Where is the webhook secret?"})
	if err != nil {
		t.Fatal(err)
	}
	q = waitRequest(t, f, q.ID, store.RequestReplied)
	if !strings.HasPrefix(asked, "71ffa009-0000: ") || !strings.Contains(asked, "Where is the webhook secret?") || !strings.Contains(asked, "Do not change anything") {
		t.Errorf("asked = %q", asked)
	}
	if q.Agent != "conversation 71ffa009" || !strings.Contains(q.Reply, "payments/stripe") {
		t.Errorf("request = %+v", q)
	}
	back := f.wait(t, q.ReplyRun)
	if back.Parent != asker.ID || !strings.Contains(back.Prompt, "payments/stripe") {
		t.Errorf("reply run = %+v", back)
	}
	if spent, _, _ := f.runner.Spent(ctx); spent < 0.5 {
		t.Errorf("the conversation's cost was not counted: %v", spent)
	}
}

func TestRequestAddressing(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	gate := openLane(t, f, "feat/m2-gate", "gate/**")
	openLane(t, f, "feat/docs", "docs/**")
	openLane(t, f, "fix/docs", "docs2/**")
	prior, _ := f.runner.Start(ctx, StartRequest{LaneID: gate.ID, Agent: "copilot", Prompt: "x"})
	f.wait(t, prior.ID)
	asker, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	f.wait(t, asker.ID)

	// A name without its prefix finds the one lane it can mean...
	q, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindHandoff, Lane: "m2-gate", Message: "x"})
	if q = waitRequest(t, f, q.ID, store.RequestReplied); q.Lane != "feat/m2-gate" {
		t.Errorf("routed to %q", q.Lane)
	}
	// ...and asks when it could mean two.
	two, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindHandoff, Lane: "docs", Message: "x"})
	if q := waitRequest(t, f, two.ID, store.RequestNeedsRouting); !strings.Contains(q.Note, "feat/docs, fix/docs") {
		t.Errorf("note = %q", q.Note)
	}

	// For the person: a decision, not a request to route.
	for _, in := range []store.Request{{Kind: KindPerson, Message: "Which price?"}, {Kind: KindQuestion, Lane: "human", Message: "Ship it?"}} {
		in.FromRun = asker.ID
		p, err := f.runner.RequestHelp(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if p.State != store.RequestClosed || !strings.Contains(p.Note, "decision") {
			t.Errorf("request for the person = %+v", p)
		}
	}
	ds, _ := f.st.Decisions(ctx, store.DecisionOpen)
	if len(ds) != 2 || ds[0].Question != "Which price?" || ds[0].RunID != asker.ID {
		t.Errorf("decisions = %+v", ds)
	}

	// A stale request is closed, once.
	if _, err := f.runner.CloseRequest(ctx, two.ID, "the docs lane was merged"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.Request(ctx, two.ID); got.State != store.RequestClosed || got.Note != "closed: the docs lane was merged" {
		t.Errorf("closed request = %+v", got)
	}
	waitFeed(t, f, fmt.Sprintf("request %d closed: the docs lane was merged", two.ID))
	if _, err := f.runner.CloseRequest(ctx, two.ID, ""); !errors.Is(err, ErrRefused) {
		t.Errorf("closing twice: %v", err)
	}
	if _, err := f.runner.RouteRequest(ctx, two.ID, "feat/docs", ""); !errors.Is(err, ErrRefused) {
		t.Errorf("routing a closed request: %v", err)
	}
}

func TestRoutedRequestStartsOrSaysWhy(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	api := openLane(t, f, "api", "api/**")
	prior, _ := f.runner.Start(ctx, StartRequest{LaneID: api.ID, Agent: "copilot", Prompt: "build the api"})
	f.wait(t, prior.ID)
	asker, _ := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	f.wait(t, asker.ID)

	// A run going in the target lane queues the request, and says so.
	busy, _ := f.st.CreateRun(ctx, store.Run{LaneID: api.ID, Agent: "copilot", Prompt: "busy", State: store.RunRunning, Log: "l"})
	q, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindHandoff, Lane: "api", Message: "add an endpoint"})
	waitFeed(t, f, fmt.Sprintf("request %d waits: queued: run %d (copilot) is going in lane api", q.ID, busy.ID))
	busy.State = store.RunSucceeded
	f.st.UpdateRun(ctx, busy)
	f.runner.Route(ctx) // what the run's end does
	waitRequest(t, f, q.ID, store.RequestReplied)

	// Over the daily budget, Shepherd's own routing holds and says why...
	budget := 0.01
	f.runner.Config.Daemon.Budget = &budget
	f.runner.Spend(ctx, store.Spend{Source: "claude", USD: 1})
	held, _ := f.runner.RequestHelp(ctx, store.Request{FromRun: asker.ID, Kind: KindHandoff, Lane: "api", Message: "more"})
	waitFeed(t, f, fmt.Sprintf("request %d waits: held: today's spend", held.ID))
	if got, _ := f.st.Request(ctx, held.ID); got.State != store.RequestPending || !strings.Contains(got.Note, "daily budget") {
		t.Errorf("held request = %+v", got)
	}
	// ...but the desk's or the person's routing is their say, and goes.
	if _, err := f.runner.RouteRequest(ctx, held.ID, "api", ""); err != nil {
		t.Fatal(err)
	}
	waitRequest(t, f, held.ID, store.RequestReplied)
}
