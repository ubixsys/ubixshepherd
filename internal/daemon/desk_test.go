package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/desk"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

// fakeClaude answers as claude -p does, in stream-json. Within each turn it tries to
// answer decision 1 with the token the daemon gave it and logs the status it got. "please
// sleep" stalls the turn. Its reply read $FAKE_TOKENS tokens of context (5 if unset).
const fakeClaude = `#!/bin/sh
msg=$(cat)
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $SHEPHERD_TOKEN" \
  -d '{"answer":"yes"}' "$SHEPHERD_URL/v1/decisions/1/answer")
echo "ANSWER $code $(printf '%s' "$msg" | head -c 40 | tr '\n' ' ')" >> "$FAKE_LOG"
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}'
case "$msg" in *"please sleep"*) sleep 30 ;; esac
echo '{"type":"assistant","message":{"usage":{"input_tokens":'"${FAKE_TOKENS:-5}"'},"content":[{"type":"text","text":"Hello"}]}}'
echo '{"type":"result","subtype":"success","total_cost_usd":0.02}'
`

type deskRig struct {
	s   *Server
	ts  *httptest.Server
	log string
	ws  store.Workspace
}

func newDeskRig(t *testing.T, wake string, grace time.Duration) *deskRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	dir := t.TempDir()
	st, err := sqlite.Open(context.Background(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := NewServer(st, config.Default(), "/cfg", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte(fakeClaude), 0o755)
	t.Setenv("FAKE_LOG", filepath.Join(dir, "claude.log"))
	cfg := config.Default()
	cfg.Desk.Wake = wake
	s.live.Store(&cfg)
	s.DeskAgent = desk.Claude{Bin: bin, Shepherd: "/unused"}
	s.DeskGrace = grace
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	s.addr.Store(&host)
	root, _ := paths.Canonical(t.TempDir())
	ws, err := s.Store.SaveWorkspace(context.Background(), store.Workspace{Name: "ws", Path: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &deskRig{s: s, ts: ts, log: filepath.Join(dir, "claude.log"), ws: ws}
}

type sseEvent struct {
	id   string
	kind string
	data store.DeskEvent
}

// stream follows a stream and sends its events on the channel until the test ends.
func (r *deskRig) stream(t *testing.T, path string, header map[string]string) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", r.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+r.s.Token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan sseEvent, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data)
			case line == "" && ev.kind != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	t.Cleanup(cancel)
	return ch, cancel
}

// until reads events until one of kind arrives, and returns them all.
func until(t *testing.T, ch <-chan sseEvent, kind string) []sseEvent {
	t.Helper()
	var got []sseEvent
	timeout := time.After(15 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended before %s: %+v", kind, got)
			}
			got = append(got, e)
			if e.kind == kind {
				return got
			}
		case <-timeout:
			t.Fatalf("no %s: %+v", kind, got)
		}
	}
}

func (r *deskRig) claudeLog() string {
	b, _ := os.ReadFile(r.log)
	return string(b)
}

