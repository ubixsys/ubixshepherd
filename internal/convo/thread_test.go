package convo

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

const secret = "glpat-AbCdEfGhIjKlMnOpQrStUv"

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func at(m int) string { return t0.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }

func claudeFixture(t *testing.T, projects, id string, lines ...string) {
	t.Helper()
	dir := filepath.Join(projects, "-work-lane")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(id int64, agent, session, state string, startMin int) store.Run {
	r := store.Run{ID: id, Agent: agent, Session: session, State: state, Started: t0.Add(time.Duration(startMin) * time.Minute)}
	if state != store.RunRunning {
		e := r.Started.Add(5 * time.Minute)
		r.Ended = &e
	}
	return r
}

// A session as Claude Code writes it: one line per content block, results as later user
// lines, harness wrappers, a sidechain, and a half-written last line.
func claudeSession(t *testing.T, projects string) {
	claudeFixture(t, projects, "sess-1",
		`{"type":"queue-operation","timestamp":"`+at(0)+`"}`,
		`{"type":"user","uuid":"u1","timestamp":"`+at(0)+`","message":{"role":"user","content":"Fix the bug, token `+secret+`"}}`,
		`{"type":"user","uuid":"u0","timestamp":"`+at(0)+`","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"`+at(1)+`","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me look at it. `+secret+`"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"`+at(1)+`","message":{"role":"assistant","content":[{"type":"text","text":"I'll **read** the file."}]}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"`+at(1)+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"cat .env # `+secret+`","description":"show env"}}]}}`,
		`{"type":"assistant","uuid":"s1","isSidechain":true,"timestamp":"`+at(1)+`","message":{"role":"assistant","content":[{"type":"text","text":"subagent chatter"}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"`+at(2)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"TOKEN=`+secret+`\nok"}]}}`,
		`{"type":"assistant","uuid":"a4","timestamp":"`+at(3)+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"Read","input":{"file_path":"/x/y.go"}}]}}`,
		`{"type":"user","uuid":"u3","timestamp":"`+at(4)+`","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>ignore</system-reminder>"}]}}`,
		`{"type":"assistant","uuid":"a5","timestamp":"`+at(4)+`","message":{"role":"assistant","content":[{"type":"te`,
	)
}

func TestClaudeThread(t *testing.T) {
	projects := t.TempDir()
	claudeSession(t, projects)
	src := Source{ClaudeProjects: projects}
	th := Read(context.Background(), src, []store.Run{run(7, "claude", "sess-1", store.RunSucceeded, 0)}, Options{})

	var kinds []string
	for _, it := range th.Items {
		kinds = append(kinds, it.Kind)
	}
	if got := strings.Join(kinds, ","); got != "run,user,thinking,agent,tool,tool" {
		t.Fatalf("kinds = %s", got)
	}
	run0, tool1, tool2 := th.Items[0], th.Items[4], th.Items[5]
	if run0.Run != 7 || run0.Agent != "claude" || run0.State != store.RunSucceeded || run0.End == nil || run0.Note != "" {
		t.Errorf("boundary = %+v", run0)
	}
	if tool1.Tool != "Bash" || tool1.Status != ToolOK || !strings.Contains(tool1.Output, "ok") || tool1.Summary == "" {
		t.Errorf("tool1 = %+v", tool1)
	}
	// The run is over and Read never got a result.
	if tool2.Status != ToolNoResult || tool2.Summary != "/x/y.go" {
		t.Errorf("tool2 = %+v", tool2)
	}
	if th.Items[1].Seq != 2 || th.Items[3].Text != "I'll **read** the file." {
		t.Errorf("items = %+v", th.Items)
	}
	// Redaction reaches every field of every item.
	for _, it := range th.Items {
		for _, s := range []string{it.Text, it.Input, it.Output, it.Summary} {
			if strings.Contains(s, secret) {
				t.Errorf("secret leaked in %+v", it)
			}
		}
	}
	if !strings.Contains(th.Items[1].Text, "[REDACTED]") || !strings.Contains(tool1.Input, "[REDACTED]") {
		t.Errorf("not redacted: %+v / %+v", th.Items[1], tool1)
	}
	if th.Running {
		t.Error("running = true")
	}
}

func TestFollowSinceCursor(t *testing.T) {
	projects := t.TempDir()
	claudeSession(t, projects)
	src := Source{ClaudeProjects: projects}
	runs := []store.Run{run(7, "claude", "sess-1", store.RunRunning, 0)}
	first := Read(context.Background(), src, runs, Options{})
	if !first.Running {
		t.Error("running = false")
	}
	// The Read call has no result yet and the run goes on: it is delivered, but the
	// cursor stays before it so it comes again with its output.
	last := first.Items[len(first.Items)-1]
	if last.Tool != "Read" || last.Status != ToolRunning {
		t.Fatalf("last = %+v", last)
	}
	if first.Cursor != last.Seq-1 {
		t.Fatalf("cursor = %d, last seq %d", first.Cursor, last.Seq)
	}
	// The result arrives, and more text.
	f := filepath.Join(projects, "-work-lane", "sess-1.jsonl")
	b, _ := os.ReadFile(f)
	b = b[:strings.LastIndex(string(b), `{"type":"assistant","uuid":"a5"`)]
	b = append(b, []byte(`{"type":"user","uuid":"u4","timestamp":"`+at(5)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"package y"}]}]}}`+"\n"+
		`{"type":"assistant","uuid":"a5","timestamp":"`+at(6)+`","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`+"\n")...)
	os.WriteFile(f, b, 0o644)
	next := Read(context.Background(), src, runs, Options{After: first.Cursor})
	var got []string
	for _, it := range next.Items {
		if it.Kind != KindRun {
			got = append(got, fmt.Sprintf("%d:%s:%s", it.Seq, it.Kind, it.Status))
		}
	}
	if strings.Join(got, " ") != "6:tool:ok 7:agent:" {
		t.Errorf("next = %v", got)
	}
	if next.Cursor != 7 {
		t.Errorf("cursor = %d", next.Cursor)
	}
	// Nothing new: no items but the boundary, and the cursor holds.
	none := Read(context.Background(), src, runs, Options{After: next.Cursor})
	if len(none.Items) != 1 || none.Items[0].Kind != KindRun || none.Cursor != 7 {
		t.Errorf("none = %+v", none)
	}
}

