package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// projectFixture is a runner whose repo "app" is in project "web" with the given budget
// lines (YAML under the project), and a daemon budget high enough not to matter unless
// extra says otherwise.
func projectFixture(t *testing.T, web, extra string) *fixture {
	t.Helper()
	f := newFixture(t, "quick")
	yml := "daemon:\n  budget: 1000\n" + extra + "repos:\n  app: {}\nprojects:\n  web:\n    repos: [app]\n" + web
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Config = cfg
	return f
}

// spendRun records a run in the fixture's lane (so the spend is the repo's) and its cost.
func (f *fixture) spendRun(t *testing.T, usd float64) {
	t.Helper()
	ctx := context.Background()
	run, err := f.st.CreateRun(ctx, store.Run{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", State: store.RunSucceeded, Log: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Spend(ctx, store.Spend{Source: "claude", Ref: run.ID, USD: usd}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) feedText(t *testing.T) string {
	t.Helper()
	items, _ := f.st.Feed(context.Background(), 0, 100)
	var all []string
	for _, it := range items {
		if it.Kind == store.FeedBudget {
			all = append(all, it.Text)
		}
	}
	return strings.Join(all, "\n")
}

// startBoth checks that an automatic run in the fixture's repo is held, and returns the
// reason, and that a run the person starts goes. The hold is read from overBudgetIn,
// the check Start makes for a request with Auto set once runner.go passes it the repo.
func (f *fixture) startBoth(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app")
	if why == "" {
		t.Fatal("an automatic run should have been held")
	}
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatalf("a run the person starts must never be held: %v", err)
	}
	f.wait(t, run.ID)
	return fmt.Errorf("%w: %s", ErrRefused, why)
}

func TestHardProjectBudgetHoldsAutomaticRunsOnly(t *testing.T) {
	for name, web := range map[string]string{
		"marked":   "    budget: {amount: 10, cap: hard}\n",
		"unmarked": "    budget: {amount: 10}\n", // behaves as hard
	} {
		t.Run(name, func(t *testing.T) {
			f := projectFixture(t, web, "")
			ctx := context.Background()
			f.spendRun(t, 9.99)
			if why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app"); why != "" {
				t.Fatalf("held under budget: %s", why)
			}
			f.spendRun(t, 0.01)
			err := f.startBoth(t)
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("hold = %v", err)
			}
			// It names the budget, its figures and what lifts it.
			for _, want := range []string{"project web's", "$10.00", "projects.web.budget", "tomorrow", "cap: soft"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("hold message lacks %q: %v", want, err)
				}
			}
			if !strings.Contains(f.feedText(t), "Project web's daily budget reached") {
				t.Errorf("no 100%% warning: %s", f.feedText(t))
			}
		})
	}
}

func TestSoftProjectBudgetWarnsAndHoldsNothing(t *testing.T) {
	f := projectFixture(t, "    budget: {amount: 10, cap: soft}\n", "")
	ctx := context.Background()
	f.spendRun(t, 8)
	f.spendRun(t, 3)
	if why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app"); why != "" {
		t.Errorf("a soft cap held a run: %s", why)
	}
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", Auto: true})
	if err != nil {
		t.Fatalf("a soft cap held an automatic run: %v", err)
	}
	f.wait(t, run.ID)
	text := f.feedText(t)
	if !strings.Contains(text, "80% of project web's daily budget") || !strings.Contains(text, "Project web's daily budget reached") || !strings.Contains(text, "soft cap") {
		t.Errorf("feed = %s", text)
	}
	// Each mark is said once a day.
	f.spendRun(t, 1)
	if n := strings.Count(f.feedText(t), "reached"); n != 1 {
		t.Errorf("100%% said %d times", n)
	}
}

func TestBothCapsWarnAtEightyPercent(t *testing.T) {
	for _, cap := range []string{"soft", "hard"} {
		f := projectFixture(t, "    budget: {amount: 10, cap: "+cap+"}\n", "")
		f.spendRun(t, 8)
		text := f.feedText(t)
		if !strings.Contains(text, "80% of project web's daily budget used: $8.00 of $10.00") || strings.Contains(text, "reached") {
			t.Errorf("%s cap at 80%%: %q", cap, text)
		}
		if why := f.runner.overBudgetIn(context.Background(), f.runner.Conf(), "app"); why != "" {
			t.Errorf("%s cap held at 80%%: %s", cap, why)
		}
	}
}

func TestDeskSpendCountsToWorkspaceAndNoProject(t *testing.T) {
	f := projectFixture(t, "    budget: {amount: 10}\n", "")
	ctx := context.Background()
	f.spendRun(t, 3)
	f.runner.Spend(ctx, store.Spend{Source: store.OriginDesk, USD: 7})
	lines, err := BudgetLines(ctx, f.st, f.runner.Conf())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, l := range lines {
		got[l.Kind] = l.Spent
	}
	if got[LineWorkspace] != 10 || got[LineDesk] != 7 || got[LineProject] != 3 {
		t.Errorf("workspace, desk, project = %v, %v, %v; want 10, 7, 3", got[LineWorkspace], got[LineDesk], got[LineProject])
	}
	// Seven desk dollars do not hold the project's runs.
	if why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app"); why != "" {
		t.Errorf("the desk's spend held a project: %s", why)
	}
}

