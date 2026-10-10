package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

const briefCols = `id, project, text, state, drafted_by, COALESCE(approved_by, ''), created, COALESCE(approved_at, '')`

func scanBrief(sc interface{ Scan(...any) error }) (store.ProjectBrief, error) {
	var b store.ProjectBrief
	var created, approved string
	err := sc.Scan(&b.ID, &b.Project, &b.Text, &b.State, &b.DraftedBy, &b.ApprovedBy, &created, &approved)
	b.Created = parseTime(created)
	if approved != "" {
		t := parseTime(approved)
		b.Approved = &t
	}
	return b, err
}

func (s *DB) SaveBriefDraft(ctx context.Context, project, text, draftedBy string) (store.ProjectBrief, error) {
	if project == "" || strings.TrimSpace(text) == "" || draftedBy == "" {
		return store.ProjectBrief{}, errors.New("a brief needs a project, text and a drafter")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ProjectBrief{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE project_briefs SET state = ? WHERE project = ? AND state = ?`,
		store.BriefSuperseded, project, store.BriefDraft); err != nil {
		return store.ProjectBrief{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO project_briefs (project, text, state, drafted_by, created) VALUES (?, ?, ?, ?, ?)`,
		project, text, store.BriefDraft, draftedBy, now())
	if err != nil {
		return store.ProjectBrief{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return store.ProjectBrief{}, err
	}
	return s.ProjectBrief(ctx, id)
}

func (s *DB) ApproveBrief(ctx context.Context, id int64, approvedBy string) (store.ProjectBrief, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ProjectBrief{}, err
	}
	defer tx.Rollback()
	b, err := scanBrief(tx.QueryRowContext(ctx, `SELECT `+briefCols+` FROM project_briefs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, store.ErrNotFound
	}
	if err != nil {
		return b, err
	}
	if b.State != store.BriefDraft {
		return b, fmt.Errorf("%w: brief %d is %s, not a draft", store.ErrConflict, id, b.State)
	}
	if approvedBy == "" || approvedBy == b.DraftedBy {
		return b, store.ErrSelfApproval
	}
	t := now()
	if _, err := tx.ExecContext(ctx, `UPDATE project_briefs SET state = ? WHERE project = ? AND state = ?`,
		store.BriefSuperseded, b.Project, store.BriefApproved); err != nil {
		return b, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_briefs SET state = ?, approved_by = ?, approved_at = ? WHERE id = ?`,
		store.BriefApproved, approvedBy, t, id); err != nil {
		return b, err
	}
	if err := tx.Commit(); err != nil {
		return b, err
	}
	return s.ProjectBrief(ctx, id)
}

func (s *DB) oneBrief(ctx context.Context, where string, args ...any) (store.ProjectBrief, error) {
	b, err := scanBrief(s.db.QueryRowContext(ctx, `SELECT `+briefCols+` FROM project_briefs WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return b, store.ErrNotFound
	}
	return b, err
}

func (s *DB) ProjectBrief(ctx context.Context, id int64) (store.ProjectBrief, error) {
	return s.oneBrief(ctx, `id = ?`, id)
}

func (s *DB) ApprovedBrief(ctx context.Context, project string) (store.ProjectBrief, error) {
	return s.oneBrief(ctx, `project = ? AND state = ?`, project, store.BriefApproved)
}

func (s *DB) PendingBrief(ctx context.Context, project string) (store.ProjectBrief, error) {
	return s.oneBrief(ctx, `project = ? AND state = ?`, project, store.BriefDraft)
}

func (s *DB) BriefHistory(ctx context.Context, project string) ([]store.ProjectBrief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+briefCols+` FROM project_briefs WHERE project = ? ORDER BY id DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ProjectBrief
	for rows.Next() {
		b, err := scanBrief(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SpendRollup attributes a spend entry to a repo through its run (ref) and that run's
// lane. The desk's entries and entries with no run are not any repo's.
func (s *DB) SpendRollup(ctx context.Context, from, to string) (store.SpendRollup, error) {
	out := store.SpendRollup{From: from, To: to, Desk: store.Spend{Source: store.OriginDesk}, Other: store.Spend{Source: "other"}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.name, COUNT(*), SUM(sp.usd), SUM(sp.credits)
		FROM spend sp JOIN runs ru ON ru.id = sp.ref JOIN lanes l ON l.id = ru.lane_id JOIN repos r ON r.id = l.repo_id
		WHERE sp.day >= ? AND sp.day <= ? AND sp.ref != 0 AND sp.source != ?
		GROUP BY r.id ORDER BY r.name, r.id`, from, to, store.OriginDesk)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var rs store.RepoSpend
		if err := rows.Scan(&rs.RepoID, &rs.Repo, &rs.Runs, &rs.USD, &rs.Credits); err != nil {
			return out, err
		}
		out.Repos = append(out.Repos, rs)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	// Everything else: the desk, and entries whose run is gone or never had one.
	rest, err := s.db.QueryContext(ctx, `
		SELECT sp.source = ?, COUNT(*), COALESCE(SUM(sp.usd), 0), COALESCE(SUM(sp.credits), 0)
		FROM spend sp
		WHERE sp.day >= ? AND sp.day <= ?
			AND (sp.source = ? OR sp.ref = 0 OR NOT EXISTS (SELECT 1 FROM runs ru JOIN lanes l ON l.id = ru.lane_id WHERE ru.id = sp.ref))
		GROUP BY sp.source = ?`, store.OriginDesk, from, to, store.OriginDesk, store.OriginDesk)
	if err != nil {
		return out, err
	}
	defer rest.Close()
	for rest.Next() {
		var desk bool
		var sp store.Spend
		if err := rest.Scan(&desk, &sp.Ref, &sp.USD, &sp.Credits); err != nil {
			return out, err
		}
		if desk {
			sp.Source = store.OriginDesk
			out.Desk = sp
		} else {
			sp.Source = "other"
			out.Other = sp
		}
	}
	return out, rest.Err()
}
