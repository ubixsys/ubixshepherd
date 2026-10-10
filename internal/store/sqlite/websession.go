package sqlite

import (
	"context"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func (s *DB) SaveWebSession(ctx context.Context, ws store.WebSession) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO web_sessions (hash, csrf, port, created, last_seen, expires)
		VALUES (?, ?, ?, ?, ?, ?)`,
		ws.Hash, ws.CSRF, ws.Port, tstr(ws.Created), tstr(ws.LastSeen), tstr(ws.Expires))
	return err
}

func (s *DB) TouchWebSession(ctx context.Context, hash string, seen time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE web_sessions SET last_seen = ? WHERE hash = ?`, tstr(seen), hash)
	return err
}

func (s *DB) DeleteWebSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE hash = ?`, hash)
	return err
}

func (s *DB) DeleteWebSessions(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *DB) LiveWebSessions(ctx context.Context, now time.Time) ([]store.WebSession, error) {
	// Times are stored as RFC3339 in UTC at one precision, so they sort as text.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires <= ?`, tstr(now)); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT hash, csrf, port, created, last_seen, expires FROM web_sessions ORDER BY created, hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.WebSession
	for rows.Next() {
		var ws store.WebSession
		var created, seen, expires string
		if err := rows.Scan(&ws.Hash, &ws.CSRF, &ws.Port, &created, &seen, &expires); err != nil {
			return nil, err
		}
		ws.Created, ws.LastSeen, ws.Expires = parseTime(created), parseTime(seen), parseTime(expires)
		out = append(out, ws)
	}
	return out, rows.Err()
}

// tstr formats a time the way parseTime reads it, fixed-width so it sorts as text.
func tstr(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }
