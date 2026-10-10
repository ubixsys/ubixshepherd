package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shepherd.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestSaveWorkspaceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)

	repos := []store.Repo{
		{Name: "alpha", Path: "/w/alpha", Remote: "git@example.com:g/alpha.git", Stacks: []string{"go"}},
		{Name: "beta", Path: "/w/beta"},
	}
	ws, err := db.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: "/w"}, repos)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID == 0 || ws.Created.IsZero() {
		t.Errorf("workspace not filled in: %+v", ws)
	}

	// Running init again with one more repo and a new name keeps the id and the old repos.
	again, err := db.SaveWorkspace(ctx, store.Workspace{Name: "git", Path: "/w"},
		[]store.Repo{{Name: "gamma", Path: "/w/gamma", Stacks: []string{"node", "php"}}})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != ws.ID || again.Name != "git" {
		t.Errorf("second save = %+v, first = %+v", again, ws)
	}

	got, err := db.Repos(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("repos = %+v", got)
	}
	if got[0].Name != "alpha" || got[0].Stacks[0] != "go" || got[0].Remote == "" {
		t.Errorf("alpha = %+v", got[0])
	}
	if got[1].Stacks == nil {
		t.Error("stacks should decode to an empty slice, not nil")
	}

	all, err := db.Workspaces(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("workspaces = %+v, %v", all, err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	db, path := open(t)
	if _, err := db.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: "/w"}, nil); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	ws, err := db2.Workspaces(ctx)
	if err != nil || len(ws) != 1 {
		t.Errorf("after reopen: %+v, %v", ws, err)
	}
}

// A store from before lanes had an origin migrates, and its lanes read as unknown.
func TestLaneOriginMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shepherd.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	old := 0 // the schema just before lanes had an origin
	for !strings.Contains(migrations[old], "origin_via") {
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
		`INSERT INTO lanes (repo_id, name, branch, worktree, state, created) VALUES (1, 'old', 'old', '/w/a-worktrees/old', 'open', '2026-01-01T00:00:00Z')`,
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
	o := store.Origin{Via: store.OriginDesk, Agent: "claude", Session: "s-1", Run: 7, PID: 42, Dir: "/w", Detail: "d"}
	if _, err := db.CreateLane(ctx, store.Lane{RepoID: 1, Name: "new", Branch: "new", Worktree: "/w/a-worktrees/new", State: store.LaneOpen, Origin: o}); err != nil {
		t.Fatal(err)
	}
	lanes, err := db.Lanes(ctx, 1)
	if err != nil || len(lanes) != 2 {
		t.Fatalf("lanes = %+v, %v", lanes, err)
	}
	if lanes[1].Name != "old" || lanes[1].Origin != (store.Origin{}) || lanes[1].Origin.String() != "unknown" {
		t.Errorf("migrated lane origin = %+v", lanes[1].Origin)
	}
	if lanes[0].Origin != o {
		t.Errorf("new lane origin = %+v, want %+v", lanes[0].Origin, o)
	}
}

func TestLanesEmpty(t *testing.T) {
	db, _ := open(t)
	l, err := db.Lanes(context.Background(), 1)
	if err != nil || len(l) != 0 {
		t.Errorf("lanes = %+v, %v", l, err)
	}
}