func TestDeskTurnOverHTTP(t *testing.T) {
	r := newDeskRig(t, config.WakeNever, 0)
	ch, _ := r.stream(t, api.PathDeskStream+"?after=0", nil)
	var acc api.DeskTurnAccepted
	if code := call(t, r.ts, r.s.Token, "POST", api.PathDeskTurn, api.DeskTurn{Text: "hi"}, &acc); code != http.StatusAccepted || acc.Turn == 0 || acc.WorkspaceID != r.ws.ID {
		t.Fatalf("turn: %d %+v", code, acc)
	}
	got := until(t, ch, store.DeskTurnEnd)
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.kind)
		if e.kind == api.DeskPartial && e.id != "" {
			t.Errorf("a partial has an id: %+v", e)
		}
		if e.kind != api.DeskPartial && (e.id != fmt.Sprint(e.data.Seq) || e.data.Turn != acc.Turn) {
			t.Errorf("event %+v: want its seq as id and turn %d", e, acc.Turn)
		}
	}
	if strings.Join(kinds, ",") != "user,turn_start,partial,assistant,cost,turn_end" {
		t.Errorf("stream = %v", kinds)
	}
	// The person's turn may answer a decision: the request got past authorization
	// (503: this test daemon runs no agents), where a wake-up's is refused.
	if !strings.Contains(r.claudeLog(), "ANSWER 503 hi") {
		t.Errorf("human turn's answer:\n%s", r.claudeLog())
	}
	var st api.DeskStatus
	call(t, r.ts, r.s.Token, "GET", api.PathDeskStatus, nil, &st)
	if st.Session == "" || st.Busy || st.Wake != config.WakeNever || st.Attached != 1 {
		t.Errorf("status = %+v", st)
	}
	var hist api.DeskHistory
	call(t, r.ts, r.s.Token, "GET", api.PathDeskHistory+"?limit=2", nil, &hist)
	if len(hist.Events) != 2 || !hist.More || hist.Events[1].Kind != store.DeskTurnEnd {
		t.Errorf("history = %+v", hist)
	}
	call(t, r.ts, r.s.Token, "GET", fmt.Sprintf("%s?limit=10&before=%d", api.PathDeskHistory, hist.Events[0].Seq), nil, &hist)
	if len(hist.Events) != 3 || hist.More || hist.Events[0].Kind != store.DeskUser {
		t.Errorf("older history = %+v", hist)
	}
	// Resume by sequence number, from the header or the query.
	ch2, _ := r.stream(t, api.PathDeskStream, map[string]string{"Last-Event-ID": fmt.Sprint(acc.Turn + 1)})
	if e := until(t, ch2, store.DeskAssistant); e[0].data.Seq != acc.Turn+2 {
		t.Errorf("resumed at %+v, want seq %d", e[0], acc.Turn+2)
	}
	ch3, _ := r.stream(t, api.PathDeskStream+fmt.Sprintf("?after=%d", acc.Turn+3), nil)
	if e := until(t, ch3, store.DeskTurnEnd); len(e) != 1 {
		t.Errorf("resumed after %d: %+v", acc.Turn+3, e)
	}
	// Spend recorded under desk.
	var sp api.SpendToday
	call(t, r.ts, r.s.Token, "GET", api.PathSpend, nil, &sp)
	if sp.BySource[store.OriginDesk].USD != 0.02 {
		t.Errorf("spend = %+v", sp)
	}
}

func TestDeskInterruptAndNewOverHTTP(t *testing.T) {
	r := newDeskRig(t, config.WakeNever, 0)
	ch, _ := r.stream(t, api.PathDeskStream, nil)
	call(t, r.ts, r.s.Token, "POST", api.PathDeskTurn, api.DeskTurn{Text: "please sleep"}, nil)
	until(t, ch, api.DeskPartial)
	var out api.DeskInterrupted
	if code := call(t, r.ts, r.s.Token, "POST", api.PathDeskInterrupt, nil, &out); code != http.StatusOK || !out.Interrupted {
		t.Fatalf("interrupt: %d %+v", code, out)
	}
	if end := until(t, ch, store.DeskTurnEnd); end[len(end)-1].data.Text != "interrupted" {
		t.Errorf("end = %+v", end[len(end)-1])
	}
	if code := call(t, r.ts, r.s.Token, "POST", api.PathDeskNew, api.DeskWorkspace{}, nil); code != http.StatusOK {
		t.Errorf("new: %d", code)
	}
	until(t, ch, store.DeskNew)
	var st api.DeskStatus
	call(t, r.ts, r.s.Token, "GET", api.PathDeskStatus, nil, &st)
	if st.Session != "" {
		t.Errorf("session after new = %q", st.Session)
	}
}

// A wake-up from the swarm reaches the desk only while someone is attached, and runs as
// a system turn whose token cannot answer a decision.
func TestDeskWakesForTheFeed(t *testing.T) {
	r := newDeskRig(t, config.WakeAttached, time.Millisecond)
	// Nobody attached: the event waits.
	call(t, r.ts, r.s.Token, "GET", api.PathDeskStatus, nil, nil) // start the desk
	r.s.Store.AddFeed(context.Background(), store.FeedRunFailed, "run 3: claude in lane x failed, 0 commit(s)", 3)
	time.Sleep(3 * feedPoll)
	var hist api.DeskHistory
	call(t, r.ts, r.s.Token, "GET", api.PathDeskHistory, nil, &hist)
	if len(hist.Events) != 0 {
		t.Fatalf("woke with nobody attached: %+v", hist.Events)
	}
	// Attaching delivers it as one digest turn.
	ch, _ := r.stream(t, api.PathDeskStream+"?after=0", nil)
	got := until(t, ch, store.DeskTurnEnd)
	if got[0].kind != store.DeskSystem || got[0].data.Origin != store.DeskBySystem || !strings.Contains(got[0].data.Text, "run 3") {
		t.Errorf("first = %+v", got[0])
	}
	if !strings.Contains(r.claudeLog(), "ANSWER 403 [Shepherd]") {
		t.Errorf("a wake-up's turn answered a decision:\n%s", r.claudeLog())
	}
	// While attached, a new event wakes it at once; one that is not a wake-up does not.
	r.s.Store.AddFeed(context.Background(), store.FeedLaneOpened, "lane y opened", 0)
	r.s.Store.AddFeed(context.Background(), store.FeedDecision, "decision 4 waits for you", 4)
	got = until(t, ch, store.DeskTurnEnd)
	if !strings.Contains(got[0].data.Text, "decision 4") || strings.Contains(got[0].data.Text, "lane y") {
		t.Errorf("second wake-up = %+v", got[0])
	}
}