func TestPagingAndExpand(t *testing.T) {
	projects := t.TempDir()
	big := strings.Repeat("x", 3*DefaultOutputCap) + "é"
	claudeFixture(t, projects, "big",
		`{"type":"assistant","uuid":"a","timestamp":"`+at(0)+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"seq"}}]}}`,
		`{"type":"user","uuid":"b","timestamp":"`+at(1)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"`+big+`"}]}}`,
		`{"type":"assistant","uuid":"c","timestamp":"`+at(2)+`","message":{"role":"assistant","content":[{"type":"text","text":"one"}]}}`,
		`{"type":"assistant","uuid":"d","timestamp":"`+at(3)+`","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}`,
	)
	src := Source{ClaudeProjects: projects}
	runs := []store.Run{run(1, "claude", "big", store.RunSucceeded, 0)}
	p1 := Read(context.Background(), src, runs, Options{Limit: 2})
	if !p1.More || p1.Cursor != 3 || len(p1.Items) != 3 {
		t.Fatalf("p1 = %+v", p1)
	}
	tool := p1.Items[1]
	if !tool.Truncated || len(tool.Output) != DefaultOutputCap || tool.Bytes != len(big) {
		t.Errorf("tool truncated=%v len=%d bytes=%d", tool.Truncated, len(tool.Output), tool.Bytes)
	}
	p2 := Read(context.Background(), src, runs, Options{After: p1.Cursor, Limit: 2})
	if p2.More || p2.Cursor != 4 || p2.Items[len(p2.Items)-1].Text != "two" {
		t.Errorf("p2 = %+v", p2)
	}
	full := Read(context.Background(), src, runs, Options{Expand: 2})
	if len(full.Items) != 1 || full.Items[0].Output != big || full.Items[0].Truncated {
		t.Errorf("expand = %d items", len(full.Items))
	}
	if x := Read(context.Background(), src, runs, Options{Expand: 99}); len(x.Items) != 0 {
		t.Errorf("expand missing = %+v", x)
	}
}

