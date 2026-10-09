package desk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

// FakeClaude stands in for claude -p: it logs its arguments and the desk's token, and
// answers in stream-json. A message with "sleep" in it stalls after a partial reply
// (for interrupts and queueing); "fail" makes the turn fail. Each session's turns cost
// $0.10, then $0.25, then $0.45 in total, as Claude Code reports a session's total.
const FakeClaude = `#!/bin/sh
msg=$(cat)
session=""
prev=""
for a in "$@"; do
  case "$prev" in --session-id|--resume) session="$a" ;; esac
  prev="$a"
done
echo "ARGS $*" >> "$FAKE_LOG"
echo "TOKEN $SHEPHERD_TOKEN URL $SHEPHERD_URL CLIENT $SHEPHERD_CLIENT" >> "$FAKE_LOG"
echo "MSG $(printf '%s' "$msg" | tr '\n' ' ')" >> "$FAKE_LOG"
n=$(cat "$FAKE_DIR/$session.n" 2>/dev/null || echo 0)
n=$((n+1))
echo $n > "$FAKE_DIR/$session.n"
case $n in 1) c=0.10 ;; 2) c=0.25 ;; *) c=0.45 ;; esac
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}'
case "$msg" in *"please sleep"*) sleep 30 ;; esac
case "$msg" in *"please fail"*) echo "the model is gone" >&2; exit 1 ;; esac
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__shepherd__lane_list","input":{"repo":"app"}}]}}'
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"reply %s"}]}}\n' "$n"
printf '{"type":"result","subtype":"success","total_cost_usd":%s}\n' "$c"
`

type fakeTokens struct {
	mu      sync.Mutex
	minted  []bool // human, per mint
	revoked int
}

func (f *fakeTokens) MintDesk(human bool, session string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted = append(f.minted, human)
	return fmt.Sprintf("desk-token-%d", len(f.minted)), nil
}
func (f *fakeTokens) RevokeToken(string) { f.mu.Lock(); f.revoked++; f.mu.Unlock() }
func (f *fakeTokens) URL() string        { return "http://127.0.0.1:9" }
func (f *fakeTokens) humans() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.minted...)
}

type rig struct {
	st     store.Store
	m      *Manager
	tok    *fakeTokens
	ws     store.Workspace
	log    string
	wake   string
	wakeMu sync.Mutex
}

