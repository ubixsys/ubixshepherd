package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestOutputReaders(t *testing.T) {
	o := claudeOutput(`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."},{"type":"tool_use","name":"Edit","input":{"file_path":"a.go"}}]}}`)
	if o.Show != "Done.\n→ Edit {\"file_path\":\"a.go\"}" {
		t.Errorf("assistant line = %q", o.Show)
	}
	if o := claudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.1234}`); o.USD != 0.1234 || o.Show != "" {
		t.Errorf("result = %+v", o)
	}
	if o := claudeOutput(`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`); o.Show != "" {
		t.Errorf("tool result shown: %+v", o)
	}
	if o := claudeOutput("plain text"); o.Show != "plain text" {
		t.Errorf("non-JSON line = %+v", o)
	}
	if o := copilotOutput("AI Credits 0.36 (39s)"); o.Credits != 0.36 || o.Show == "" {
		t.Errorf("copilot credits = %+v", o)
	}
}

// Claude Code's total_cost_usd on a resumed session is the session's: run 12, a
// 10-second continuation reporting $8.39 after run 11's $8.28, cost $0.11.
func TestClaudeSessionCostRecordsWhatEachRunAdded(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	runWith := func(cost string) store.Run {
		t.Helper()
		t.Setenv("COST", cost)
		run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return f.wait(t, run.ID)
	}
	first, second := runWith("8.28"), runWith("8.39")
	if second.Parent != first.ID || second.Session != first.Session {
		t.Fatalf("second run did not continue the session: %+v", second)
	}
	if first.CostUSD != 8.28 || second.CostUSD != 0.11 || second.SessionUSD != 8.39 {
		t.Errorf("costs: first %v, second %v (session %v)", first.CostUSD, second.CostUSD, second.SessionUSD)
	}
	if spent, _, _ := f.runner.Spent(ctx); spent < 8.389 || spent > 8.391 {
		t.Errorf("today's spend = %v, want 8.39", spent)
	}
}

// Copilot's credits count the whole session: a continued run records what it added.
func TestSessionCostRecordsWhatEachRunAdded(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	runWith := func(credits string, req StartRequest) store.Run {
		t.Helper()
		t.Setenv("CREDITS", credits)
		req.LaneID, req.Agent, req.Prompt = f.lane.ID, "copilot", "x"
		run, err := f.runner.Start(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return f.wait(t, run.ID)
	}
	first := runWith("4.12", StartRequest{})
	second := runWith("6.17", StartRequest{})
	quiet := runWith("", StartRequest{})     // reports nothing
	third := runWith("6.50", StartRequest{}) // still the same session
	fresh := runWith("0.40", StartRequest{NewSession: true})
	if second.Parent != first.ID || third.Parent != quiet.ID {
		t.Fatalf("runs did not continue the session: %+v %+v", second, third)
	}
	for _, c := range []struct {
		run           store.Run
		credits, sess float64
	}{{first, 4.12, 4.12}, {second, 2.05, 6.17}, {quiet, 0, 6.17}, {third, 0.33, 6.50}, {fresh, 0.40, 0.40}} {
		if c.run.Credits != c.credits || c.run.SessionCredits != c.sess {
			t.Errorf("run %d: credits %v (session %v), want %v (session %v)", c.run.ID, c.run.Credits, c.run.SessionCredits, c.credits, c.sess)
		}
	}
	_, by, _ := f.runner.Spent(ctx)
	if got := by["copilot"].Credits; got < 6.899 || got > 6.901 {
		t.Errorf("today's copilot credits = %v, want 6.90 (6.50 for the session, 0.40 fresh)", got)
	}

	// A run recorded before session totals were kept stored the total as its cost.
	legacy, _ := f.st.CreateRun(ctx, store.Run{LaneID: f.lane.ID, Agent: "copilot", Prompt: "x", State: store.RunSucceeded, Session: "old", Log: "x"})
	legacy.Credits = 4.12
	f.st.UpdateRun(ctx, legacy)
	next := store.Run{Parent: legacy.ID, Credits: 6.17}
	f.runner.sessionCost(ctx, &next)
	if next.Credits != 2.05 || next.SessionCredits != 6.17 {
		t.Errorf("after a legacy run: %+v", next)
	}
	// A total below the last means the CLI counts afresh: all of it is this run's.
	reset := store.Run{Parent: second.ID, Credits: 1.5}
	f.runner.sessionCost(ctx, &reset)
	if reset.Credits != 1.5 {
		t.Errorf("after a reset: %+v", reset)
	}
}

func TestBudgetHoldsAutomaticRuns(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	cfg, _ := config.Parse([]byte("daemon:\n  budget: 1\n  credit_usd: 0.5\n"))
	f.runner.Config = cfg

	// 0.6 dollars plus one credit at 0.5 is 1.10: past 80% after the first, past 100% after the second.
	f.runner.Spend(ctx, store.Spend{Source: "claude", USD: 0.85})
	f.runner.Spend(ctx, store.Spend{Source: "copilot", Credits: 0.5})
	f.runner.Spend(ctx, store.Spend{Source: "desk", USD: 0.01})
	spent, by, _ := f.runner.Spent(ctx)
	if spent < 1.10 || spent > 1.11 || by["copilot"].Credits != 0.5 {
		t.Errorf("spent = %v, by = %+v", spent, by)
	}
	items, _ := f.st.Feed(ctx, 0, 50)
	var warn, reached int
	for _, it := range items {
		if strings.Contains(it.Text, "80% of today's budget") {
			warn++
		}
		if strings.Contains(it.Text, "Daily budget reached") {
			reached++
		}
	}
	if warn != 1 || reached != 1 {
		t.Errorf("warnings: 80%% x%d, reached x%d", warn, reached)
	}

	if _, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", Auto: true}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "daily budget") {
		t.Errorf("automatic run over budget: %v", err)
	}
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatalf("a run the person starts should still go: %v", err)
	}
	f.wait(t, run.ID)

	zero, _ := config.Parse([]byte("daemon:\n  budget: 0\n"))
	f.runner.SetConfig(zero) // the run's ship may still be reading the configuration
	if why := f.runner.overBudget(ctx, f.runner.Conf()); why != "" {
		t.Errorf("budget 0 should mean no cap: %s", why)
	}
}