func TestRunsShareASessionAndCopiesAreDropped(t *testing.T) {
	projects := t.TempDir()
	claudeFixture(t, projects, "s",
		`{"type":"user","uuid":"1","timestamp":"`+at(0)+`","message":{"role":"user","content":"first brief"}}`,
		`{"type":"assistant","uuid":"2","timestamp":"`+at(1)+`","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}`,
		`{"type":"user","uuid":"3","timestamp":"`+at(10)+`","message":{"role":"user","content":"second brief"}}`,
		`{"type":"assistant","uuid":"4","timestamp":"`+at(11)+`","message":{"role":"assistant","content":[{"type":"text","text":"second reply"}]}}`,
	)
	// A resumed session that copied the first run's history under a new id.
	claudeFixture(t, projects, "s2",
		`{"type":"user","uuid":"1","timestamp":"`+at(0)+`","message":{"role":"user","content":"first brief"}}`,
		`{"type":"assistant","uuid":"2","timestamp":"`+at(1)+`","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}`,
		`{"type":"assistant","uuid":"5","timestamp":"`+at(21)+`","message":{"role":"assistant","content":[{"type":"text","text":"third reply"}]}}`,
	)
	runs := []store.Run{run(3, "claude", "s2", store.RunSucceeded, 20), run(1, "claude", "s", store.RunSucceeded, 0), run(2, "claude", "s", store.RunSucceeded, 10)}
	th := Read(context.Background(), Source{ClaudeProjects: projects}, runs, Options{})
	var got []string
	for _, it := range th.Items {
		if it.Kind == KindRun {
			got = append(got, fmt.Sprintf("[run %d]", it.Run))
		} else {
			got = append(got, it.Text)
		}
	}
	want := "[run 1] first brief first reply [run 2] second brief second reply [run 3] third reply"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}

func TestMissingUnreadableAndUnsupported(t *testing.T) {
	projects := t.TempDir()
	runs := []store.Run{
		run(1, "claude", "gone", store.RunSucceeded, 0),
		run(2, "claude", "../escape", store.RunSucceeded, 1),
		run(3, "copilot", "c-1", store.RunSucceeded, 2),
		run(4, "cursor", "", store.RunFailed, 3),
		run(5, "claude", "", store.RunFailed, 4),
		run(6, "opencode", "ses_x", store.RunSucceeded, 5),
	}
	th := Read(context.Background(), Source{ClaudeProjects: projects, OpenCodeDB: filepath.Join(projects, "none.db")}, runs, Options{})
	if len(th.Items) != 6 {
		t.Fatalf("items = %d", len(th.Items))
	}
	for _, it := range th.Items {
		if it.Kind != KindRun || it.Note == "" {
			t.Errorf("run %d: %+v", it.Run, it)
		}
	}
	if !strings.Contains(th.Items[2].Note, "no transcript for copilot") || !strings.Contains(th.Items[3].Note, "no transcript for cursor") {
		t.Errorf("notes: %q %q", th.Items[2].Note, th.Items[3].Note)
	}
	// A file that exists but cannot be read.
	if os.Getuid() != 0 {
		claudeSession(t, projects)
		f := filepath.Join(projects, "-work-lane", "sess-1.jsonl")
		os.Chmod(f, 0)
		defer os.Chmod(f, 0o644)
		th = Read(context.Background(), Source{ClaudeProjects: projects}, []store.Run{run(1, "claude", "sess-1", store.RunSucceeded, 0)}, Options{})
		if len(th.Items) != 1 || th.Items[0].Note == "" {
			t.Errorf("unreadable = %+v", th.Items)
		}
	}
}