func TestDeskEvents(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 5; i++ {
		if _, err := s.AddDeskEvent(ctx, store.DeskEvent{WorkspaceID: 1, Kind: store.DeskUser, Text: fmt.Sprint(i), Origin: store.DeskByHuman}); err != nil {
			t.Fatal(err)
		}
	}
	s.AddDeskEvent(ctx, store.DeskEvent{WorkspaceID: 2, Kind: store.DeskUser, Text: "other"})
	after, _ := s.DeskEvents(ctx, 1, 2, 10)
	if len(after) != 3 || after[0].Seq != 3 || after[0].Text != "2" {
		t.Errorf("after 2 = %+v", after)
	}
	hist, _ := s.DeskHistory(ctx, 1, 5, 2)
	if len(hist) != 2 || hist[0].Seq != 3 || hist[1].Seq != 4 {
		t.Errorf("history before 5 = %+v", hist)
	}
	if err := s.TrimDeskEvents(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	all, _ := s.DeskEvents(ctx, 1, 0, 10)
	if len(all) != 2 || all[0].Seq != 4 {
		t.Errorf("after trim = %+v", all)
	}
	if other, _ := s.DeskEvents(ctx, 2, 0, 10); len(other) != 1 {
		t.Errorf("trim touched another workspace: %+v", other)
	}
}

// history builds two repos with three lanes (one closed with a merged MR, one closed with
// none, one open) and runs of two agents, then backdates what the tests filter on.
func history(t *testing.T) (*DB, store.Workspace, map[string]store.Lane) {
	t.Helper()
	ctx := context.Background()
	db, _ := open(t)
	ws, err := db.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: "/w"},
		[]store.Repo{{Name: "alpha", Path: "/w/alpha"}, {Name: "beta", Path: "/w/beta"}})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := db.Repos(ctx, ws.ID)
	lanes := map[string]store.Lane{}
	for _, c := range []struct {
		name string
		repo int
	}{{"merged", 0}, {"dropped", 0}, {"live", 1}} {
		l, err := db.CreateLane(ctx, store.Lane{RepoID: repos[c.repo].ID, Name: c.name, Branch: c.name, Worktree: "/w/" + c.name, State: store.LaneOpen})
		if err != nil {
			t.Fatal(err)
		}
		lanes[c.name] = l
	}
	add := func(lane, agent, state, prompt string, usd, credits float64, commits int) store.Run {
		r, err := db.CreateRun(ctx, store.Run{LaneID: lanes[lane].ID, Agent: agent, Prompt: prompt, State: store.RunRunning, Log: "/log"})
		if err != nil {
			t.Fatal(err)
		}
		r.State, r.CostUSD, r.Credits, r.Commits = state, usd, credits, commits
		end := r.Started.Add(time.Minute)
		r.Ended = &end
		if err := db.UpdateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	add("merged", "claude", store.RunSucceeded, "\n  Fix the login\nmore detail", 1.5, 0, 2)
	add("merged", "claude", store.RunFailed, "second try", 0.5, 0, 0)
	add("dropped", "copilot", store.RunStopped, "Try the thing", 0, 3, 0)
	add("live", "claude", store.RunSucceeded, "Work on live", 2, 0, 1)
	if err := db.PutLaneForge(ctx, store.LaneForge{LaneID: lanes["merged"].ID, MR: 7, MRState: "merged", MRURL: "https://f/7"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"merged", "dropped"} {
		if err := db.SetLaneState(ctx, lanes[n].ID, store.LaneClosed); err != nil {
			t.Fatal(err)
		}
	}
	return db, ws, lanes
}

func TestLaneHistory(t *testing.T) {
	ctx := context.Background()
	db, ws, lanes := history(t)

	got, err := db.LaneHistory(ctx, store.LaneHistoryFilter{WorkspaceID: ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("closed lanes = %+v", got)
	}
	var merged store.LaneSummary
	for _, l := range got {
		if l.State != store.LaneClosed || l.Closed == nil {
			t.Errorf("%s: state %q closed %v", l.Name, l.State, l.Closed)
		}
		if l.Name == "merged" {
			merged = l
		}
	}
	if merged.Repo != "alpha" || merged.Runs != 2 || merged.CostUSD != 2 || merged.Forge.MR != 7 || merged.Forge.MRState != "merged" {
		t.Errorf("merged lane = %+v", merged)
	}
	for _, l := range got {
		if l.Name == "dropped" && (l.Runs != 1 || l.Credits != 3 || l.Forge.MR != 0) {
			t.Errorf("dropped lane = %+v", l)
		}
	}

	// Newest closed first.
	back := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := db.db.Exec(`UPDATE lanes SET closed = ? WHERE id = ?`, back, lanes["merged"].ID); err != nil {
		t.Fatal(err)
	}
	got, _ = db.LaneHistory(ctx, store.LaneHistoryFilter{WorkspaceID: ws.ID})
	if got[0].Name != "dropped" || got[1].Name != "merged" {
		t.Errorf("order = %s, %s", got[0].Name, got[1].Name)
	}
	// Since drops the older one.
	got, _ = db.LaneHistory(ctx, store.LaneHistoryFilter{WorkspaceID: ws.ID, Since: time.Now().Add(-24 * time.Hour)})
	if len(got) != 1 || got[0].Name != "dropped" {
		t.Errorf("since = %+v", got)
	}
	// Open and all.
	got, _ = db.LaneHistory(ctx, store.LaneHistoryFilter{WorkspaceID: ws.ID, State: store.HistoryOpen})
	if len(got) != 1 || got[0].Name != "live" || got[0].Closed != nil {
		t.Errorf("open = %+v", got)
	}
	got, _ = db.LaneHistory(ctx, store.LaneHistoryFilter{WorkspaceID: ws.ID, State: store.HistoryAll, Since: time.Now().Add(-24 * time.Hour)})
	if len(got) != 2 {
		t.Errorf("all since = %+v", got)
	}
	// One lane by id, and another workspace sees nothing.
	got, _ = db.LaneHistory(ctx, store.LaneHistoryFilter{LaneID: lanes["live"].ID, State: store.HistoryAll})
	if len(got) != 1 || got[0].Name != "live" {
		t.Errorf("by id = %+v", got)
	}
	got, _ = db.LaneHistory(ctx, store.LaneHistoryFilter{WorkspaceID: ws.ID + 1, State: store.HistoryAll})
	if len(got) != 0 {
		t.Errorf("other workspace = %+v", got)
	}
}

func TestRunHistory(t *testing.T) {
	ctx := context.Background()
	db, ws, lanes := history(t)

	h, err := db.RunHistory(ctx, store.RunHistoryFilter{WorkspaceID: ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	if h.Count != 4 || len(h.Runs) != 4 || h.CostUSD != 4 || h.Credits != 3 {
		t.Fatalf("all = count %d runs %d usd %v credits %v", h.Count, len(h.Runs), h.CostUSD, h.Credits)
	}
	if h.Runs[0].Task != "Work on live" || h.Runs[0].LaneState != store.LaneOpen || h.Runs[0].Repo != "beta" {
		t.Errorf("newest = %+v", h.Runs[0])
	}
	if h.Runs[3].Task != "Fix the login" || h.Runs[3].Lane != "merged" {
		t.Errorf("task line = %+v", h.Runs[3])
	}
	if strings.Join(h.Agents, ",") != "claude,copilot" || len(h.Lanes) != 3 {
		t.Errorf("facets = %v %+v", h.Agents, h.Lanes)
	}

	// Filters narrow the runs and the totals, not what a filter offers.
	h, _ = db.RunHistory(ctx, store.RunHistoryFilter{WorkspaceID: ws.ID, Agent: "claude", LaneID: lanes["merged"].ID})
	if h.Count != 2 || h.CostUSD != 2 || len(h.Agents) != 2 || len(h.Lanes) != 3 {
		t.Errorf("filtered = %+v", h)
	}
	h, _ = db.RunHistory(ctx, store.RunHistoryFilter{WorkspaceID: ws.ID, State: store.RunFailed})
	if h.Count != 1 || h.Runs[0].State != store.RunFailed {
		t.Errorf("failed = %+v", h)
	}
	// Limit caps the rows, not the totals.
	h, _ = db.RunHistory(ctx, store.RunHistoryFilter{WorkspaceID: ws.ID, Limit: 1})
	if len(h.Runs) != 1 || h.Count != 4 || h.CostUSD != 4 {
		t.Errorf("limit = %+v", h)
	}

	// Since keeps runs started at or after it.
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := db.db.Exec(`UPDATE runs SET started = ? WHERE lane_id = ?`, old, lanes["merged"].ID); err != nil {
		t.Fatal(err)
	}
	h, _ = db.RunHistory(ctx, store.RunHistoryFilter{WorkspaceID: ws.ID, Since: time.Now().Add(-7 * 24 * time.Hour)})
	if h.Count != 2 || h.CostUSD != 2 || len(h.Lanes) != 2 {
		t.Errorf("since = count %d usd %v lanes %+v", h.Count, h.CostUSD, h.Lanes)
	}
}
