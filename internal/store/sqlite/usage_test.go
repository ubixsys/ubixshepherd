package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestUsageRollup(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	_, _, runA := fixture(t, db, "a")
	_, _, runB := fixture(t, db, "b")

	add := func(u store.Usage) {
		t.Helper()
		if err := db.AddUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	add(store.Usage{Day: "2026-10-10", RunID: runA.ID, Agent: "claude", Model: "opus", ContextWindow: 1_000_000, Requests: 3,
		Input: 10, CacheRead: 600_000, CacheCreation: 5_000, Output: 700, PeakContext: 250_000, Compactions: 1, CostUSD: 2})
	add(store.Usage{Day: "2026-10-10", RunID: runB.ID, Agent: "claude", Model: "opus", ContextWindow: 200_000, Requests: 2,
		Input: 5, CacheRead: 50_000, Output: 100, PeakContext: 60_000, CostUSD: 1})
	add(store.Usage{Day: "2026-10-10", Kind: store.UsageDesk, WorkspaceID: 1, Turn: 4, Agent: "claude", Model: "sonnet", Requests: 1,
		Input: 3, CacheRead: 210_000, Output: 40, PeakContext: 210_003, CostUSD: 0.5})
	// A CLI that reports nothing: counted, not measured.
	add(store.Usage{Day: "2026-10-10", RunID: 0, Kind: store.UsageRun, Agent: "cursor", CostUSD: 0.25})
	add(store.Usage{Day: "2026-10-11", RunID: 0, Kind: store.UsageDesk, Agent: "claude", Model: "sonnet", CostUSD: 99})

	got, err := db.UsageStats(ctx, "2026-10-10", "2026-10-10")
	if err != nil {
		t.Fatal(err)
	}
	if got.Total.Count != 4 || got.Total.CostUSD != 3.75 {
		t.Errorf("total = %+v", got.Total)
	}
	if r := got.Runs; r.Count != 3 || r.Measured != 2 || r.Over200k != 1 || r.PeakContext != 250_000 || r.Compactions != 1 ||
		r.CacheRead != 650_000 || r.ContextWindow != 1_000_000 {
		t.Errorf("runs = %+v", r)
	}
	if d := got.Desk; d.Count != 1 || d.Over200k != 1 || d.PeakContext != 210_003 || d.CostUSD != 0.5 {
		t.Errorf("desk = %+v", d)
	}
	if len(got.ByModel) != 2 || got.ByModel[0].Key != "claude opus" || got.ByModel[0].Count != 2 || got.ByModel[1].Agent != "cursor" ||
		got.ByModel[1].Key != "cursor default model" {
		t.Errorf("by model = %+v", got.ByModel)
	}
	if len(got.ByLane) != 3 || got.ByLane[0].Key != "(no lane)" || got.ByLane[1].Key != "a/l-a" || got.ByLane[1].PeakContext != 250_000 ||
		got.ByLane[2].Key != "b/l-b" {
		t.Errorf("by lane = %+v", got.ByLane)
	}
	if len(got.DeskByModel) != 1 || got.DeskByModel[0].Key != "sonnet" {
		t.Errorf("desk by model = %+v", got.DeskByModel)
	}

	none, err := db.UsageStats(ctx, "2020-01-01", "2020-01-31")
	if err != nil || none.Total.Count != 0 {
		t.Errorf("empty range = %+v, %v", none, err)
	}
}

func TestRunUsageIsReplacedAndReadBack(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	_, _, run := fixture(t, db, "a")
	if _, err := db.RunUsage(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no usage yet: %v", err)
	}
	for _, peak := range []int64{10, 20} {
		if err := db.AddUsage(ctx, store.Usage{Day: "2026-10-10", RunID: run.ID, Agent: "claude", PeakContext: peak, Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	u, err := db.RunUsage(ctx, run.ID)
	if err != nil || u.PeakContext != 20 || u.Kind != store.UsageRun {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	st, _ := db.UsageStats(ctx, "2026-10-10", "2026-10-10")
	if st.Total.Count != 1 {
		t.Errorf("a re-recorded run counted %d times", st.Total.Count)
	}
}

// A store from before usage was kept migrates with its runs and spend intact.
func TestUsageMigrationKeepsOldRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shepherd.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	old := 0
	for !strings.Contains(migrations[old], "CREATE TABLE usage") {
		old++
	}
	for i := 0; i < old; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, old),
		`INSERT INTO workspaces (name, path, created) VALUES ('w', '/w', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repos (workspace_id, name, path, created) VALUES (1, 'a', '/w/a', '2026-01-01T00:00:00Z')`,
		`INSERT INTO lanes (repo_id, name, branch, worktree, state, created) VALUES (1, 'l', 'l', '/w/a-wt/l', 'open', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs (lane_id, agent, prompt, state, log, started, cost_usd) VALUES (1, 'claude', 'p', 'done', '', '2026-01-01T00:00:00Z', 1.25)`,
		`INSERT INTO spend (day, source, ref, usd, created) VALUES ('2026-01-01', 'claude', 1, 1.25, '2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run, err := db.Run(ctx, 1)
	if err != nil || run.CostUSD != 1.25 {
		t.Fatalf("old run = %+v, %v", run, err)
	}
	if _, err := db.RunUsage(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an old run has no usage: %v", err)
	}
	st, err := db.UsageStats(ctx, "2026-01-01", "2026-01-01")
	if err != nil || st.Total.Count != 0 {
		t.Errorf("stats = %+v, %v", st, err)
	}
}
