package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

const usageCols = `id, day, kind, run_id, workspace_id, turn, agent, model, context_window, requests,
	input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, peak_context, compactions,
	cost_usd, credits, created`

func scanUsage(sc interface{ Scan(...any) error }, extra ...any) (store.Usage, error) {
	var u store.Usage
	var created string
	dst := append([]any{&u.ID, &u.Day, &u.Kind, &u.RunID, &u.WorkspaceID, &u.Turn, &u.Agent, &u.Model, &u.ContextWindow,
		&u.Requests, &u.Input, &u.CacheRead, &u.CacheCreation, &u.Output, &u.PeakContext, &u.Compactions,
		&u.CostUSD, &u.Credits, &created}, extra...)
	if err := sc.Scan(dst...); err != nil {
		return u, err
	}
	u.Created = parseTime(created)
	return u, nil
}

// AddUsage stores a usage record. A run's record replaces any earlier one for the run.
func (s *DB) AddUsage(ctx context.Context, u store.Usage) error {
	if u.Kind == "" {
		u.Kind = store.UsageRun
		if u.RunID == 0 {
			u.Kind = store.UsageDesk
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if u.RunID != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage WHERE run_id = ?`, u.RunID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage (day, kind, run_id, workspace_id, turn, agent, model, context_window, requests,
			input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, peak_context, compactions,
			cost_usd, credits, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Day, u.Kind, u.RunID, u.WorkspaceID, u.Turn, u.Agent, u.Model, u.ContextWindow, u.Requests,
		u.Input, u.CacheRead, u.CacheCreation, u.Output, u.PeakContext, u.Compactions,
		u.CostUSD, u.Credits, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// RunUsage returns the usage recorded for a run.
func (s *DB) RunUsage(ctx context.Context, runID int64) (store.Usage, error) {
	u, err := scanUsage(s.db.QueryRowContext(ctx, `SELECT `+usageCols+` FROM usage WHERE run_id = ? AND run_id != 0`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return u, store.ErrNotFound
	}
	return u, err
}

// UsageStats totals the days from to to inclusive. A run's lane is read through the run,
// so a record whose run is gone is counted with no lane.
func (s *DB) UsageStats(ctx context.Context, from, to string) (store.UsageStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed(usageCols, "u.")+`, COALESCE(l.name, ''), COALESCE(r.name, '')
		FROM usage u
		LEFT JOIN runs ru ON ru.id = u.run_id AND u.run_id != 0
		LEFT JOIN lanes l ON l.id = ru.lane_id
		LEFT JOIN repos r ON r.id = l.repo_id
		WHERE u.day >= ? AND u.day <= ?
		ORDER BY u.id`, from, to)
	if err != nil {
		return store.UsageStats{}, err
	}
	defer rows.Close()
	var all []store.UsageRow
	for rows.Next() {
		var row store.UsageRow
		if row.Usage, err = scanUsage(rows, &row.Lane, &row.Repo); err != nil {
			return store.UsageStats{}, err
		}
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		return store.UsageStats{}, err
	}
	return store.RollupUsage(from, to, all), nil
}