func TestDeskLineHoldsOnlyTheDesk(t *testing.T) {
	f := projectFixture(t, "", "desk:\n  budget: 2\n")
	ctx := context.Background()
	f.runner.Spend(ctx, store.Spend{Source: store.OriginDesk, USD: 2.5})
	if why := DeskHeld(ctx, f.st, f.runner.Conf()); !strings.Contains(why, "desk.budget") {
		t.Errorf("DeskHeld = %q", why)
	}
	if why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app"); why != "" {
		t.Errorf("the desk's line held a run: %s", why)
	}
	soft := projectFixture(t, "", "desk:\n  budget: 2\n  budget_cap: soft\n")
	soft.runner.Spend(ctx, store.Spend{Source: store.OriginDesk, USD: 2.5})
	if why := DeskHeld(ctx, soft.st, soft.runner.Conf()); why != "" {
		t.Errorf("a soft desk line held: %s", why)
	}
}

func TestRepoOutsideAnyProjectCountsToWorkspaceOnly(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	cfg, err := config.Parse([]byte("daemon:\n  budget: 5\nrepos:\n  app: {}\n  elsewhere: {}\nprojects:\n  other:\n    repos: [elsewhere]\n    budget: {amount: 1000}\n"))
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Config = cfg
	f.spendRun(t, 5)
	lines, _ := BudgetLines(ctx, f.st, cfg)
	for _, l := range lines {
		switch l.Kind {
		case LineWorkspace:
			if l.Spent != 5 {
				t.Errorf("workspace spent %v", l.Spent)
			}
		case LineProject:
			if l.Spent != 0 {
				t.Errorf("project %s was charged %v for a repo outside it", l.Name, l.Spent)
			}
		}
	}
	err = f.startBoth(t)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "daily budget of $5.00 (daemon.budget)") {
		t.Errorf("workspace hold = %v", err)
	}
	// The no-project entry point is the same ceiling.
	if why := f.runner.overBudget(ctx, cfg); why == "" {
		t.Error("overBudget missed the workspace ceiling")
	}
}

func TestWorkspaceCeilingHoldsAProjectUnderItsOwn(t *testing.T) {
	f := projectFixture(t, "    budget: {amount: 50}\n", "")
	cfg, _ := config.Parse([]byte("daemon:\n  budget: 5\nrepos:\n  app: {}\nprojects:\n  web:\n    repos: [app]\n    budget: {amount: 50}\n"))
	f.runner.Config = cfg
	f.spendRun(t, 5)
	err := f.startBoth(t)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "daemon.budget") {
		t.Errorf("hold = %v, want the workspace ceiling's", err)
	}
	// A soft workspace ceiling holds nothing, whatever the project's cap.
	soft, _ := config.Parse([]byte("daemon:\n  budget: 5\n  budget_cap: soft\nrepos:\n  app: {}\nprojects:\n  web:\n    repos: [app]\n    budget: {amount: 50}\n"))
	if why := f.runner.overBudgetIn(context.Background(), soft, "app"); why != "" {
		t.Errorf("soft ceiling held: %s", why)
	}
}

func TestMonthlyProjectBudgetRollsOver(t *testing.T) {
	f := projectFixture(t, "    budget: {amount: 10, period: month}\n", "")
	ctx := context.Background()
	at := func(s string) {
		tm, _ := time.ParseInLocation("2006-01-02", s, time.Local)
		clock = func() time.Time { return tm }
	}
	defer func() { clock = time.Now }()

	at("2026-10-03")
	f.spendRun(t, 6)
	at("2026-10-20")
	f.spendRun(t, 4) // a different day, the same month: the figure holds the sum
	why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app")
	if !strings.Contains(why, "month's spend, $10.00") || !strings.Contains(why, "the first of next month") {
		t.Errorf("hold = %q", why)
	}
	if !strings.Contains(f.feedText(t), "Project web's monthly budget reached") {
		t.Errorf("feed = %s", f.feedText(t))
	}

	at("2026-11-01")
	if why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app"); why != "" {
		t.Errorf("still held in the new month: %s", why)
	}
	f.spendRun(t, 8)
	text := f.feedText(t)
	if strings.Count(text, "Project web's monthly budget reached") != 1 || !strings.Contains(text, "80% of project web's monthly budget used: $8.00") {
		t.Errorf("new month's warnings: %s", text)
	}
}

func TestDailyBudgetRollsOver(t *testing.T) {
	f := projectFixture(t, "    budget: {amount: 10}\n", "")
	ctx := context.Background()
	defer func() { clock = time.Now }()
	clock = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local) }
	f.spendRun(t, 10)
	if f.runner.overBudgetIn(ctx, f.runner.Conf(), "app") == "" {
		t.Fatal("not held at the amount")
	}
	clock = func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local) }
	if why := f.runner.overBudgetIn(ctx, f.runner.Conf(), "app"); why != "" {
		t.Errorf("yesterday's spend held today: %s", why)
	}
}
