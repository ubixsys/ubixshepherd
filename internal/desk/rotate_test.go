package desk

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// deskConfig is a rig option giving the desk these desk.* settings.
func deskConfig(set func(*config.Desk)) func(*Options) {
	return func(o *Options) {
		c := config.Default()
		set(&c.Desk)
		o.Config = func() config.Config { return c }
	}
}

func intp(n int) *int         { return &n }
func usdp(f float64) *float64 { return &f }
func (r *rig) tokens(n int) {
	os.WriteFile(filepath.Join(r.dir(), "tokens"), []byte(strconv.Itoa(n)), 0o644)
}
func (r *rig) dir() string { return filepath.Dir(r.log) }
func (r *rig) lastArgs(t *testing.T) string {
	log := r.claudeLog(t)
	return log[strings.LastIndex(log, "ARGS"):]
}

func (r *rig) rotations(t *testing.T) []store.FeedItem {
	t.Helper()
	items, err := r.st.Feed(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.FeedItem
	for _, it := range items {
		if it.Kind == FeedRotated {
			out = append(out, it)
		}
	}
	return out
}

// Past desk.rotate_tokens, the session ends with the turn; the next turn starts a new
// one, seeded with the summary, and the thread goes on as before.
func TestRotatesAtThreshold(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.RotateTokens = intp(50000) }))
	d := r.desk(t)
	r.tokens(60000)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	old := d.Session(context.Background())
	if old != "" {
		t.Fatalf("session %q kept past the threshold", old)
	}
	rot := r.rotations(t)
	if len(rot) != 1 || !strings.Contains(rot[0].Text, "60000 tokens (desk.rotate_tokens 50000)") || rot[0].Ref != r.ws.ID {
		t.Fatalf("rotations = %+v", rot)
	}
	first := r.claudeLog(t)

	r.tokens(1000)
	d.Send(context.Background(), "two")
	es := r.waitEnded(t, 2)
	last := r.lastArgs(t)
	if !strings.Contains(last, "--session-id") || !strings.Contains(last, "This session continues the front desk's earlier conversation") {
		t.Errorf("the new session was not briefed as a continuation:\n%s", last)
	}
	if !strings.Contains(last, "MSG [Shepherd] Summary for the new session") || !strings.Contains(last, "The person:   one") ||
		!strings.Contains(last, "The desk:   reply 1") || !strings.Contains(last, "End of the summary. The message that started this turn follows.  two") {
		t.Errorf("the new session's first message:\n%s", last)
	}
	if strings.Contains(first, "Summary for the new session") || strings.Contains(first, "continues the front desk") {
		t.Errorf("the very first session was seeded:\n%s", first)
	}
	// The thread holds the person's words, not the summary.
	for _, e := range es {
		if strings.Contains(e.Text, "Summary for the new session") {
			t.Errorf("the summary reached the thread: %s", kinds(es))
		}
	}
	if s := d.Session(context.Background()); s == "" {
		t.Error("the new session was not kept")
	}
	// Below the threshold now: the third turn resumes it.
	d.Send(context.Background(), "three")
	r.waitEnded(t, 3)
	if !strings.Contains(r.lastArgs(t), "--resume ") || len(r.rotations(t)) != 1 {
		t.Errorf("third turn:\n%s", r.lastArgs(t))
	}
}

func TestNoRotationBelowThreshold(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.RotateTokens = intp(50000) }))
	d := r.desk(t)
	r.tokens(49000)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	s := d.Session(context.Background())
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	if s == "" || !strings.Contains(r.lastArgs(t), "--resume "+s) || len(r.rotations(t)) != 0 {
		t.Errorf("rotated below the threshold:\n%s", r.claudeLog(t))
	}
}

func TestRotationDisabled(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.RotateTokens = intp(0) }))
	d := r.desk(t)
	r.tokens(900000)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	if d.Session(context.Background()) == "" || len(r.rotations(t)) != 0 {
		t.Error("rotated with desk.rotate_tokens 0")
	}
}

// The default threshold applies with no setting.
func TestRotationDefault(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	r.tokens(config.DefaultRotateTokens + 1)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	if d.Session(context.Background()) != "" || len(r.rotations(t)) != 1 {
		t.Error("did not rotate past the default threshold")
	}
}

// desk.rotate_cost: the fake session costs $0.10 then $0.25 in total.
func TestRotationOnCost(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.RotateCost = usdp(0.2) }))
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	if d.Session(context.Background()) == "" {
		t.Fatal("rotated below the cost")
	}
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	rot := r.rotations(t)
	if d.Session(context.Background()) != "" || len(rot) != 1 || !strings.Contains(rot[0].Text, "$0.25 (desk.rotate_cost $0.20)") {
		t.Errorf("rotations = %+v", rot)
	}
}

// A turn the person queued while the rotating turn ran, answering a decision, still runs
// as theirs, in the new session; the decision waiting is in the summary; and the swarm's
// events waiting for the desk are still delivered.
func TestRotationKeepsQueuedTurns(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.RotateTokens = intp(50000) }))
	ctx := context.Background()
	r.records(t, "hunter2-not-a-real-one")
	d := r.desk(t)
	sub := d.Subscribe()
	defer d.Unsubscribe(sub)
	r.tokens(60000)
	d.Send(ctx, "please wait")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.claudeLog(t), "MSG please wait") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r.tokens(1000)
	answer, _, err := d.Send(ctx, "decision 1: yes")
	if err != nil {
		t.Fatal(err)
	}
	r.m.Wake([]store.FeedItem{feedItem(store.FeedRunFailed, "run 7 failed")})
	es := r.waitEnded(t, 3)
	if len(r.rotations(t)) != 1 {
		t.Fatalf("rotations: %+v", r.rotations(t))
	}
	log := r.claudeLog(t)
	second := log[strings.Index(log, "MSG [Shepherd] Summary"):]
	if !strings.Contains(second, "decision 1 (run 1)") || !strings.Contains(second, "Use the password=") ||
		!strings.Contains(second, "End of the summary. The message that started this turn follows.  decision 1: yes") {
		t.Errorf("the queued turn after the rotation:\n%s", second)
	}
	if h := r.tok.humans(); len(h) < 2 || !h[1] {
		t.Errorf("the queued answer's turn was not the person's: %v", h)
	}
	ended := map[int64]string{}
	for _, e := range es {
		if e.Kind == store.DeskTurnEnd {
			ended[e.Turn] = e.Text
		}
	}
	if ended[answer] != "done" || !strings.Contains(log, "run 7 failed") {
		t.Errorf("ends %v:\n%s", ended, log)
	}
}

// Starting a new conversation on purpose takes the same path: the next turn is a new
// session seeded with the summary.
func TestNewSeedsTheSummary(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	if err := d.New(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	last := r.lastArgs(t)
	if !strings.Contains(last, "--session-id") || !strings.Contains(last, "continues the front desk's earlier conversation") ||
		!strings.Contains(last, "MSG [Shepherd] Summary for the new session") || !strings.Contains(last, "The person:   one") {
		t.Errorf("after new:\n%s", last)
	}
	if len(r.rotations(t)) != 0 {
		t.Error("new was posted as a rotation")
	}
}

// desk.summary_model adds the model's notes, and the call's cost is the desk's.
func TestSummaryNotes(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.SummaryModel = "small" }))
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	d.New(context.Background())
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	log := r.claudeLog(t)
	if !strings.Contains(log, "NOTES -p --output-format json --model small --no-session-persistence --tools") {
		t.Errorf("notes call:\n%s", log)
	}
	if !strings.Contains(r.lastArgs(t), "The person wants the login lane landed today.") {
		t.Errorf("notes missing:\n%s", r.lastArgs(t))
	}
	spent, _ := r.st.SpendOn(context.Background(), time.Now().Format("2006-01-02"))
	if got := spent[store.OriginDesk].USD; got < 0.2099 || got > 0.2101 {
		t.Errorf("desk spend = %v, want 0.21 (two turns and the notes)", got)
	}
}