func newRig(t *testing.T, mut func(*Options)) *rig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	dir := t.TempDir()
	st, err := sqlite.Open(context.Background(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte(FakeClaude), 0o755)
	t.Setenv("FAKE_LOG", filepath.Join(dir, "claude.log"))
	t.Setenv("FAKE_DIR", dir)
	ws, err := st.SaveWorkspace(context.Background(), store.Workspace{Name: "ws", Path: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, tok: &fakeTokens{}, ws: ws, log: filepath.Join(dir, "claude.log"), wake: config.WakeAttached}
	o := Options{Store: st, Agent: Claude{Bin: bin, Shepherd: "/bin/shepherd"}, Tokens: r.tok,
		Wake: func() string { r.wakeMu.Lock(); defer r.wakeMu.Unlock(); return r.wake }}
	if mut != nil {
		mut(&o)
	}
	r.m = NewManager(o)
	t.Cleanup(r.m.Close)
	return r
}

func (r *rig) desk(t *testing.T) *Desk {
	d, err := r.m.For(r.ws)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (r *rig) events(t *testing.T) []store.DeskEvent {
	es, err := r.st.DeskEvents(context.Background(), r.ws.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// waitEnded waits for n turns to end and returns every event.
func (r *rig) waitEnded(t *testing.T, n int) []store.DeskEvent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		es := r.events(t)
		ended := 0
		for _, e := range es {
			if e.Kind == store.DeskTurnEnd {
				ended++
			}
		}
		if ended >= n {
			return es
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d turn(s) did not end: %s", n, kinds(r.events(t)))
	return nil
}

func kinds(es []store.DeskEvent) string {
	var s []string
	for _, e := range es {
		s = append(s, fmt.Sprintf("%d:%s(%d,%s)%q", e.Seq, e.Kind, e.Turn, e.Origin, e.Text))
	}
	return strings.Join(s, " ")
}

func (r *rig) claudeLog(t *testing.T) string {
	b, _ := os.ReadFile(r.log)
	return string(b)
}

func TestTurnRoundTrip(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	id, _, err := d.Send(context.Background(), "hello there")
	if err != nil {
		t.Fatal(err)
	}
	es := r.waitEnded(t, 1)
	want := []string{store.DeskUser, store.DeskTurnStart, store.DeskTool, store.DeskAssistant, store.DeskCost, store.DeskTurnEnd}
	var got []string
	for _, e := range es {
		got = append(got, e.Kind)
		if e.Turn != id || e.Origin != store.DeskByHuman {
			t.Errorf("event %s: turn %d origin %q, want %d human", e.Kind, e.Turn, e.Origin, id)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %s", kinds(es))
	}
	if es[3].Text != "reply 1" || es[len(es)-1].Text != "done" {
		t.Errorf("events = %s", kinds(es))
	}
	log := r.claudeLog(t)
	if !strings.Contains(log, "--session-id") || !strings.Contains(log, "--append-system-prompt") ||
		!strings.Contains(log, `["mcp","--scoped"]`) || !strings.Contains(log, "--disallowedTools Edit Write Bash") {
		t.Errorf("args:\n%s", log)
	}
	if !strings.Contains(log, "TOKEN desk-token-1 URL http://127.0.0.1:9 CLIENT desk") || !strings.Contains(log, "MSG hello there") {
		t.Errorf("env or message:\n%s", log)
	}
	if h := r.tok.humans(); len(h) != 1 || !h[0] {
		t.Errorf("tokens minted (human?) = %v", h)
	}
	if s := d.Session(context.Background()); s == "" {
		t.Error("no session kept")
	}
}

// The second turn resumes the session; each turn records what it added to the
// session's total, not the total.
func TestResumeAndCostDifference(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	session := d.Session(context.Background())
	d.Send(context.Background(), "two")
	es := r.waitEnded(t, 2)
	if !strings.Contains(r.claudeLog(t), "--resume "+session) {
		t.Errorf("second turn did not resume %s:\n%s", session, r.claudeLog(t))
	}
	var costs []string
	for _, e := range es {
		if e.Kind == store.DeskCost {
			costs = append(costs, e.Text)
		}
	}
	if strings.Join(costs, ",") != "0.1,0.15" {
		t.Errorf("costs = %v, want 0.1 then 0.15", costs)
	}
	spent, _ := r.st.SpendOn(context.Background(), time.Now().Format("2006-01-02"))
	if got := spent[store.OriginDesk].USD; got < 0.2499 || got > 0.2501 {
		t.Errorf("desk spend = %v, want 0.25", got)
	}
}

func TestQueueAndInterrupt(t *testing.T) {
	r := newRig(t, func(o *Options) { o.QueueMax = 2 })
	d := r.desk(t)
	first, _, _ := d.Send(context.Background(), "please sleep")
	// Wait for the first turn's partial reply.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.claudeLog(t), "MSG please sleep") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	second, ahead, err := d.Send(context.Background(), "then this")
	if err != nil || ahead != 1 {
		t.Fatalf("second: ahead %d, %v", ahead, err)
	}
	if _, ahead, err := d.Send(context.Background(), "and this"); err != nil || ahead != 2 {
		t.Fatalf("third: ahead %d, %v", ahead, err)
	}
	if _, _, err := d.Send(context.Background(), "too many"); err != ErrQueueFull {
		t.Errorf("full queue: %v", err)
	}
	if busy, queued := d.Busy(); !busy || queued != 2 {
		t.Errorf("busy %v with %d queued", busy, queued)
	}
	if !d.Interrupt() {
		t.Fatal("nothing to interrupt")
	}
	es := r.waitEnded(t, 3)
	ends := map[int64]string{}
	var order []int64
	for _, e := range es {
		if e.Kind == store.DeskTurnEnd {
			ends[e.Turn] = e.Text
		}
		if e.Kind == store.DeskTurnStart {
			order = append(order, e.Turn)
		}
		if e.Kind == store.DeskAssistant && e.Turn == first && e.Text != "Hel" {
			t.Errorf("interrupted turn kept %q, want its partial reply", e.Text)
		}
	}
	if ends[first] != "interrupted" || ends[second] != "done" {
		t.Errorf("ends = %v: %s", ends, kinds(es))
	}
	if len(order) < 2 || order[0] != first || order[1] != second {
		t.Errorf("turns ran in order %v, want %d then %d", order, first, second)
	}
}

func TestFailedTurn(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	d.Send(context.Background(), "please fail")
	es := r.waitEnded(t, 1)
	last := es[len(es)-1]
	if last.Kind != store.DeskTurnEnd || last.Text != "failed" || !strings.Contains(kinds(es), "the model is gone") {
		t.Errorf("events = %s", kinds(es))
	}
}

// A daemon restart: the history is intact, the turn left open is closed, and the next
// turn resumes the same session.
func TestRestartResumes(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	session := d.Session(context.Background())
	d.Send(context.Background(), "please sleep")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.claudeLog(t), "MSG please sleep") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r.m.Close()
	before := r.events(t)

	m2 := NewManager(Options{Store: r.st, Agent: r.m.o.Agent, Tokens: r.tok})
	defer m2.Close()
	d2, err := m2.For(r.ws)
	if err != nil {
		t.Fatal(err)
	}
	after := r.events(t)
	if len(after) < len(before) || after[0].Seq != before[0].Seq {
		t.Fatalf("history lost: %s", kinds(after))
	}
	open := 0
	for _, e := range after {
		switch e.Kind {
		case store.DeskUser:
			open++
		case store.DeskTurnEnd:
			open--
		}
	}
	if open != 0 {
		t.Errorf("a turn left open: %s", kinds(after))
	}
	d2.Send(context.Background(), "three")
	r.waitEnded(t, 3)
	log := r.claudeLog(t)
	if !strings.Contains(log[strings.LastIndex(log, "ARGS"):], "--resume "+session) {
		t.Errorf("did not resume %s after the restart:\n%s", session, log)
	}
}

func TestNewConversation(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	d.Send(context.Background(), "one")
	r.waitEnded(t, 1)
	old := d.Session(context.Background())
	if err := d.New(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.Send(context.Background(), "two")
	r.waitEnded(t, 2)
	if s := d.Session(context.Background()); s == "" || s == old {
		t.Errorf("session after new = %q, was %q", s, old)
	}
}

func feedItem(kind, text string) store.FeedItem { return store.FeedItem{Kind: kind, Text: text} }

// With nobody attached, events wait; the next client to attach gets them as one digest
// turn, of origin system, newest winning past the bound.
func TestWakeDigestWhenAttached(t *testing.T) {
	r := newRig(t, func(o *Options) { o.DigestMax = 2; o.Grace = time.Millisecond })
	d := r.desk(t)
	r.m.Wake([]store.FeedItem{feedItem(store.FeedRunPassed, "run 1 passed"), feedItem(store.FeedLaneOpened, "not a wake-up"),
		feedItem(store.FeedDecision, "decision 2 waits"), feedItem(store.FeedRequestStuck, "request 3 needs routing")})
	time.Sleep(300 * time.Millisecond)
	if es := r.events(t); len(es) != 0 {
		t.Fatalf("woke with nobody attached: %s", kinds(es))
	}
	sub := d.Subscribe()
	defer d.Unsubscribe(sub)
	es := r.waitEnded(t, 1)
	sys := es[0]
	if sys.Kind != store.DeskSystem || sys.Origin != store.DeskBySystem {
		t.Fatalf("first event = %s", kinds(es))
	}
	if strings.Contains(sys.Text, "run 1") || !strings.Contains(sys.Text, "decision 2") || !strings.Contains(sys.Text, "request 3") ||
		!strings.Contains(sys.Text, "1 earlier event") || strings.Contains(sys.Text, "not a wake-up") {
		t.Errorf("digest = %q", sys.Text)
	}
	if h := r.tok.humans(); len(h) != 1 || h[0] {
		t.Errorf("a wake-up's token was minted human: %v", h)
	}
	// Partial replies reach the subscriber, and are never stored.
	select {
	case e := <-sub.Live:
		if e.Kind != KindPartial {
			t.Errorf("live event = %+v", e)
		}
	default:
		t.Error("no partial reply reached the subscriber")
	}
	for _, e := range es {
		if e.Kind == KindPartial {
			t.Errorf("a partial was stored: %+v", e)
		}
	}
}

func TestWakeModes(t *testing.T) {
	r := newRig(t, nil)
	d := r.desk(t)
	sub := d.Subscribe()
	r.m.Wake([]store.FeedItem{feedItem(store.FeedRunFailed, "run 4 failed")})
	n := len(r.waitEnded(t, 1))
	d.Unsubscribe(sub)

	r.wakeMu.Lock()
	r.wake = config.WakeNever
	r.wakeMu.Unlock()
	sub = d.Subscribe()
	r.m.Wake([]store.FeedItem{feedItem(store.FeedRunFailed, "run 5 failed")})
	time.Sleep(300 * time.Millisecond)
	d.Unsubscribe(sub)
	if len(r.events(t)) != n {
		t.Errorf("never woke anyway: %s", kinds(r.events(t)))
	}

	r.wakeMu.Lock()
	r.wake = config.WakeAlways
	r.wakeMu.Unlock()
	r.m.Wake([]store.FeedItem{feedItem(store.FeedRunFailed, "run 6 failed")})
	es := r.waitEnded(t, 2)
	if !strings.Contains(kinds(es), "run 6 failed") || strings.Contains(kinds(es), "run 5 failed") {
		t.Errorf("always: %s", kinds(es))
	}
}

// Within the grace period after the last client leaves, the desk still wakes.
func TestWakeWithinGrace(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Grace = time.Minute })
	d := r.desk(t)
	d.Unsubscribe(d.Subscribe())
	r.m.Wake([]store.FeedItem{feedItem(store.FeedRunPassed, "run 7 passed")})
	r.waitEnded(t, 1)
}
