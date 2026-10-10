package convo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCodeDB is where OpenCode keeps its sessions: one SQLite database under its data
// directory (XDG_DATA_HOME, else ~/.local/share).
func OpenCodeDB() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db")
}

// loadOpenCode reads one OpenCode session from its database, opened read-only.
func loadOpenCode(ctx context.Context, dbfile, id string) ([]entry, string) {
	if _, err := os.Stat(dbfile); err != nil {
		return nil, fmt.Sprintf("OpenCode's database is not at %s; see this run's log", dbfile)
	}
	key := dbfile + "#" + id
	st := stamp(dbfile, dbfile+"-wal")
	if es, ok := cacheGet(key, st); ok {
		return es, ""
	}
	dsn := "file:" + filepath.ToSlash(dbfile) + "?mode=ro&_pragma=busy_timeout(3000)&_pragma=query_only(1)"
	if u, err := url.Parse(dsn); err == nil {
		dsn = u.String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, "OpenCode's database cannot be opened; see this run's log"
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session WHERE id = ?`, id).Scan(&n); err != nil {
		return nil, "OpenCode's database cannot be read (an unfamiliar layout, or locked); see this run's log"
	}
	if n == 0 {
		return nil, fmt.Sprintf("OpenCode has no session %s (removed); see this run's log", short(id))
	}
	msgs, err := db.QueryContext(ctx,
		`SELECT id, time_created, json_extract(data,'$.role') FROM message WHERE session_id = ? ORDER BY time_created, id`, id)
	if err != nil {
		return nil, "OpenCode's messages cannot be read; see this run's log"
	}
	type message struct {
		id, role string
		created  int64
	}
	var ms []message
	for msgs.Next() {
		var m message
		var role sql.NullString
		if msgs.Scan(&m.id, &m.created, &role) == nil {
			m.role = role.String
			ms = append(ms, m)
		}
	}
	msgs.Close()
	parts, err := db.QueryContext(ctx,
		`SELECT message_id, id, time_created, data FROM part WHERE session_id = ? ORDER BY message_id, id`, id)
	if err != nil {
		return nil, "OpenCode's parts cannot be read; see this run's log"
	}
	defer parts.Close()
	byMsg := map[string][]ocPart{}
	for parts.Next() {
		var p ocPart
		var data string
		if parts.Scan(&p.msg, &p.id, &p.created, &data) != nil {
			continue
		}
		if json.Unmarshal([]byte(data), &p.d) == nil {
			byMsg[p.msg] = append(byMsg[p.msg], p)
		}
	}
	var out []entry
	for _, m := range ms {
		for _, p := range byMsg[m.id] {
			ts := p.created
			if p.d.Time.Start > 0 {
				ts = p.d.Time.Start
			}
			if ts == 0 {
				ts = m.created
			}
			t := time.UnixMilli(ts).UTC()
			e := entry{id: p.id, t: t}
			switch p.d.Type {
			case "text":
				if p.d.Synthetic || p.d.Ignored || p.d.Text == "" {
					continue
				}
				e.kind = KindAgent
				if m.role == "user" {
					e.kind = KindUser
				}
				e.text = red(p.d.Text)
			case "reasoning":
				e.kind, e.text = KindThinking, red(p.d.Text)
			case "tool":
				e.kind, e.tool = KindTool, p.d.Tool
				var in map[string]any
				if json.Unmarshal(p.d.State.Input, &in) == nil {
					e.summary = red(summarize(p.d.Tool, in))
					if pretty, err := json.MarshalIndent(in, "", "  "); err == nil {
						e.input = red(string(pretty))
					}
				}
				e.output = red(p.d.State.Output)
				switch p.d.State.Status {
				case "completed":
					e.status = ToolOK
				case "error":
					e.status = ToolError
					if e.output == "" {
						e.output = red(p.d.State.Error)
					}
				default:
					e.status = ToolRunning
				}
				if p.d.State.Time.Start > 0 {
					e.t = time.UnixMilli(p.d.State.Time.Start).UTC()
				}
			default:
				continue
			}
			out = append(out, e)
		}
	}
	cachePut(key, st, out)
	return out, ""
}

type ocPart struct {
	msg, id string
	created int64
	d       struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Synthetic bool   `json:"synthetic"`
		Ignored   bool   `json:"ignored"`
		Tool      string `json:"tool"`
		Time      struct {
			Start int64 `json:"start"`
		} `json:"time"`
		State struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			Error  string          `json:"error"`
			Time   struct {
				Start int64 `json:"start"`
			} `json:"time"`
		} `json:"state"`
	}
}