// records fills the store with a lane, runs, a report, decisions and a request, some
// carrying a secret.
func (r *rig) records(t *testing.T, secret string) {
	t.Helper()
	ctx := context.Background()
	ws, err := r.st.SaveWorkspace(ctx, r.ws, []store.Repo{{Name: "app", Path: filepath.Join(r.ws.Path, "app")}})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := r.st.Repos(ctx, ws.ID)
	lane, err := r.st.CreateLane(ctx, store.Lane{RepoID: repos[0].ID, Name: "feat/login", Branch: "feat/login", Base: "main",
		Worktree: filepath.Join(r.ws.Path, "wt"), Scope: []string{"src/auth/**"}, State: store.LaneOpen})
	if err != nil {
		t.Fatal(err)
	}
	running, err := r.st.CreateRun(ctx, store.Run{LaneID: lane.ID, Agent: "claude", Prompt: "add the login form, token " + secret, State: store.RunRunning, Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	r.st.AddEvent(ctx, store.Event{RunID: running.ID, Kind: "report", Status: "progress", Text: "form done, tests next"})
	done, _ := r.st.CreateRun(ctx, store.Run{LaneID: lane.ID, Agent: "copilot", Prompt: "fix the crash", State: store.RunRunning, Started: time.Now()})
	done.State, done.Commits = store.RunSucceeded, 2
	r.st.UpdateRun(ctx, done)
	r.st.CreateDecision(ctx, store.Decision{RunID: running.ID, Question: "Use the password=" + secret + " from the vault?", Recommendation: "no"})
	old, _ := r.st.CreateDecision(ctx, store.Decision{RunID: done.ID, Question: "Rename the module?"})
	r.st.AnswerDecision(ctx, old.ID, "keep the name")
	r.st.CreateRequest(ctx, store.Request{FromRun: running.ID, Kind: "review", Message: "review my form", State: store.RequestNeedsRouting})
	r.st.AddSpend(ctx, store.Spend{Day: time.Now().Format("2006-01-02"), Source: "claude", USD: 1.5})
}

// The summary is built from the store, redacted: a secret-shaped string in a run's task
// or a decision never reaches the new session.
func TestSummaryFromTheStoreRedacted(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	r := newRig(t, nil)
	r.records(t, secret)
	d := r.desk(t)
	d.Send(context.Background(), "my key is "+secret)
	r.waitEnded(t, 1)
	d.New(context.Background())
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	last := r.lastArgs(t)
	for _, want := range []string{
		"Waiting for the person", "Use the password=" + redact.Mask,
		"request 1, review from run 1, needs_routing: review my form",
		"Runs in flight:", "claude in app feat/login, running", "last report: progress: form done, tests next",
		"Open lanes:", "app feat/login: open, scope src/auth/**",
		"Standing rules", "repo app: base main, push human, merge human",
		"Decisions the person already answered", "Rename the module? Answer: keep the name",
		"Recent finished runs", "copilot in app feat/login: succeeded, 2 commit(s): fix the crash",
		"Spend today: $1.", "The person:   my key is " + redact.Mask,
	} {
		if !strings.Contains(last, want) {
			t.Errorf("summary lacks %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, secret) {
		t.Errorf("the secret reached the new session:\n%s", last)
	}
}

// The summary keeps within its cap however much the store holds, the records first and
// the quoted turns, newest first, in what room is left.
func TestSummaryCap(t *testing.T) {
	in := summaryInput{now: time.Now(), cfg: config.Default(), lanes: map[int64]laneAt{}}
	for i := 0; i < 8; i++ {
		in.turns = append(in.turns, quotedTurn{origin: store.DeskByHuman, opener: "turn " + strconv.Itoa(i) + " " + strings.Repeat("words ", 100), reply: strings.Repeat("reply ", 100)})
	}
	for _, max := range []int{config.MinSummaryChars, 3000, config.DefaultSummaryChars} {
		s := in.render(max, "")
		if len(s) > max {
			t.Errorf("cap %d: %d characters", max, len(s))
		}
		if !strings.Contains(s, "turn 7") || strings.Contains(s, "turn 0") || !strings.HasSuffix(s, summaryEnd) {
			t.Errorf("cap %d: the newest turns should fill the room:\n%s", max, s)
		}
	}
	// Records alone past the cap: cut, marked, still closed.
	for i := 0; i < 40; i++ {
		in.open = append(in.open, store.Decision{ID: int64(i), Question: strings.Repeat("why ", 80)})
		in.requests = append(in.requests, store.Request{ID: int64(i), Message: strings.Repeat("ask ", 80)})
	}
	s := in.render(config.MinSummaryChars, strings.Repeat("note ", 400))
	if len(s) > config.MinSummaryChars || !strings.Contains(s, strings.TrimSpace(cutMark)) || !strings.HasSuffix(s, summaryEnd) {
		t.Errorf("records past the cap (%d):\n%s", len(s), s)
	}
	if s = in.render(config.DefaultSummaryChars, ""); len(s) > config.DefaultSummaryChars || !strings.Contains(s, "(28 more not listed)") {
		t.Errorf("long lists are not shortened:\n%s", s)
	}
}

// summary_turns 0 quotes none.
func TestSummaryNoTurns(t *testing.T) {
	r := newRig(t, deskConfig(func(d *config.Desk) { d.SummaryTurns = intp(0) }))
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	d.New(context.Background())
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	if last := r.lastArgs(t); !strings.Contains(last, "Summary for the new session") || strings.Contains(last, "The person:") {
		t.Errorf("summary_turns 0:\n%s", last)
	}
}
