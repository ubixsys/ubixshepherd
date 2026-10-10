package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

// seedUsage records a lane with two claude runs (one past 200k, on a 1M window) and a
// desk turn, and returns the first run's id.
func seedUsage(t *testing.T, h *harness) int64 {
	t.Helper()
	ctx := context.Background()
	st := h.srv.Store
	ws, err := st.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: "/w"}, []store.Repo{{Name: "app", Path: "/w/app"}})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := st.Repos(ctx, ws.ID)
	lane, err := st.CreateLane(ctx, store.Lane{RepoID: repos[0].ID, Name: "feat/x", Branch: "feat/x", Worktree: "/w/app-wt/x", State: store.LaneOpen})
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	var first int64
	for i, u := range []store.Usage{
		{Agent: "claude", Model: "claude-opus-4-5[1m]", ContextWindow: 1_000_000, Requests: 9, Input: 40, CacheRead: 1_200_000, CacheCreation: 9_000, Output: 3_000, PeakContext: 312_000, Compactions: 1, CostUSD: 4.5},
		{Agent: "claude", Model: "claude-sonnet-4-5", ContextWindow: 200_000, Requests: 3, Input: 12, CacheRead: 90_000, Output: 800, PeakContext: 41_000, CostUSD: 0.4},
	} {
		run, err := st.CreateRun(ctx, store.Run{LaneID: lane.ID, Agent: "claude", Prompt: "p", State: store.RunSucceeded})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = run.ID
		}
		u.Day, u.Kind, u.RunID = today, store.UsageRun, run.ID
		if err := st.AddUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddUsage(ctx, store.Usage{Day: today, Kind: store.UsageDesk, WorkspaceID: ws.ID, Turn: 1, Agent: "claude", Model: "claude-sonnet-4-5",
		Requests: 2, Input: 6, CacheRead: 230_000, Output: 90, PeakContext: 230_006, CostUSD: 1.25}); err != nil {
		t.Fatal(err)
	}
	return first
}

func TestStatsAndRunUsageLine(t *testing.T) {
	h := newHarness(t, "", false)
	id := seedUsage(t, h)

	if code := h.run("stats", "--days", "3"); code != 0 {
		t.Fatalf("stats: %s", h.err)
	}
	out := h.out.String()
	for _, want := range []string{
		"Agent runs: 2 runs (2 with token counts), $4.90",
		"peak context 312k, 1 over 200k, 1 compaction(s), largest window 1M",
		"claude claude-opus-4-5[1m]", "app/feat/x",
		"Front desk: 1 turns (1 with token counts), $1.25",
		"peak context 230k, 1 over 200k",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lacks %q:\n%s", want, out)
		}
	}

	if code := h.run("stats", "--json"); code != 0 {
		t.Fatalf("stats --json: %s", h.err)
	}
	var st store.UsageStats
	if err := json.Unmarshal(h.out.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Total.Count != 3 || st.Runs.Over200k != 1 || st.Desk.PeakContext != 230_006 || st.Total.CostUSD != 6.15 {
		t.Errorf("json stats = %+v", st.Total)
	}

	// A range with nothing in it says so; a bad one is refused.
	if code := h.run("stats", "--from", "2020-01-01", "--to", "2020-01-02"); code != 0 || !strings.Contains(h.out.String(), "Nothing recorded") {
		t.Errorf("empty range: %d %s %s", code, h.out, h.err)
	}
	if code := h.run("stats", "--from", "2026-13-01"); code == 0 {
		t.Error("a bad day was accepted")
	}
	if code := h.run("stats", "--from", "2026-10-10", "--to", "2026-10-01"); code == 0 {
		t.Error("a backwards range was accepted")
	}

	if code := h.run("run", "show", fmt.Sprint(id)); code != 0 {
		t.Fatalf("run show: %s", h.err)
	}
	if want := "usage     claude-opus-4-5[1m]; 9 request(s); peak context 312k of a 1M window; in 40, cache read 1.2M, cache write 9k, out 3k; 1 compaction(s)"; !strings.Contains(h.out.String(), want) {
		t.Errorf("run show lacks the usage line:\n%s", h.out)
	}
}

func TestUsageStatsTool(t *testing.T) {
	h := newHarness(t, "", false)
	seedUsage(t, h)
	resps := mcpExchange(t, h.env,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"usage_stats","arguments":{"days":2}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"usage_stats","arguments":{"from":"bad"}}}`,
	)
	text, isErr := toolText(t, resps[0])
	if isErr || !strings.Contains(text, "Front desk: 1 turns") || !strings.Contains(text, "1 over 200k") {
		t.Errorf("usage_stats: %v %s", isErr, text)
	}
	if text, isErr = toolText(t, resps[1]); !isErr || !strings.Contains(text, "not a day") {
		t.Errorf("a bad day: %v %s", isErr, text)
	}
}
