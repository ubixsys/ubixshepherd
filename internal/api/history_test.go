package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

func TestLaneOutcome(t *testing.T) {
	for _, c := range []struct {
		state   string
		mr      int
		mrState string
		want    string
	}{
		{store.LaneOpen, 3, MRStateMerged, ""},
		{store.LaneClosed, 0, "", OutcomeNoMR},
		{store.LaneClosed, 3, MRStateMerged, OutcomeMerged},
		{store.LaneClosed, 3, MRStateClosed, OutcomeMRClosed},
		{store.LaneClosed, 3, MRStateOpen, OutcomeDropped},
		{store.LaneClosed, 3, MRStateUnknown, OutcomeDropped},
	} {
		if got := LaneOutcome(c.state, c.mr, c.mrState); got != c.want {
			t.Errorf("LaneOutcome(%q, %d, %q) = %q, want %q", c.state, c.mr, c.mrState, got, c.want)
		}
	}
}

func historyFixture(t *testing.T) (http.Handler, store.Workspace, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ws, err := st.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: "/w"}, []store.Repo{{Name: "r", Path: "/w/r"}})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := st.Repos(ctx, ws.ID)
	ids := map[string]int64{}
	for _, n := range []string{"merged", "closed-mr", "dropped", "bare", "live"} {
		l, err := st.CreateLane(ctx, store.Lane{RepoID: repos[0].ID, Name: n, Branch: n, Worktree: "/w/" + n, State: store.LaneOpen})
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = l.ID
		r, err := st.CreateRun(ctx, store.Run{LaneID: l.ID, Agent: "claude", Prompt: "task for " + n + " glpat-AbCdEfGhIjKlMnOpQrStUv\nmore", State: store.RunRunning, Log: "/l"})
		if err != nil {
			t.Fatal(err)
		}
		r.State, r.CostUSD = store.RunSucceeded, 0.25
		end := r.Started.Add(time.Second)
		r.Ended = &end
		st.UpdateRun(ctx, r)
		if n != "live" {
			st.SetLaneState(ctx, l.ID, store.LaneClosed)
		}
	}
	for n, f := range map[string]store.LaneForge{
		"merged":    {MR: 1, MRState: "merged", MRURL: "https://forge/1"},
		"closed-mr": {MR: 2, MRState: "closed"},
		"dropped":   {MR: 3, MRState: "opened"},
	} {
		f.LaneID = ids[n]
		st.PutLaneForge(ctx, f)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHistoryLanes, HistoryLanes(st))
	mux.HandleFunc("GET "+PathHistoryRuns, HistoryRuns(st))
	return mux, ws, ids
}

func getJSON(t *testing.T, h http.Handler, path string, out any) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if out != nil && rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, rec.Body)
		}
	}
	return rec.Code, rec.Body.String()
}

func TestHistoryLanes(t *testing.T) {
	h, ws, ids := historyFixture(t)
	var got []LaneRecord
	if code, body := getJSON(t, h, fmt.Sprintf("%s?workspace_id=%d", PathHistoryLanes, ws.ID), &got); code != 200 {
		t.Fatalf("code %d: %s", code, body)
	}
	outcomes := map[string]string{}
	for _, l := range got {
		outcomes[l.Name] = l.Outcome
		if l.Runs != 1 || l.CostUSD != 0.25 || l.Closed == nil || l.Repo != "r" {
			t.Errorf("%s = %+v", l.Name, l)
		}
	}
	want := map[string]string{"merged": OutcomeMerged, "closed-mr": OutcomeMRClosed, "dropped": OutcomeDropped, "bare": OutcomeNoMR}
	if fmt.Sprint(outcomes) != fmt.Sprint(want) {
		t.Errorf("outcomes = %v, want %v", outcomes, want)
	}
	for _, l := range got {
		if l.Name == "merged" && (l.MR != 1 || l.MRURL != "https://forge/1" || l.MRState != MRStateMerged) {
			t.Errorf("merged lane forge = %+v", l.LaneView)
		}
	}

	// A lane by id comes back whether or not it is closed, with no workspace needed.
	got = nil
	getJSON(t, h, fmt.Sprintf("%s?lane_id=%d", PathHistoryLanes, ids["live"]), &got)
	if len(got) != 1 || got[0].Name != "live" || got[0].Outcome != "" {
		t.Errorf("by id = %+v", got)
	}
	got = nil
	getJSON(t, h, fmt.Sprintf("%s?workspace_id=%d&since=%s", PathHistoryLanes, ws.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), &got)
	if len(got) != 0 {
		t.Errorf("since the future = %+v", got)
	}
	for _, bad := range []string{"", "?workspace_id=x", fmt.Sprintf("?workspace_id=%d&state=bogus", ws.ID), fmt.Sprintf("?workspace_id=%d&since=yesterday", ws.ID)} {
		if code, _ := getJSON(t, h, PathHistoryLanes+bad, nil); code != 400 {
			t.Errorf("%q: code %d, want 400", bad, code)
		}
	}
}

func TestHistoryRuns(t *testing.T) {
	h, ws, ids := historyFixture(t)
	var got RunHistory
	code, body := getJSON(t, h, fmt.Sprintf("%s?workspace_id=%d", PathHistoryRuns, ws.ID), &got)
	if code != 200 {
		t.Fatalf("code %d: %s", code, body)
	}
	if got.Count != 5 || len(got.Runs) != 5 || got.CostUSD != 1.25 || len(got.Lanes) != 5 || len(got.Agents) != 1 {
		t.Errorf("all = %+v", got)
	}
	if strings.Contains(body, "glpat-") || strings.Contains(body, "more") {
		t.Errorf("prompt leaked past the first line, or unredacted: %s", body)
	}
	if !strings.HasPrefix(got.Runs[0].Task, "task for live") {
		t.Errorf("task = %q", got.Runs[0].Task)
	}

	got = RunHistory{}
	getJSON(t, h, fmt.Sprintf("%s?workspace_id=%d&lane_id=%d&agent=claude&limit=1", PathHistoryRuns, ws.ID, ids["merged"]), &got)
	if got.Count != 1 || got.CostUSD != 0.25 || got.Runs[0].Lane != "merged" {
		t.Errorf("filtered = %+v", got)
	}
	got = RunHistory{}
	getJSON(t, h, fmt.Sprintf("%s?workspace_id=%d&agent=nobody", PathHistoryRuns, ws.ID), &got)
	if got.Count != 0 || got.Runs == nil || len(got.Agents) != 1 {
		t.Errorf("no match = %+v", got)
	}
	if code, _ := getJSON(t, h, PathHistoryRuns, nil); code != 400 {
		t.Errorf("no workspace: code %d", code)
	}
}