// The conversation survives a restart: a new daemon on the same store serves the same
// history and resumes the same session.
func TestDeskHistorySurvivesRestart(t *testing.T) {
	r := newDeskRig(t, config.WakeNever, 0)
	ch, _ := r.stream(t, api.PathDeskStream, nil)
	call(t, r.ts, r.s.Token, "POST", api.PathDeskTurn, api.DeskTurn{Text: "remember this"}, nil)
	until(t, ch, store.DeskTurnEnd)
	var before api.DeskStatus
	call(t, r.ts, r.s.Token, "GET", api.PathDeskStatus, nil, &before)
	r.s.Close()

	s2, err := NewServer(r.s.Store, config.Default(), "/cfg", r.s.Log)
	if err != nil {
		t.Fatal(err)
	}
	s2.DeskAgent = r.s.DeskAgent
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	defer s2.Close()
	var hist api.DeskHistory
	call(t, ts2, s2.Token, "GET", api.PathDeskHistory, nil, &hist)
	if len(hist.Events) == 0 || hist.Events[0].Text != "remember this" {
		t.Errorf("history after restart = %+v", hist)
	}
	var after api.DeskStatus
	call(t, ts2, s2.Token, "GET", api.PathDeskStatus, nil, &after)
	if after.Session == "" || after.Session != before.Session {
		t.Errorf("session %q after restart, was %q", after.Session, before.Session)
	}
}

// Closing the daemon ends the streams following it.
func TestDeskStreamEndsOnClose(t *testing.T) {
	r := newDeskRig(t, config.WakeNever, 0)
	ch, _ := r.stream(t, api.PathDeskStream, nil)
	r.s.Close()
	select {
	case _, ok := <-ch:
		for ok {
			_, ok = <-ch
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the daemon")
	}
}

// A session past desk.rotate_tokens is rotated between turns: the feed says so once, and
// the person's stream and history go on as if nothing happened.
func TestDeskRotationOverHTTP(t *testing.T) {
	r := newDeskRig(t, config.WakeNever, 0)
	cfg := r.s.LiveConfig()
	at := 50000
	cfg.Desk.RotateTokens = &at
	r.s.live.Store(&cfg)
	ch, _ := r.stream(t, api.PathDeskStream+"?after=0", nil)
	t.Setenv("FAKE_TOKENS", "60000")
	call(t, r.ts, r.s.Token, "POST", api.PathDeskTurn, api.DeskTurn{Text: "one"}, nil)
	until(t, ch, store.DeskTurnEnd)
	var st api.DeskStatus
	call(t, r.ts, r.s.Token, "GET", api.PathDeskStatus, nil, &st)
	if st.Session != "" {
		t.Fatalf("session %q kept past desk.rotate_tokens", st.Session)
	}
	var feed api.Feed
	call(t, r.ts, r.s.Token, "GET", api.PathFeed+"?after=0", nil, &feed)
	var rotated []string
	for _, it := range feed.Items {
		if it.Kind == desk.FeedRotated {
			rotated = append(rotated, it.Text)
		}
	}
	if len(rotated) != 1 || !strings.Contains(rotated[0], "60000 tokens") {
		t.Errorf("feed = %+v", feed.Items)
	}
	t.Setenv("FAKE_TOKENS", "100")
	call(t, r.ts, r.s.Token, "POST", api.PathDeskTurn, api.DeskTurn{Text: "two"}, nil)
	got := until(t, ch, store.DeskTurnEnd)
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.kind)
	}
	if strings.Join(kinds, ",") != "user,turn_start,partial,assistant,cost,turn_end" || got[0].data.Text != "two" {
		t.Errorf("stream after the rotation = %v %+v", kinds, got[0])
	}
	call(t, r.ts, r.s.Token, "GET", api.PathDeskStatus, nil, &st)
	if st.Session == "" {
		t.Error("no session after the rotation")
	}
	var hist api.DeskHistory
	call(t, r.ts, r.s.Token, "GET", api.PathDeskHistory, nil, &hist)
	if len(hist.Events) != 10 || hist.Events[0].Text != "one" {
		t.Errorf("history = %+v", hist.Events)
	}
}
