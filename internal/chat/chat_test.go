package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestParse(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__shepherd__lane_open","input":{"repo":"app","name":"feat/x","scope":["src/**"]}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"opened"}]}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"Opened feat/x."}]}}
not json
{"type":"result","subtype":"success","result":"Opened feat/x.","session_id":"s","total_cost_usd":0.25}
`
	var got []Line
	ok := Parse(strings.NewReader(stream), func(l Line) { got = append(got, l) })
	if !ok || len(got) != 3 || got[2].Kind != KindCost || got[2].Text != "0.25" {
		t.Fatalf("lines = %+v", got)
	}
	if got[0].Kind != KindTool || got[0].Text != "lane_open app feat/x scope src/**" {
		t.Errorf("tool line = %+v", got[0])
	}
	if got[1].Kind != KindDesk || got[1].Text != "Opened feat/x." {
		t.Errorf("text line = %+v", got[1])
	}
	got = nil
	Parse(strings.NewReader(`{"type":"result","subtype":"error_max_turns","is_error":true}`+"\n"), func(l Line) { got = append(got, l) })
	if len(got) != 1 || got[0].Kind != KindError {
		t.Errorf("error result = %+v", got)
	}
}

func TestDeskArgs(t *testing.T) {
	d := ClaudeDesk{Bin: "claude", Shepherd: "/bin/shepherd", Dir: "/w"}
	first := strings.Join(d.Args("S", "hello", true), " ")
	for _, want := range []string{"-p hello", "--session-id S", "--append-system-prompt", "--strict-mcp-config", `"args":["mcp"]`, "--allowedTools mcp__shepherd Read Grep Glob", "--disallowedTools Edit Write Bash"} {
		if !strings.Contains(first, want) {
			t.Errorf("first turn lacks %q", want)
		}
	}
	next := strings.Join(d.Args("S", "again", false), " ")
	if !strings.Contains(next, "--resume S") || strings.Contains(next, "--append-system-prompt") {
		t.Errorf("next turn: %s", next)
	}
}

// fakeDesk records what it is told and replies with one line.
type fakeDesk struct {
	mu   sync.Mutex
	got  []string
	hold chan struct{} // when set, a turn waits for it
}

func (f *fakeDesk) Turn(_ context.Context, session, message string, emit func(Line)) (string, error) {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	f.got = append(f.got, message)
	f.mu.Unlock()
	emit(Line{Kind: KindDesk, Text: "ok: " + strings.SplitN(message, "\n", 2)[0]})
	if session == "" {
		session = "new-session"
	}
	return session, nil
}

type fakeAPI struct {
	feed       []store.FeedItem
	answered   map[int64]string
	settings   map[string]string
	configured string // desk.model in config.yaml
	ds         []api.DecisionView
	lanes      []api.LaneView
	runs       []api.RunView
	reqs       []api.RequestView
	spent      float64
	asked      []string
	redialErrs []error
	redials    int
	feedCalls  int
	laneCalls  int
	runCalls   int
	spendCalls int
	sessCalls  int
}

func (f *fakeAPI) Feed(_ context.Context, after int64) (api.Feed, error) {
	f.feedCalls++
	if after < 0 {
		var last int64
		if len(f.feed) > 0 {
			last = f.feed[len(f.feed)-1].ID
		}
		return api.Feed{Last: last}, nil
	}
	var out api.Feed
	out.Last = after
	for _, it := range f.feed {
		if it.ID > after {
			out.Items = append(out.Items, it)
			out.Last = it.ID
		}
	}
	return out, nil
}
func (f *fakeAPI) Lanes(context.Context, int64, int64) ([]api.LaneView, error) {
	f.laneCalls++
	return f.lanes, nil
}
func (f *fakeAPI) Runs(context.Context, int64, string, int) ([]api.RunView, error) {
	f.runCalls++
	return f.runs, nil
}
func (f *fakeAPI) Requests(context.Context, string) ([]api.RequestView, error) { return f.reqs, nil }
func (f *fakeAPI) RunLog(context.Context, int64, int64) (api.RunLog, error) {
	return api.RunLog{Data: "line\n", Offset: 5, Done: true}, nil
}
func (f *fakeAPI) Decisions(context.Context, string) ([]api.DecisionView, error) { return f.ds, nil }
func (f *fakeAPI) Answer(_ context.Context, id int64, a string) (store.Decision, error) {
	f.answered[id] = a
	return store.Decision{ID: id, AnswerRun: 9}, nil
}
func (f *fakeAPI) SpendToday(context.Context) (api.SpendToday, error) {
	f.spendCalls++
	return api.SpendToday{Day: "today", USD: f.spent, Budget: 20}, nil
}
func (f *fakeAPI) AddSpend(_ context.Context, sp store.Spend) error {
	f.spent += sp.USD
	return nil
}
func (f *fakeAPI) Sessions(context.Context, int64) ([]api.SessionView, error) {
	f.sessCalls++
	return []api.SessionView{{Conversation: store.Conversation{ID: "71ffa009-aaaa", Title: "Stripe integration", Dir: "/w/app"}, Repo: "app"}}, nil
}
func (f *fakeAPI) AskSession(_ context.Context, id, q string) (convo.Answer, error) {
	f.asked = append(f.asked, id+": "+q)
	return convo.Answer{Text: "The webhook secret is in Vault."}, nil
}
func (f *fakeAPI) Setting(_ context.Context, k string) (string, error) { return f.settings[k], nil }
func (f *fakeAPI) SettingInfo(_ context.Context, k string) (api.Setting, error) {
	s := api.Setting{Value: f.settings[k]}
	if k == settingModel {
		s.Configured = f.configured
	}
	return s, nil
}
func (f *fakeAPI) SetSetting(_ context.Context, k, v string) error {
	f.settings[k] = v
	return nil
}
func (f *fakeAPI) Redial() error {
	f.redials++
	if len(f.redialErrs) == 0 {
		return nil
	}
	err := f.redialErrs[0]
	f.redialErrs = f.redialErrs[1:]
	return err
}

// drive runs a command and every command its messages lead to, the way the Bubble Tea
// runtime would, until nothing is left (ticks excluded).
func drive(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0 && steps < 200; steps++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch msg := msg.(type) {
		case nil, tickMsg:
			continue
		case tea.BatchMsg:
			queue = append(queue, msg...)
			continue
		}
		_, next := m.Update(msg)
		queue = append(queue, next)
	}
}

func newTestModel() (*Model, *fakeDesk, *fakeAPI) {
	d := &fakeDesk{}
	a := &fakeAPI{answered: map[int64]string{}, settings: map[string]string{}}
	m := New(context.Background(), a, d, store.Workspace{ID: 1, Name: "git", Path: "/w"})
	// Timers never fire in tests: drive would wait them out.
	m.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return func() tea.Msg { return nil } }
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, d, a
}

// capturePrints records what the chat prints to scrollback.
func capturePrints(m *Model) *[]string {
	var out []string
	m.println = func(a ...any) tea.Cmd {
		out = append(out, fmt.Sprint(a...))
		return nil
	}
	return &out
}

func typeLine(t *testing.T, m *Model, s string) {
	t.Helper()
	m.input.SetValue(s)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drive(t, m, cmd)
}

func has(lines []Line, kind, sub string) bool {
	for _, l := range lines {
		if l.Kind == kind && strings.Contains(l.Text, sub) {
			return true
		}
	}
	return false
}

func TestMessageGoesToTheDeskAndSessionIsKept(t *testing.T) {
	m, d, a := newTestModel()
	typeLine(t, m, "open a lane for the login fix")
	if !has(m.Lines(), KindYou, "login fix") || !has(m.Lines(), KindDesk, "ok: open a lane") {
		t.Fatalf("thread = %+v", m.Lines())
	}
	if a.settings[settingSession] != "new-session" {
		t.Errorf("session not saved: %v", a.settings)
	}
	typeLine(t, m, "and another")
	if len(d.got) != 2 || m.Busy() {
		t.Errorf("desk got %v, busy %v", d.got, m.Busy())
	}
}

func TestEventsShowAndActionableOnesBriefTheDesk(t *testing.T) {
	m, d, a := newTestModel()
	drive(t, m, m.pollFeed()) // the first poll learns where the feed is
	a.feed = []store.FeedItem{
		{ID: 1, Kind: store.FeedRunStarted, Text: "run 3: copilot started in lane api"},
		{ID: 2, Kind: store.FeedDecision, Text: "decision 4 from claude: raise the price?", Ref: 4},
		{ID: 3, Kind: store.FeedRunEnded, Text: "run 3: copilot in lane api succeeded, 1 commit(s)"},
	}
	drive(t, m, m.pollFeed())
	if !has(m.Lines(), KindEvent, "copilot started") || !has(m.Lines(), KindDecision, "/answer 4") {
		t.Fatalf("thread = %+v", m.Lines())
	}
	// Only the ended run needs someone, so only it reaches the desk.
	if len(d.got) != 1 || !strings.HasPrefix(d.got[0], "[Shepherd]") || !strings.Contains(d.got[0], "succeeded") || strings.Contains(d.got[0], "started") {
		t.Errorf("desk briefed with %q", d.got)
	}

	typeLine(t, m, "/auto off")
	a.feed = append(a.feed, store.FeedItem{ID: 4, Kind: store.FeedRunEnded, Text: "run 5 ended"})
	drive(t, m, m.pollFeed())
	if len(d.got) != 1 {
		t.Errorf("desk briefed with auto off: %q", d.got)
	}
}

func TestAnswerIsThePersonsAndPicksOptions(t *testing.T) {
	m, d, a := newTestModel()
	a.ds = []api.DecisionView{{Decision: store.Decision{ID: 4, Options: []string{"8080", "9090"}}}}
	typeLine(t, m, "/answer 4 2")
	if a.answered[4] != "option 2: 9090" {
		t.Errorf("answered %q", a.answered[4])
	}
	if len(d.got) != 0 {
		t.Error("an answer went through the desk")
	}
	if !has(m.Lines(), KindInfo, "carries on as run 9") {
		t.Errorf("thread = %+v", m.Lines())
	}
}

func TestQueueWhileTheDeskWorks(t *testing.T) {
	m, d, _ := newTestModel()
	d.hold = make(chan struct{})
	m.input.SetValue("first")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.Busy() {
		t.Fatal("not busy")
	}
	m.input.SetValue("second")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	close(d.hold)
	drive(t, m, cmd)
	if len(d.got) != 2 || d.got[1] != "second" {
		t.Errorf("desk got %q", d.got)
	}
}

func TestCommands(t *testing.T) {
	m, _, a := newTestModel()
	a.settings[settingSession] = "old"
	typeLine(t, m, "/new")
	if a.settings[settingSession] != "" {
		t.Error("/new kept the session")
	}
	typeLine(t, m, "/log 7")
	if m.logRun != 7 || !strings.Contains(m.logText.String(), "line") {
		t.Errorf("/log: run %d, text %q", m.logRun, m.logText.String())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.logRun != 0 {
		t.Error("Esc did not leave the log")
	}
	typeLine(t, m, "/bogus")
	if !has(m.Lines(), KindError, "unknown command") {
		t.Error("unknown command not reported")
	}
	if v := m.View(); !strings.Contains(v, "desk ready") {
		t.Errorf("view:\n%s", v)
	}
}

func TestDeskCostIsRecordedNotShown(t *testing.T) {
	m, _, a := newTestModel()
	_, cmd := m.Update(deskLineMsg{Kind: KindCost, Text: "0.4"})
	drive(t, m, cmd)
	if a.spent != 0.4 || has(m.Lines(), KindCost, "") {
		t.Errorf("spent %v, lines %+v", a.spent, m.Lines())
	}
}

func TestConversationsInTheChat(t *testing.T) {
	m, d, a := newTestModel()
	drive(t, m, m.pollPanel(true))
	typeLine(t, m, "/sessions")
	if !has(m.Lines(), KindInfo, "Stripe integration") {
		t.Errorf("/sessions: %+v", m.Lines())
	}
	typeLine(t, m, "/ask 71ff where are the webhooks?")
	if len(a.asked) != 1 || a.asked[0] != "71ffa009-aaaa: where are the webhooks?" || !has(m.Lines(), KindDesk, "secret is in Vault") {
		t.Errorf("/ask: asked %v, lines %+v", a.asked, m.Lines())
	}
	if len(d.got) != 0 {
		t.Error("/ask went through the desk")
	}
	typeLine(t, m, "/ask nope hello")
	if !has(m.Lines(), KindError, "no conversation starts with nope") {
		t.Error("unknown conversation not reported")
	}
}

// Finished entries go to the terminal's scrollback, not into the live region.
func TestEntriesArePrintedToScrollback(t *testing.T) {
	m, _, _ := newTestModel()
	printed := capturePrints(m)
	typeLine(t, m, "open a lane for the login fix")
	out := strings.Join(*printed, "\n")
	if !strings.Contains(out, "› open a lane for the login fix") || !strings.Contains(out, "● ok: open a lane") {
		t.Fatalf("printed:\n%s", out)
	}
	if strings.Contains(m.View(), "login fix") {
		t.Errorf("the live region repeats a printed entry:\n%s", m.View())
	}
	// Each entry is printed once.
	n := len(*printed)
	m.Update(lineMsg{Kind: KindInfo, Text: "later"})
	if len(*printed) != n+1 || strings.Contains((*printed)[n], "login fix") {
		t.Errorf("reprinted: %q", (*printed)[n:])
	}
}

func TestPrintedEntriesWrapToTheTerminal(t *testing.T) {
	m, _, _ := newTestModel()
	printed := capturePrints(m)
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 20})
	m.Update(lineMsg{Kind: KindEvent, Text: "run 3: copilot in lane api succeeded, 1 commit(s), and a long tail of words"})
	for _, l := range strings.Split(strings.Join(*printed, "\n"), "\n") {
		if w := ansi.StringWidth(l); w > 30 {
			t.Errorf("line %q is %d wide", l, w)
		}
	}
}

// While the pager holds the alternate screen nothing can be printed; entries wait.
func TestEntriesWaitWhileThePagerIsOpen(t *testing.T) {
	m, _, _ := newTestModel()
	printed := capturePrints(m)
	typeLine(t, m, "/log 7")
	if m.pager == nil || !strings.Contains(m.View(), "line") {
		t.Fatalf("/log did not open the pager:\n%s", m.View())
	}
	n := len(*printed)
	m.Update(lineMsg{Kind: KindEvent, Text: "run 8 started"})
	if len(*printed) != n {
		t.Fatal("printed while the pager was open")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.pager != nil || len(*printed) != n+1 || !strings.Contains((*printed)[n], "run 8 started") {
		t.Fatalf("closing the pager: open %v, printed %q", m.pager != nil, (*printed)[n:])
	}
}

// A reply streams in the live region and is printed once it is whole.
func TestStreamingReplyIsLiveUntilWhole(t *testing.T) {
	m, _, _ := newTestModel()
	printed := capturePrints(m)
	m.busy = true
	m.Update(deskLineMsg{Kind: KindPartial, Text: "Opening a lane "})
	m.Update(deskLineMsg{Kind: KindPartial, Text: "for the fix"})
	if !strings.Contains(m.View(), "Opening a lane for the fix") || len(*printed) != 0 {
		t.Fatalf("partial: printed %q, view:\n%s", *printed, m.View())
	}
	m.Update(deskLineMsg{Kind: KindDesk, Text: "Opening a lane for the fix."})
	if strings.Contains(m.View(), "Opening a lane") || len(*printed) != 1 || !strings.Contains((*printed)[0], "for the fix.") {
		t.Fatalf("whole: printed %q, view:\n%s", *printed, m.View())
	}
}

func TestLiveRegionFitsNarrowTerminals(t *testing.T) {
	m, _, _ := newTestModel()
	m.lanes = []api.LaneView{
		{Lane: store.Lane{ID: 1, Name: "feat/a-rather-long-lane-name", State: store.LaneOpen}, Repo: "app"},
		{Lane: store.Lane{ID: 2, Name: "fix/y", State: store.LaneOpen}, Repo: "app"},
	}
	m.runs = []api.RunView{
		{Run: store.Run{ID: 9, LaneID: 1, Agent: "copilot", State: store.RunRunning, Started: m.clock()}, Lane: "feat/a-rather-long-lane-name"},
		{Run: store.Run{ID: 10, LaneID: 2, Agent: "claude", State: store.RunRunning, Started: m.clock()}, Lane: "fix/y"},
	}
	m.decisions = []api.DecisionView{{Decision: store.Decision{ID: 12}}}
	m.spend = api.SpendToday{Day: "today", USD: 1.5, Budget: 20}
	m.busy, m.lastTool, m.queue = true, "lane_open app feat/a-rather-long-lane-name scope src/**", []string{"x"}
	m.partial = "A reply that streams in with enough words to wrap a few times at this width."
	for _, w := range []int{12, 20, 40, 80} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
		v := m.View()
		for _, l := range strings.Split(v, "\n") {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("width %d: line %q is %d wide", w, l, lw)
			}
		}
		if !strings.Contains(v, "desk") || !strings.Contains(v, "›") {
			t.Errorf("width %d lacks the status or the input:\n%s", w, v)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	v := m.View()
	for _, want := range []string{"desk working", "claude", "● feat/a-rather-long-lane-name  copilot  run 9", "1 needs you · 2 working", "Enter send"} {
		if !strings.Contains(ansi.Strip(v), want) {
			t.Errorf("live region lacks %q:\n%s", want, v)
		}
	}
	if strings.Count(v, "\n") > 16 {
		t.Errorf("live region is %d lines", strings.Count(v, "\n")+1)
	}
}

func TestParsePartials(t *testing.T) {
	stream := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}
`
	var got []Line
	Parse(strings.NewReader(stream), func(l Line) { got = append(got, l) })
	want := []Line{{Kind: KindPartial, Text: "Hel"}, {Kind: KindPartial, Text: "lo"}, {Kind: KindDesk, Text: "Hello"}}
	if len(got) != len(want) {
		t.Fatalf("lines = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReconnectRetriesAndRestoresChat(t *testing.T) {
	m, _, a := newTestModel()
	a.redialErrs = []error{errors.New("runtime file is being replaced"), nil}

	_, cmd := m.Update(errorMsg{err: client.ErrNoDaemon})
	if !m.Reconnecting() || !strings.Contains(m.View(), "reconnecting") {
		t.Fatal("connection failure did not show reconnecting state")
	}
	drive(t, m, cmd)
	if !m.Reconnecting() || a.redials != 1 {
		t.Fatalf("first reconnect: state %v, attempts %d", m.Reconnecting(), a.redials)
	}

	_, cmd = m.Update(tickMsg{generation: m.tickGeneration})
	drive(t, m, cmd)
	if m.Reconnecting() || a.redials != 2 {
		t.Fatalf("reconnect did not recover: state %v, attempts %d", m.Reconnecting(), a.redials)
	}
}

// The desk's model: the --model flag, then /model's setting, then config.yaml, then
// Claude Code's default; /model shows which and where from, and sets or resets it.
func TestModelCommand(t *testing.T) {
	cases := []struct {
		flag, set, configured, want, from string
	}{
		{"", "", "", "", "Claude Code's default"},
		{"", "", "sonnet", "sonnet", "desk.model in config.yaml"},
		{"", "opus", "sonnet", "opus", "set with /model"},
		{"haiku", "opus", "sonnet", "haiku", "the --model flag"},
	}
	for _, c := range cases {
		got, from := deskModel(c.flag, api.Setting{Value: c.set, Configured: c.configured})
		if got != c.want || from != c.from {
			t.Errorf("deskModel(%q, %q, %q) = %q, %q", c.flag, c.set, c.configured, got, from)
		}
	}

	m, _, a := newTestModel()
	a.configured = "sonnet"
	m.desk = ClaudeDesk{Bin: "claude"}
	drive(t, m, m.loadModel(false))
	if d := m.currentDesk().(ClaudeDesk); d.Model != "sonnet" || m.deskName() != "claude · sonnet" {
		t.Errorf("config's model not used: %+v %q", d, m.deskName())
	}
	typeLine(t, m, "/model")
	if !has(m.Lines(), KindInfo, "The desk's model: sonnet (desk.model in config.yaml)") {
		t.Errorf("/model: %+v", m.Lines())
	}
	typeLine(t, m, "/model opus")
	if a.settings[settingModel] != "opus" || m.currentDesk().(ClaudeDesk).Model != "opus" ||
		!has(m.Lines(), KindInfo, "The desk's model: opus (set with /model)") {
		t.Errorf("/model opus: %q %+v", a.settings[settingModel], m.Lines())
	}
	if args := strings.Join(m.currentDesk().(ClaudeDesk).Args("S", "hi", false), " "); !strings.Contains(args, "--model opus") {
		t.Errorf("the turn does not use the model: %s", args)
	}
	typeLine(t, m, "/model reset")
	if a.settings[settingModel] != "" || m.currentDesk().(ClaudeDesk).Model != "sonnet" {
		t.Errorf("/model reset: %q %+v", a.settings[settingModel], m.currentDesk())
	}
	m.ModelFlag = "haiku"
	typeLine(t, m, "/model fable")
	if m.currentDesk().(ClaudeDesk).Model != "haiku" || !has(m.Lines(), KindInfo, "The /model setting, fable, applies when the chat starts without --model") {
		t.Errorf("the flag must win: %+v", m.Lines())
	}
	typeLine(t, m, "/model a b")
	if !has(m.Lines(), KindError, "usage: /model") {
		t.Error("bad /model not refused")
	}
}