func openCodeFixture(t *testing.T, id string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE TABLE session (id text PRIMARY KEY, directory text)")
	exec("CREATE TABLE message (id text PRIMARY KEY, session_id text, time_created integer, time_updated integer, data text)")
	exec("CREATE TABLE part (id text PRIMARY KEY, message_id text, session_id text, time_created integer, time_updated integer, data text)")
	exec("INSERT INTO session VALUES (?, '/w')", id)
	ms := t0.UnixMilli()
	msg := func(mid string, off int64, data string) {
		exec("INSERT INTO message VALUES (?, ?, ?, ?, ?)", mid, id, ms+off, ms+off, data)
	}
	part := func(pid, mid string, off int64, data string) {
		exec("INSERT INTO part VALUES (?, ?, ?, ?, ?, ?)", pid, mid, id, ms+off, ms+off, data)
	}
	msg("m1", 0, `{"role":"user","time":{"created":1},"summary":{"diffs":[{"file":"a"}]}}`)
	part("p1", "m1", 0, `{"type":"text","text":"Rename Foo to Bar, key `+secret+`"}`)
	part("p0", "m1", 0, `{"type":"text","text":"synthetic note","synthetic":true}`)
	msg("m2", 1000, `{"role":"assistant","parentID":"m1"}`)
	part("p2", "m2", 1000, `{"type":"step-start","snapshot":"abc"}`)
	part("p3", "m2", 1001, `{"type":"reasoning","text":"Need to grep.","time":{"start":`+fmt.Sprint(ms+1001)+`,"end":1}}`)
	part("p4", "m2", 1002, `{"type":"text","text":"Searching.","time":{"start":`+fmt.Sprint(ms+1002)+`}}`)
	part("p5", "m2", 1003, `{"type":"tool","tool":"grep","callID":"c1","state":{"status":"completed","input":{"pattern":"Foo","path":"."},"output":"a.go:1: Foo `+secret+`","time":{"start":`+fmt.Sprint(ms+1003)+`,"end":1}}}`)
	part("p6", "m2", 1004, `{"type":"tool","tool":"edit","callID":"c2","state":{"status":"error","input":{"filePath":"a.go"},"error":"file changed"}}`)
	part("p7", "m2", 1005, `{"type":"tool","tool":"bash","callID":"c3","state":{"status":"running","input":{"command":"go test"}}}`)
	part("p8", "m2", 1006, `{"type":"step-finish","reason":"tool-calls"}`)
	return file
}

func TestOpenCodeThread(t *testing.T) {
	db := openCodeFixture(t, "ses_1")
	before, _ := os.ReadFile(db)
	runs := []store.Run{run(9, "opencode", "ses_1", store.RunRunning, 0)}
	th := Read(context.Background(), Source{OpenCodeDB: db}, runs, Options{})
	var got []string
	for _, it := range th.Items {
		got = append(got, it.Kind+":"+it.Tool+":"+it.Status)
	}
	want := "run::,user::,thinking::,agent::,tool:grep:ok,tool:edit:error,tool:bash:running"
	if strings.Join(got, ",") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, ","), want)
	}
	if strings.Contains(th.Items[1].Text, secret) || strings.Contains(th.Items[4].Output, secret) {
		t.Errorf("secret leaked: %+v %+v", th.Items[1], th.Items[4])
	}
	if th.Items[5].Output != "file changed" || th.Items[4].Summary != "." && th.Items[4].Summary != "Foo" {
		t.Errorf("items = %+v", th.Items[4:6])
	}
	if th.Cursor != 6 || !th.Running {
		t.Errorf("cursor = %d running = %v", th.Cursor, th.Running)
	}
	// Reading never changes the database file.
	after, _ := os.ReadFile(db)
	if string(before) != string(after) {
		t.Error("the OpenCode database changed")
	}
	// A session the database does not have is a note, not an error.
	th = Read(context.Background(), Source{OpenCodeDB: db}, []store.Run{run(9, "opencode", "ses_nope", store.RunSucceeded, 0)}, Options{})
	if len(th.Items) != 1 || !strings.Contains(th.Items[0].Note, "no session") {
		t.Errorf("missing = %+v", th.Items)
	}
}
