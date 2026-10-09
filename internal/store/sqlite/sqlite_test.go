package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

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
