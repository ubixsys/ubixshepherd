package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestProjectMigrationKeepsOldRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shepherd.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	old := 0 // the schema just before projects
	for !strings.Contains(migrations[old], "project_briefs") {
		old++
	}
	if old != len(migrations)-1 {
		t.Fatalf("projects migration is %d of %d; later migrations need their own test", old+1, len(migrations))
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
		`INSERT INTO requests (from_run, kind, lane, message, state, depth, created, updated)
			VALUES (1, 'question', 'other', 'which port?', 'replied', 2, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO spend (day, source, ref, usd, created) VALUES ('2026-01-01', 'claude', 1, 1.25, '2026-01-01T00:00:00Z')`,
		`INSERT INTO spend (day, source, usd, created) VALUES ('2026-01-01', 'desk', 0.5, '2026-01-01T00:00:00Z')`,
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

	q, err := db.Request(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if q.Kind != "question" || q.Message != "which port?" || q.State != store.RequestReplied || q.Depth != 2 || q.Lane != "other" {
		t.Errorf("old request changed: %+v", q)
	}
	if q.Project != "" || q.FromProject != "" || q.Evidence != nil || q.Touches != nil || q.Version != "" {
		t.Errorf("old request has project fields: %+v", q)
	}
	all, err := db.Requests(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("requests = %+v, %v", all, err)
	}

	by, err := db.SpendOn(ctx, "2026-01-01")
	if err != nil || by["claude"].USD != 1.25 || by["desk"].USD != 0.5 {
		t.Errorf("SpendOn = %+v, %v", by, err)
	}
	roll, err := db.SpendRollup(ctx, "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.RepoSpend{{RepoID: 1, Repo: "a", Runs: 1, USD: 1.25}}
	if !reflect.DeepEqual(roll.Repos, want) || roll.Desk.USD != 0.5 || roll.Other.USD != 0 {
		t.Errorf("rollup = %+v", roll)
	}
}

func TestRequestTypedFields(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	_, _, run := fixture(t, db, "a")

	plain, err := db.CreateRequest(ctx, store.Request{FromRun: run.ID, Kind: store.RequestQuestion, Message: "m", State: store.RequestPending, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	typed, err := db.CreateRequest(ctx, store.Request{
		FromRun: run.ID, Kind: store.RequestBug, Message: "m", State: store.RequestPending, Depth: 1,
		Project: "framework", FromProject: "shop", Evidence: []string{"failing_test", "log"},
		Touches: []string{"internal", "tests"}, Version: "v1.4.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Project != "" || plain.Evidence != nil {
		t.Errorf("plain request = %+v", plain)
	}
	if typed.Project != "framework" || typed.FromProject != "shop" || typed.Version != "v1.4.0" ||
		!reflect.DeepEqual(typed.Evidence, []string{"failing_test", "log"}) || !reflect.DeepEqual(typed.Touches, []string{"internal", "tests"}) {
		t.Errorf("typed request = %+v", typed)
	}
	// An update leaves the typed fields alone.
	typed.State, typed.Reply = store.RequestReplied, "fixed in v1.4.1"
	if err := db.UpdateRequest(ctx, typed); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Request(ctx, typed.ID)
	if got.State != store.RequestReplied || got.Project != "framework" || got.Version != "v1.4.0" {
		t.Errorf("after update = %+v", got)
	}
}

func TestBriefApprovalSupersedes(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)

	if _, err := db.ApprovedBrief(ctx, "p"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no brief yet: %v", err)
	}
	d1, err := db.SaveBriefDraft(ctx, "p", "ship the importer", "desk")
	if err != nil || d1.State != store.BriefDraft || d1.Approved != nil {
		t.Fatalf("draft = %+v, %v", d1, err)
	}
	// An agent's draft is never given out until approved.
	if _, err := db.ApprovedBrief(ctx, "p"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a draft is approved: %v", err)
	}
	a1, err := db.ApproveBrief(ctx, d1.ID, "person")
	if err != nil || a1.State != store.BriefApproved || a1.ApprovedBy != "person" || a1.Approved == nil {
		t.Fatalf("approved = %+v, %v", a1, err)
	}

	d2, _ := db.SaveBriefDraft(ctx, "p", "now the exporter", "desk")
	other, _ := db.SaveBriefDraft(ctx, "q", "another project", "desk")
	a2, err := db.ApproveBrief(ctx, d2.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := db.ApprovedBrief(ctx, "p")
	if err != nil || cur.ID != a2.ID || cur.Text != "now the exporter" {
		t.Errorf("current = %+v, %v", cur, err)
	}
	old, _ := db.ProjectBrief(ctx, a1.ID)
	if old.State != store.BriefSuperseded || old.ApprovedBy != "person" {
		t.Errorf("first brief = %+v", old)
	}
	// Another project's draft is untouched.
	if o, _ := db.ProjectBrief(ctx, other.ID); o.State != store.BriefDraft {
		t.Errorf("other project = %+v", o)
	}
	hist, _ := db.BriefHistory(ctx, "p")
	if len(hist) != 2 || hist[0].ID != a2.ID || hist[1].ID != a1.ID {
		t.Errorf("history = %+v", hist)
	}
	// Approving a brief twice, or a superseded one, is refused.
	for _, id := range []int64{a2.ID, a1.ID} {
		if _, err := db.ApproveBrief(ctx, id, "person"); !errors.Is(err, store.ErrConflict) {
			t.Errorf("approve %d again: %v", id, err)
		}
	}
	if _, err := db.ApproveBrief(ctx, 999, "person"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown brief: %v", err)
	}
	// One approved row per project, whatever the code does.
	if _, err := db.db.ExecContext(ctx, `UPDATE project_briefs SET state = 'approved' WHERE id = ?`, d1.ID); err == nil {
		t.Error("the schema allows two approved briefs for a project")
	}
}

func TestBriefDraftCannotBeApprovedByItsDrafter(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	d, _ := db.SaveBriefDraft(ctx, "p", "focus", "run:12")

	for _, who := range []string{"run:12", ""} {
		if _, err := db.ApproveBrief(ctx, d.ID, who); !errors.Is(err, store.ErrSelfApproval) || !errors.Is(err, store.ErrConflict) {
			t.Errorf("approve as %q: %v", who, err)
		}
	}
	got, _ := db.ProjectBrief(ctx, d.ID)
	if got.State != store.BriefDraft || got.ApprovedBy != "" {
		t.Errorf("draft changed: %+v", got)
	}
	if _, err := db.ApproveBrief(ctx, d.ID, "person"); err != nil {
		t.Errorf("person approves: %v", err)
	}
}

func TestNewDraftSupersedesTheOpenOne(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	d1, _ := db.SaveBriefDraft(ctx, "p", "one", "desk")
	d2, err := db.SaveBriefDraft(ctx, "p", "two", "desk")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ProjectBrief(ctx, d1.ID); got.State != store.BriefSuperseded {
		t.Errorf("first draft = %+v", got)
	}
	if p, err := db.PendingBrief(ctx, "p"); err != nil || p.ID != d2.ID {
		t.Errorf("pending = %+v, %v", p, err)
	}
	if _, err := db.ApproveBrief(ctx, d1.ID, "person"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("approve a superseded draft: %v", err)
	}
	for _, c := range [][3]string{{"", "t", "d"}, {"p", "  ", "d"}, {"p", "t", ""}} {
		if _, err := db.SaveBriefDraft(ctx, c[0], c[1], c[2]); err == nil {
			t.Errorf("draft %q accepted", c)
		}
	}
}

func TestSpendRollup(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	_, a, runA := fixture(t, db, "a")
	_, b, runB := fixture(t, db, "b")
	_ = a
	_ = b

	add := func(day, source string, ref int64, usd, credits float64) {
		t.Helper()
		if err := db.AddSpend(ctx, store.Spend{Day: day, Source: source, Ref: ref, USD: usd, Credits: credits}); err != nil {
			t.Fatal(err)
		}
	}
	add("2026-10-09", "claude", runA.ID, 1, 0)
	add("2026-10-10", "claude", runA.ID, 2, 0)
	add("2026-10-10", "claude", runA.ID, 0.5, 0)
	add("2026-10-10", "copilot", runB.ID, 0, 3)
	add("2026-10-10", "desk", 0, 0.25, 0)
	add("2026-10-10", "session", 0, 4, 0)
	add("2026-10-11", "claude", runB.ID, 100, 0)

	got, err := db.SpendRollup(ctx, "2026-10-10", "2026-10-10")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 2 || got.Repos[0].Repo != "a" || got.Repos[0].USD != 2.5 || got.Repos[0].Runs != 2 ||
		got.Repos[1].Repo != "b" || got.Repos[1].Credits != 3 {
		t.Errorf("repos = %+v", got.Repos)
	}
	if got.Desk.USD != 0.25 || got.Desk.Source != store.OriginDesk {
		t.Errorf("desk = %+v", got.Desk)
	}
	if got.Other.USD != 4 {
		t.Errorf("other = %+v", got.Other)
	}
	if usd, credits := got.Total(); usd != 6.75 || credits != 3 {
		t.Errorf("total = %v, %v", usd, credits)
	}

	// The parts sum to what SpendOn reports for the same day.
	by, _ := db.SpendOn(ctx, "2026-10-10")
	sum, sumCredits := 0.0, 0.0
	for _, sp := range by {
		sum += sp.USD
		sumCredits += sp.Credits
	}
	if usd, credits := got.Total(); usd != sum || credits != sumCredits {
		t.Errorf("rollup total %v/%v, SpendOn total %v/%v", usd, credits, sum, sumCredits)
	}

	// A range, and an empty one.
	wide, _ := db.SpendRollup(ctx, "2026-10-09", "2026-10-11")
	if usd, _ := wide.Total(); usd != 107.75 {
		t.Errorf("wide total = %v", usd)
	}
	none, err := db.SpendRollup(ctx, "2020-01-01", "2020-01-31")
	if err != nil || len(none.Repos) != 0 || none.Desk.USD != 0 {
		t.Errorf("empty = %+v, %v", none, err)
	}
}

// fixture makes a workspace-less repo with a lane and a run, returning the repo and run.
func fixture(t *testing.T, db *DB, name string) (store.Workspace, store.Repo, store.Run) {
	t.Helper()
	ctx := context.Background()
	ws, err := db.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: "/w"}, []store.Repo{{Name: name, Path: "/w/" + name}})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := db.Repos(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	var repo store.Repo
	for _, r := range repos {
		if r.Name == name {
			repo = r
		}
	}
	l, err := db.CreateLane(ctx, store.Lane{RepoID: repo.ID, Name: "l-" + name, Branch: "l-" + name, Worktree: "/w/" + name + "-wt", State: store.LaneOpen})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateRun(ctx, store.Run{LaneID: l.ID, Agent: "claude", State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	return ws, repo, run
}
