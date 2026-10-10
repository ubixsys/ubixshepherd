package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

// Times are stored as RFC 3339 with trailing fractional zeros trimmed, which does not
// sort exactly as text. A text comparison against the second below the cut-off is a
// coarse filter that never drops a match; the exact test is made on the parsed time.
func coarse(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05")
}

// withExtra scans a row's columns after the ones a shared scanner takes.
type withExtra struct {
	rows  *sql.Rows
	extra []any
}

func (w withExtra) Scan(dest ...any) error { return w.rows.Scan(append(dest, w.extra...)...) }

func prefixed(cols, prefix string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

func (s *DB) LaneHistory(ctx context.Context, f store.LaneHistoryFilter) ([]store.LaneSummary, error) {
	state := f.State
	if state == "" {
		state = store.HistoryClosed
	}
	q := `SELECT ` + prefixed(laneCols, "l.") + `, r.name,
			COALESCE(g.mr, 0), COALESCE(g.mr_state, ''), COALESCE(g.mr_url, ''),
			(SELECT COUNT(*) FROM runs WHERE lane_id = l.id),
			(SELECT COALESCE(SUM(cost_usd), 0) FROM runs WHERE lane_id = l.id),
			(SELECT COALESCE(SUM(credits), 0) FROM runs WHERE lane_id = l.id)
		FROM lanes l JOIN repos r ON r.id = l.repo_id LEFT JOIN lane_forge g ON g.lane_id = l.id
		WHERE (? = 0 OR r.workspace_id = ?) AND (? = 0 OR l.repo_id = ?) AND (? = 0 OR l.id = ?)
			AND (? = 'all' OR (? = 'closed') = (l.state = 'closed'))
			AND (? = '' OR l.state != 'closed' OR l.closed >= ?)`
	cut := coarse(f.Since)
	rows, err := s.db.QueryContext(ctx, q, f.WorkspaceID, f.WorkspaceID, f.RepoID, f.RepoID, f.LaneID, f.LaneID,
		state, state, cut, cut)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.LaneSummary
	for rows.Next() {
		var ls store.LaneSummary
		extra := []any{&ls.Repo, &ls.Forge.MR, &ls.Forge.MRState, &ls.Forge.MRURL, &ls.Runs, &ls.CostUSD, &ls.Credits}
		l, err := scanLane(withExtra{rows, extra})
		if err != nil {
			return nil, err
		}
		ls.Lane = l
		ls.Forge.LaneID = l.ID
		if !f.Since.IsZero() && l.State == store.LaneClosed && (l.Closed == nil || l.Closed.Before(f.Since)) {
			continue
		}
		out = append(out, ls)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	closedAt := func(l store.Lane) time.Time {
		if l.Closed == nil {
			return time.Time{}
		}
		return *l.Closed
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if state == store.HistoryClosed {
			if ca, cb := closedAt(a.Lane), closedAt(b.Lane); !ca.Equal(cb) {
				return ca.After(cb)
			}
			return a.ID > b.ID
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Name < b.Name
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// taskLine is the first non-empty line of a prompt, clipped to 200 runes.
func taskLine(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 200 {
			return string(r[:199]) + "…"
		}
		return line
	}
	return ""
}

func (s *DB) RunHistory(ctx context.Context, f store.RunHistoryFilter) (store.RunHistory, error) {
	var h store.RunHistory
	cut := coarse(f.Since)
	// The prompt is clipped in SQL: a task line never needs more, and the totals walk
	// every run in the range.
	q := `SELECT x.id, x.lane_id, r.name, l.name, l.state, x.agent, x.model, x.state, x.commits,
			x.cost_usd, x.credits, x.started, x.ended, substr(x.prompt, 1, 1000)
		FROM runs x JOIN lanes l ON l.id = x.lane_id JOIN repos r ON r.id = l.repo_id
		WHERE (? = 0 OR r.workspace_id = ?) AND (? = 0 OR l.repo_id = ?)
			AND (? = '' OR x.state = ?) AND (? = '' OR x.started >= ?)
		ORDER BY x.id DESC`
	rows, err := s.db.QueryContext(ctx, q, f.WorkspaceID, f.WorkspaceID, f.RepoID, f.RepoID, f.State, f.State, cut, cut)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	agents := map[string]bool{}
	lanes := map[int64]store.HistoryLane{}
	for rows.Next() {
		var r store.RunSummary
		var started, ended, prompt string
		if err := rows.Scan(&r.ID, &r.LaneID, &r.Repo, &r.Lane, &r.LaneState, &r.Agent, &r.Model, &r.State, &r.Commits,
			&r.CostUSD, &r.Credits, &started, &ended, &prompt); err != nil {
			return h, err
		}
		r.Started = parseTime(started)
		if !f.Since.IsZero() && r.Started.Before(f.Since) {
			continue
		}
		if ended != "" {
			t := parseTime(ended)
			r.Ended = &t
		}
		agents[r.Agent] = true
		lanes[r.LaneID] = store.HistoryLane{ID: r.LaneID, Repo: r.Repo, Name: r.Lane}
		if (f.Agent != "" && r.Agent != f.Agent) || (f.LaneID != 0 && r.LaneID != f.LaneID) {
			continue
		}
		h.Count++
		h.CostUSD += r.CostUSD
		h.Credits += r.Credits
		if len(h.Runs) < limit {
			r.Task = taskLine(prompt)
			h.Runs = append(h.Runs, r)
		}
	}
	if err := rows.Err(); err != nil {
		return h, err
	}
	for a := range agents {
		h.Agents = append(h.Agents, a)
	}
	sort.Strings(h.Agents)
	for _, l := range lanes {
		h.Lanes = append(h.Lanes, l)
	}
	sort.Slice(h.Lanes, func(i, j int) bool {
		if h.Lanes[i].Repo != h.Lanes[j].Repo {
			return h.Lanes[i].Repo < h.Lanes[j].Repo
		}
		return h.Lanes[i].Name < h.Lanes[j].Name
	})
	return h, nil
}
