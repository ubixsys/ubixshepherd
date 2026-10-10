package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// History routes (GET, read-only): what ran and how it ended, including lanes that have
// closed, which GET /v1/lanes and GET /v1/runs leave out or cut off.
const (
	PathHistoryLanes = "/v1/history/lanes"
	PathHistoryRuns  = "/v1/history/runs"
)

// How a closed lane ended, in a LaneRecord's outcome. Shepherd does not record a close as
// forced or not; the outcome is what the forge last said about the lane's merge request.
const (
	OutcomeMerged   = "merged"    // the forge reports the merge request merged
	OutcomeMRClosed = "mr_closed" // the merge request was closed without a merge
	OutcomeDropped  = "dropped"   // the lane closed while its merge request was still open
	OutcomeNoMR     = "no_mr"     // the lane never had a merge request
)

// LaneOutcome says how a lane ended, from the forge's last word on its merge request
// (mr 0 for none, state as MRState*). "" for a lane that is not closed.
func LaneOutcome(laneState string, mr int, mrState string) string {
	switch {
	case laneState != store.LaneClosed:
		return ""
	case mr == 0:
		return OutcomeNoMR
	case mrState == MRStateMerged:
		return OutcomeMerged
	case mrState == MRStateClosed:
		return OutcomeMRClosed
	}
	return OutcomeDropped
}

// LaneRecord is a lane in the history: the lane view, its runs' count and cost, and for a
// closed lane how it ended. Closed is the lane's own close time.
type LaneRecord struct {
	LaneView
	Outcome string  `json:"outcome,omitempty"`
	Runs    int     `json:"runs"`
	CostUSD float64 `json:"cost_usd"`
	Credits float64 `json:"credits,omitempty"`
}

// RunRecord is a run in the history: the run without its prompt, with the first line of
// it as the task.
type RunRecord = store.RunSummary

// RunHistory answers GET /v1/history/runs. Count, USD and Credits cover every matching
// run, so a total is right when Runs is cut at the limit.
type RunHistory struct {
	Runs    []RunRecord         `json:"runs"`
	Count   int                 `json:"count"`
	CostUSD float64             `json:"cost_usd"`
	Credits float64             `json:"credits"`
	Agents  []string            `json:"agents"`
	Lanes   []store.HistoryLane `json:"lanes"`
}

// HistoryLanes serves PathHistoryLanes from the store:
//
//	workspace_id=N  the workspace (required unless lane_id is given)
//	repo_id=N       one repo
//	lane_id=N       one lane, closed or not
//	state=S         closed (the default), open or all
//	since=TIME      RFC 3339: closed lanes that closed at or after it
//	limit=N         most lanes returned (default 200, at most 1000)
//
// Closed lanes come newest first. The daemon registers it on "GET /v1/history/lanes".
func HistoryLanes(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.LaneHistoryFilter{State: q.Get("state"), Limit: 200}
		var ok bool
		if f.WorkspaceID, ok = number(w, q.Get("workspace_id"), "workspace_id"); !ok {
			return
		}
		if f.RepoID, ok = number(w, q.Get("repo_id"), "repo_id"); !ok {
			return
		}
		if f.LaneID, ok = number(w, q.Get("lane_id"), "lane_id"); !ok {
			return
		}
		if f.WorkspaceID == 0 && f.LaneID == 0 {
			historyError(w, http.StatusBadRequest, errors.New("workspace_id is required"))
			return
		}
		switch f.State {
		case "":
			f.State = store.HistoryClosed
		case store.HistoryClosed, store.HistoryOpen, store.HistoryAll:
		default:
			historyError(w, http.StatusBadRequest, errors.New("state must be closed, open or all"))
			return
		}
		if f.LaneID != 0 {
			// A lane asked for by id is returned whatever its state.
			f.State = store.HistoryAll
		}
		if f.Since, ok = since(w, q.Get("since")); !ok {
			return
		}
		if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
			f.Limit = min(n, 1000)
		}
		lanes, err := st.LaneHistory(r.Context(), f)
		if err != nil {
			historyError(w, http.StatusInternalServerError, err)
			return
		}
		out := make([]LaneRecord, 0, len(lanes))
		for _, l := range lanes {
			rec := LaneRecord{
				LaneView: LaneView{Lane: l.Lane, Repo: l.Repo, MR: l.Forge.MR, MRURL: l.Forge.MRURL, MRState: MRState(l.Forge.MRState)},
				Outcome:  LaneOutcome(l.State, l.Forge.MR, MRState(l.Forge.MRState)),
				Runs:     l.Runs, CostUSD: l.CostUSD, Credits: l.Credits,
			}
			out = append(out, rec)
		}
		writeHistory(w, out)
	}
}

// HistoryRuns serves PathHistoryRuns from the store:
//
//	workspace_id=N  the workspace (required)
//	repo_id=N, lane_id=N, agent=NAME, state=STATE   filters
//	since=TIME      RFC 3339: runs that started at or after it
//	limit=N         most runs listed (default 500, at most 1000); the totals cover all
//
// The daemon registers it on "GET /v1/history/runs".
func HistoryRuns(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.RunHistoryFilter{Agent: q.Get("agent"), State: q.Get("state"), Limit: 500}
		var ok bool
		if f.WorkspaceID, ok = number(w, q.Get("workspace_id"), "workspace_id"); !ok {
			return
		}
		if f.RepoID, ok = number(w, q.Get("repo_id"), "repo_id"); !ok {
			return
		}
		if f.LaneID, ok = number(w, q.Get("lane_id"), "lane_id"); !ok {
			return
		}
		if f.WorkspaceID == 0 {
			historyError(w, http.StatusBadRequest, errors.New("workspace_id is required"))
			return
		}
		if f.Since, ok = since(w, q.Get("since")); !ok {
			return
		}
		if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
			f.Limit = min(n, 1000)
		}
		h, err := st.RunHistory(r.Context(), f)
		if err != nil {
			historyError(w, http.StatusInternalServerError, err)
			return
		}
		out := RunHistory{Runs: h.Runs, Count: h.Count, CostUSD: h.CostUSD, Credits: h.Credits, Agents: h.Agents, Lanes: h.Lanes}
		if out.Runs == nil {
			out.Runs = []RunRecord{}
		}
		if out.Agents == nil {
			out.Agents = []string{}
		}
		if out.Lanes == nil {
			out.Lanes = []store.HistoryLane{}
		}
		for i := range out.Runs {
			out.Runs[i].Task = redact.String(out.Runs[i].Task)
		}
		writeHistory(w, out)
	}
}

func number(w http.ResponseWriter, v, name string) (int64, bool) {
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		historyError(w, http.StatusBadRequest, errors.New("bad "+name))
		return 0, false
	}
	return n, true
}

func since(w http.ResponseWriter, v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		historyError(w, http.StatusBadRequest, errors.New("since must be an RFC 3339 time"))
		return time.Time{}, false
	}
	return t, true
}

func writeHistory(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func historyError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(Error{Error: redact.String(err.Error())})
}
