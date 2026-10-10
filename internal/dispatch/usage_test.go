package dispatch

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func feedFile(t *testing.T, r UsageReader, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		r.Feed(sc.Text())
	}
}

func TestClaudeUsageFromStream(t *testing.T) {
	// A new session: the result's totals are this run's own.
	fresh := NewClaudeUsage(true)
	feedFile(t, fresh, "testdata/claude_usage.jsonl")
	u := fresh.Usage()
	if u.Model != "claude-opus-4-5[1m]" || u.ContextWindow != 1_000_000 {
		t.Errorf("model %q window %d", u.Model, u.ContextWindow)
	}
	// msg_1 is printed twice and counts once; the subagent's request counts in the
	// totals but never in the peak, which is msg_2's prompt: 5 + 2000 + 230000.
	if u.Requests != 4 || u.PeakContext != 232_005 || u.Compactions != 1 {
		t.Errorf("requests %d, peak %d, compactions %d", u.Requests, u.PeakContext, u.Compactions)
	}
	if u.Input != 300_015 || u.CacheRead != 290_000 || u.CacheCreation != 3_500 || u.Output != 520 {
		t.Errorf("fresh totals = %+v", u)
	}

	// A resumed session reports the whole session's totals in its result: sum the
	// requests this invocation printed instead.
	resumed := NewClaudeUsage(false)
	feedFile(t, resumed, "testdata/claude_usage.jsonl")
	u = resumed.Usage()
	if u.Input != 3+300_000+5+4 || u.CacheRead != 20_000+230_000+40_000 || u.CacheCreation != 1000+2000+500 || u.Output != 45+50+300+120 {
		t.Errorf("resumed totals = %+v", u)
	}
	if u.PeakContext != 232_005 || u.ContextWindow != 1_000_000 {
		t.Errorf("resumed peak %d window %d", u.PeakContext, u.ContextWindow)
	}
}

func TestClaudeUsageIgnoresWhatIsNotUsage(t *testing.T) {
	r := NewClaudeUsage(true)
	for _, line := range []string{"plain text", `{"type":"assistant"`, `{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0}}}`, `{"type":"user"}`} {
		r.Feed(line)
	}
	if u := r.Usage(); u.Measured() || u.Model != "" {
		t.Errorf("usage from nothing = %+v", u)
	}
	// A result alone (no requests printed) still gives the totals, with no peak.
	r.Feed(`{"type":"result","usage":{"input_tokens":7,"cache_read_input_tokens":9,"output_tokens":2},"modelUsage":{"m":{"costUSD":1,"contextWindow":200000}}}`)
	u := r.Usage()
	if u.Requests != 0 || u.Prompt() != 16 || u.Output != 2 || u.PeakContext != 0 || u.Model != "m" || u.ContextWindow != 200_000 {
		t.Errorf("result-only usage = %+v", u)
	}
}

func TestOpenCodeUsageFromSteps(t *testing.T) {
	r := NewOpenCodeUsage(32768)
	for _, line := range []string{
		`{"type":"step_start","sessionID":"s"}`,
		`{"type":"step_finish","sessionID":"s","part":{"reason":"tool-calls","cost":0,"tokens":{"input":1200,"output":80,"reasoning":0,"cache":{"read":0,"write":0}}}}`,
		`{"type":"step_finish","sessionID":"s","part":{"reason":"stop","cost":0,"tokens":{"input":1500,"output":60,"cache":{"read":300,"write":0}}}}`,
		"# opencode ended (stop): 2700 tokens in, 140 out, no billed cost",
	} {
		r.Feed(line)
	}
	u := r.Usage()
	if u.Requests != 2 || u.Input != 2700 || u.CacheRead != 300 || u.Output != 140 || u.PeakContext != 1800 || u.ContextWindow != 32768 {
		t.Errorf("opencode usage = %+v", u)
	}

	// With only the line Shepherd prints at the end of a run, the counts still come.
	r = NewOpenCodeUsage(0)
	r.Feed("# opencode ended (stop): 4100 tokens in, 90 out, no billed cost (a local model has no rates)")
	if u := r.Usage(); u.Input != 4100 || u.Output != 90 || !u.Measured() {
		t.Errorf("ended line usage = %+v", u)
	}
}

func TestRunRecordsUsageFromStream(t *testing.T) {
	f := newFixture(t, "stream")
	ctx := context.Background()
	abs, _ := os.Getwd()
	t.Setenv("STREAM_FILE", abs+"/testdata/claude_usage.jsonl")
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	u, err := f.st.RunUsage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Kind != store.UsageRun || u.Agent != "claude" || u.Model != "claude-opus-4-5[1m]" || u.ContextWindow != 1_000_000 ||
		u.PeakContext != 232_005 || u.Compactions != 1 || u.CostUSD != done.CostUSD || u.CostUSD != 1.5 || u.Day != Today() {
		t.Errorf("usage = %+v", u)
	}
	// It is in the stats, over the 200k line, and on the log's closing lines.
	st, err := f.st.UsageStats(ctx, Today(), Today())
	if err != nil || st.Runs.Count != 1 || st.Runs.Over200k != 1 || st.ByLane[0].Key != "app/work" {
		t.Errorf("stats = %+v, %v", st.Runs, err)
	}
	log, _ := os.ReadFile(done.Log)
	if !strings.Contains(string(log), "# shepherd: usage: claude-opus-4-5[1m]") || !strings.Contains(string(log), "peak context 232k of a 1M window") {
		t.Errorf("log lacks the usage line:\n%s", log)
	}

	// A continued session sums its own requests, not the result's session totals.
	cont, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "more"})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, cont.ID)
	u2, err := f.st.RunUsage(ctx, cont.ID)
	if err != nil || u2.Input != 300_012 {
		t.Errorf("continued usage = %+v, %v", u2, err)
	}
}

// A CLI that reports no tokens still leaves a record, so the run is counted.
func TestRunWithoutTokensStillRecordsUsage(t *testing.T) {
	f := newFixture(t, "quick")
	ctx := context.Background()
	t.Setenv("CREDITS", "2.5")
	run, err := f.runner.Start(ctx, StartRequest{LaneID: f.lane.ID, Agent: "copilot", Prompt: "x", Model: "gpt-x"})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, run.ID)
	u, err := f.st.RunUsage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Agent != "copilot" || u.Model != "gpt-x" || u.Credits != 2.5 || u.Measured() {
		t.Errorf("usage = %+v", u)
	}
	st, _ := f.st.UsageStats(ctx, Today(), Today())
	if st.Runs.Count != 1 || st.Runs.Measured != 0 || st.Runs.Credits != 2.5 {
		t.Errorf("stats = %+v", st.Runs)
	}
}
