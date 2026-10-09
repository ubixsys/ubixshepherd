package chat

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// API is what the chat needs from the daemon; *client.Client provides it.
type API interface {
	Feed(ctx context.Context, after int64) (api.Feed, error)
	Lanes(ctx context.Context, workspaceID, repoID int64) ([]api.LaneView, error)
	Runs(ctx context.Context, laneID int64, state string, limit int) ([]api.RunView, error)
	RunLog(ctx context.Context, id, offset int64) (api.RunLog, error)
	Decisions(ctx context.Context, state string) ([]api.DecisionView, error)
	Requests(ctx context.Context, states string) ([]api.RequestView, error)
	Answer(ctx context.Context, id int64, answer string) (store.Decision, error)
	Setting(ctx context.Context, key string) (string, error)
	SettingInfo(ctx context.Context, key string) (api.Setting, error)
	SetSetting(ctx context.Context, key, value string) error
	SpendToday(ctx context.Context) (api.SpendToday, error)
	AddSpend(ctx context.Context, sp store.Spend) error
	Sessions(ctx context.Context, repoID int64) ([]api.SessionView, error)
	AskSession(ctx context.Context, id, question string) (convo.Answer, error)
}

// Redialer re-reads daemon.json so the chat can follow a restarted daemon.
type Redialer interface {
	Redial() error
}

// Settings the chat keeps in the daemon.
const (
	settingSession = "desk.session"
	// settingModel is the desk's model set with /model, over desk.model in config.yaml.
	settingModel = "desk.model"
)

// autoKinds are feed items that continue the desk on their own when it is idle: they
// usually need someone to act. A finished run is run_ended from an older daemon, and
// one of the outcome kinds from a newer one.
var autoKinds = map[string]bool{
	store.FeedRunEnded:      true,
	kindRunPassed:           true,
	kindRunFailed:           true,
	kindRunInterrupted:      true,
	kindRunQuota:            true,
	store.FeedRequestStuck:  true,
	store.FeedRequestFailed: true,
}

const (
	activeFeedInterval  = time.Second
	maxIdleFeedInterval = 10 * time.Second
	activePanelInterval = 5 * time.Second
	idlePanelInterval   = 30 * time.Second
	sessionsInterval    = time.Minute
	activeWindow        = 30 * time.Second
	// recentRuns is how many of the latest runs the dock reads, enough to cover
	// every open lane's last run.
	recentRuns = 50
)

// Model is the chat's state.
//
// The chat runs inline, not in the alternate screen: each finished entry is printed above
// the program into the terminal's own scrollback, where the wheel, search, selection and
// copying work as in any shell, and where it stays after the chat ends. The program
// itself draws only a small live region at the bottom: a reply as it streams, the
// desk's status, the agents at work, and the input.
type Model struct {
	ctx       context.Context
	api       API
	desk      Desk
	workspace store.Workspace

	// ModelFlag is the --model the chat was started with; it wins over the rest.
	ModelFlag string
	// model is the desk.model setting and the config value under it, as last read.
	model api.Setting

	// HistoryItems is how many earlier entries to print when the chat starts.
	HistoryItems   int
	loadingHistory bool
	welcomed       bool

	lines     []Line // the whole thread, for the transcript
	unprinted []Line // entries not yet printed to scrollback
	printed   bool   // whether anything has been printed yet, for spacing
	println   func(...any) tea.Cmd

	session   string
	busy      bool
	busySince time.Time
	partial   string // the desk's reply so far, while it streams
	lastTool  string // the desk's latest tool call this turn
	queue     []string
	auto      bool
	deskCh    chan tea.Msg
	pending   []string // events waiting for the desk to be free

	lastFeed         int64
	lanes            []api.LaneView
	runs             []api.RunView // the latest, of every state
	decisions        []api.DecisionView
	requests         []api.RequestView // those needing routing
	lastActivity     time.Time
	lastPanelPoll    time.Time
	lastSessionsPoll time.Time
	clock            func() time.Time
	tick             func(time.Duration, func(time.Time) tea.Msg) tea.Cmd
	tickGeneration   uint64
	focused          bool

	spend    api.SpendToday
	sessions []api.SessionView

	// What the person has seen, for the dock's "done unseen": anything that finished
	// before seenUntil (when the chat started, or they last cleared it), and anything
	// they opened since.
	seenUntil time.Time
	seen      map[string]bool

	// pager is the full-screen reader, when open: a run's log (logRun set) or the
	// transcript.
	pager     *pager
	logRun    int64
	logText   strings.Builder
	logOffset int64
	logDone   bool

	title string // the window title last set

	// focusDecision is the decision in the dock that has the keys, 0 when the input has
	// them; confirm is the option it waits on Enter to answer with.
	focusDecision int64
	confirm       int

	width, height int
	input         textarea.Model
	ready         bool
	reconnecting  bool
	quitting      bool
}

// New returns the chat for a workspace.
func New(ctx context.Context, a API, d Desk, ws store.Workspace) *Model {
	in := textarea.New()
	in.Placeholder = "Talk to Shepherd. /help for commands."
	in.ShowLineNumbers = false
	in.Prompt = "› "
	in.SetHeight(2)
	in.CharLimit = 8000
	in.Focus()
	now := time.Now()
	return &Model{
		ctx: ctx, api: a, desk: d, workspace: ws, HistoryItems: DefaultHistory, auto: true, input: in,
		println: tea.Println, lastFeed: -1, lastActivity: now, clock: time.Now, tick: tea.Tick, focused: true,
		seenUntil: now,
	}
}

// Lines returns the thread so far (for tests).
func (m *Model) Lines() []Line { return m.lines }

// Busy reports whether the desk is mid-turn (for tests).
func (m *Model) Busy() bool { return m.busy }

// Reconnecting reports whether the chat is retrying the daemon (for tests).
func (m *Model) Reconnecting() bool { return m.reconnecting }

type tickMsg struct {
	at         time.Time
	generation uint64
}

type feedMsg api.Feed

type panelMsg struct {
	lanes           []api.LaneView
	runs            []api.RunView
	decisions       []api.DecisionView
	requests        []api.RequestView
	spend           api.SpendToday
	sessions        []api.SessionView
	panelUpdated    bool
	sessionsUpdated bool
}

// modelMsg is the desk.model setting as read; show says to tell the person.
type modelMsg struct {
	setting api.Setting
	show    bool
}

// modelDesk is a desk that can run on a chosen model.
type modelDesk interface {
	WithModel(model string) Desk
}

// deskModel is the desk's model and where it comes from: the --model flag, then the
// override set with /model, then desk.model in config.yaml, then Claude Code's own
// default (model "").
func deskModel(flag string, s api.Setting) (model, from string) {
	switch {
	case flag != "":
		return flag, "the --model flag"
	case s.Value != "":
		return s.Value, "set with /model"
	case s.Configured != "":
		return s.Configured, "desk.model in config.yaml"
	}
	return "", "Claude Code's default"
}

// currentDesk is the desk on the model in force.
func (m *Model) currentDesk() Desk {
	if md, ok := m.desk.(modelDesk); ok {
		model, _ := deskModel(m.ModelFlag, m.model)
		return md.WithModel(model)
	}
	return m.desk
}

// deskName says what the desk runs on, for the status line.
func (m *Model) deskName() string {
	if n, ok := m.currentDesk().(interface{ Name() string }); ok {
		return n.Name()
	}
	return ""
}

// loadModel reads the desk.model setting. A daemon too old to have it leaves the
// model to the flag and Claude Code; that is said only when the person asked.
func (m *Model) loadModel(show bool) tea.Cmd {
	return func() tea.Msg {
		s, err := m.api.SettingInfo(m.ctx, settingModel)
		if err != nil {
			if show {
				return errorMsg{err}
			}
			return nil
		}
		return modelMsg{setting: s, show: show}
	}
}

// setModel stores the /model override ("" clears it), then reads it back to show.
func (m *Model) setModel(model string) tea.Cmd {
	return func() tea.Msg {
		if err := m.api.SetSetting(m.ctx, settingModel, model); err != nil {
			return errorMsg{err}
		}
		return m.loadModel(true)()
	}
}

func (m *Model) modelText() string {
	model, from := deskModel(m.ModelFlag, m.model)
	if model == "" {
		model = "not set"
	}
	text := fmt.Sprintf("The desk's model: %s (%s).", model, from)
	if m.ModelFlag != "" && m.model.Value != "" {
		text += fmt.Sprintf(" The /model setting, %s, applies when the chat starts without --model.", m.model.Value)
	}
	return text + " /model <name> sets it for the next turn on; /model reset goes back to config.yaml."
}

type deskLineMsg Line

type deskDoneMsg struct {
	session string
	err     error
}

type logMsg api.RunLog

type lineMsg Line

type errorMsg struct{ err error }

// reconnectMsg is the outcome of one Redial attempt.
type reconnectMsg struct {
	ok  bool
	err error
}

func (m *Model) Init() tea.Cmd {
	now := m.clock()
	m.lastPanelPoll = now
	m.lastSessionsPoll = now
	m.loadingHistory = true
	return tea.Batch(textarea.Blink, m.loadHistory(), m.openDecisions(), m.pollPanel(true), m.loadModel(false), m.scheduleTick(m.feedInterval(now)))
}

func (m *Model) openDecisions() tea.Cmd {
	return func() tea.Msg {
		ds, err := m.api.Decisions(m.ctx, store.DecisionOpen)
		if err != nil {
			return errorMsg{err}
		}
		if len(ds) == 0 {
			return nil
		}
		return lineMsg{Kind: KindDecision, Text: fmt.Sprintf("%d decision(s) waiting for you: %s. /decisions to see them.", len(ds), decisionIDs(ds))}
	}
}

func decisionIDs(ds []api.DecisionView) string {
	var ids []string
	for _, d := range ds {
		ids = append(ids, strconv.FormatInt(d.ID, 10))
	}
	return strings.Join(ids, ", ")
}

// pollFeed asks for new feed items. Without history, the first poll only learns the
// newest id, so the thread goes on with what happens from now on. While history loads
// it waits: history says where the feed ends.
func (m *Model) pollFeed() tea.Cmd {
	if m.loadingHistory {
		return nil
	}
	after := m.lastFeed
	return func() tea.Msg {
		f, err := m.api.Feed(m.ctx, after)
		if err != nil {
			return errorMsg{err}
		}
		return feedMsg(f)
	}
}

func (m *Model) pollPanel(includeSessions bool) tea.Cmd {
	return m.pollPanelAt(includeSessions, m.clock())
}

func (m *Model) pollPanelAt(includeSessions bool, now time.Time) tea.Cmd {
	m.lastPanelPoll = now
	if includeSessions {
		m.lastSessionsPoll = now
	}
	return func() tea.Msg {
		lanes, err := m.api.Lanes(m.ctx, m.workspace.ID, 0)
		if err != nil {
			return errorMsg{err}
		}
		runs, err := m.api.Runs(m.ctx, 0, "", recentRuns)
		if err != nil {
			return errorMsg{err}
		}
		ds, err := m.api.Decisions(m.ctx, store.DecisionOpen)
		if err != nil {
			return errorMsg{err}
		}
		reqs, err := m.api.Requests(m.ctx, store.RequestNeedsRouting)
		if err != nil {
			return errorMsg{err}
		}
		sp, err := m.api.SpendToday(m.ctx)
		if err != nil {
			return errorMsg{err}
		}
		var ss []api.SessionView
		if includeSessions {
			ss, err = m.api.Sessions(m.ctx, 0)
			if err != nil {
				return errorMsg{err}
			}
		}
		return panelMsg{
			lanes: lanes, runs: runs, decisions: ds, requests: reqs, spend: sp, sessions: ss,
			panelUpdated: true, sessionsUpdated: includeSessions,
		}
	}
}

func (m *Model) pollSessionsAt(now time.Time) tea.Cmd {
	m.lastSessionsPoll = now
	return func() tea.Msg {
		ss, err := m.api.Sessions(m.ctx, 0)
		if err != nil {
			return errorMsg{err}
		}
		return panelMsg{sessions: ss, sessionsUpdated: true}
	}
}

func (m *Model) scheduleTick(interval time.Duration) tea.Cmd {
	m.tickGeneration++
	generation := m.tickGeneration
	return m.tick(interval, func(at time.Time) tea.Msg {
		return tickMsg{at: at, generation: generation}
	})
}

func (m *Model) feedInterval(now time.Time) time.Duration {
	if m.active(now) {
		return activeFeedInterval
	}
	if !m.focused {
		return maxIdleFeedInterval
	}
	idle := now.Sub(m.lastActivity) - activeWindow
	if idle < 0 {
		idle = 0
	}
	steps := int(idle/(10*time.Second)) + 1
	interval := time.Duration(steps*2) * time.Second
	if interval > maxIdleFeedInterval {
		return maxIdleFeedInterval
	}
	return interval
}

func (m *Model) active(now time.Time) bool {
	if !m.focused {
		return false
	}
	if m.busy || (m.logRun != 0 && !m.logDone) || now.Sub(m.lastActivity) < activeWindow {
		return true
	}
	for _, r := range m.runs {
		if r.State == store.RunRunning {
			return true
		}
	}
	return false
}

func (m *Model) panelInterval(now time.Time) time.Duration {
	if m.active(now) {
		return activePanelInterval
	}
	return idlePanelInterval
}

func (m *Model) sessionsDue(now time.Time) bool {
	return m.lastSessionsPoll.IsZero() || now.Sub(m.lastSessionsPoll) >= sessionsInterval
}

func (m *Model) noteActivity(now time.Time) tea.Cmd {
	m.lastActivity = now
	return m.scheduleTick(m.feedInterval(now))
}

func (m *Model) pollLog() tea.Cmd {
	id, off := m.logRun, m.logOffset
	return func() tea.Msg {
		l, err := m.api.RunLog(m.ctx, id, off)
		if err != nil {
			return errorMsg{err}
		}
		return logMsg(l)
	}
}

func (m *Model) reconnect() tea.Cmd {
	return func() tea.Msg {
		r, ok := m.api.(Redialer)
		if !ok {
			return reconnectMsg{ok: false, err: errors.New("daemon unreachable")}
		}
		if err := r.Redial(); err != nil {
			return reconnectMsg{ok: false, err: err}
		}
		// A cheap probe: settings round-trip proves the new address answers.
		if _, err := m.api.Setting(m.ctx, settingSession); err != nil {
			return reconnectMsg{ok: false, err: err}
		}
		return reconnectMsg{ok: true}
	}
}

func isConnErr(err error) bool {
	return err != nil && errors.Is(err, client.ErrNoDaemon)
}

// Update handles a message, then prints whatever finished in the meantime.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if p := m.flush(); p != nil {
		cmd = tea.Batch(cmd, p)
	}
	if t := windowTitle(m.dockItems()); t != m.title && !m.quitting {
		m.title = t
		cmd = tea.Batch(cmd, tea.SetWindowTitle(t))
	}
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(m.width)
		m.ready = true
		m.refreshPager()
	case tea.MouseMsg:
		// The mouse is captured only while the pager is open.
		if m.pager != nil {
			m.pager.update(msg)
		}
	case tea.KeyMsg:
		cmds = append(cmds, m.noteActivity(m.clock()))
		if msg.Type == tea.KeyCtrlC {
			return tea.Batch(append(cmds, m.quit())...)
		}
		if m.pager != nil {
			if m.pager.update(msg) {
				cmds = append(cmds, m.closePager())
			}
			return tea.Batch(cmds...)
		}
		if msg.Type == tea.KeyCtrlO {
			return tea.Batch(append(cmds, m.openPager("transcript"))...)
		}
		if msg.Type == tea.KeyCtrlG {
			m.clearDone()
			return tea.Batch(cmds...)
		}
		if m.focusDecision != 0 {
			return tea.Batch(append(cmds, m.decisionKey(msg))...)
		}
		if msg.Type == tea.KeyTab && m.moveFocus(1) || msg.Type == tea.KeyShiftTab && m.moveFocus(-1) {
			return tea.Batch(cmds...)
		}
		if msg.Type == tea.KeyEnter {
			text := strings.TrimSpace(m.input.Value())
			m.input.Reset()
			if text == "" {
				return tea.Batch(cmds...)
			}
			return tea.Batch(append(cmds, m.handle(text))...)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
	case tickMsg:
		if msg.generation != m.tickGeneration {
			break
		}
		if m.reconnecting {
			cmds = append(cmds, m.reconnect(), m.scheduleTick(activeFeedInterval))
		} else {
			cmds = append(cmds, m.pollFeed())
			if msg.at.Sub(m.lastPanelPoll) >= m.panelInterval(msg.at) {
				cmds = append(cmds, m.pollPanelAt(m.sessionsDue(msg.at), msg.at))
			} else if m.sessionsDue(msg.at) {
				cmds = append(cmds, m.pollSessionsAt(msg.at))
			}
			if m.logRun != 0 && !m.logDone {
				cmds = append(cmds, m.pollLog())
			}
			cmds = append(cmds, m.scheduleTick(m.feedInterval(msg.at)))
		}
	case tea.BlurMsg:
		m.focused = false
		cmds = append(cmds, m.scheduleTick(m.feedInterval(m.clock())))
	case tea.FocusMsg:
		m.focused = true
		now := m.clock()
		m.lastActivity = now
		cmds = append(cmds, m.pollFeed(), m.pollPanelAt(m.sessionsDue(now), now), m.scheduleTick(activeFeedInterval))
	case historyMsg:
		m.onHistory(msg)
	case feedMsg:
		feed := api.Feed(msg)
		if len(feed.Items) > 0 {
			cmds = append(cmds, m.noteActivity(m.clock()))
		}
		cmds = append(cmds, m.onFeed(feed))
	case panelMsg:
		if msg.panelUpdated {
			m.lanes, m.runs, m.decisions, m.requests = msg.lanes, msg.runs, msg.decisions, msg.requests
			if _, ok := m.focusedDecision(); !ok && m.focusDecision != 0 {
				// Answered elsewhere: the keys go back to the input.
				m.focusInput()
			}
			m.spend = msg.spend
		}
		if msg.sessionsUpdated {
			m.sessions = msg.sessions
		}
	case deskLineMsg:
		switch msg.Kind {
		case KindCost:
			usd, _ := strconv.ParseFloat(msg.Text, 64)
			cmds = append(cmds, func() tea.Msg { m.api.AddSpend(m.ctx, store.Spend{Source: "desk", USD: usd}); return nil })
		case KindPartial:
			m.partial += msg.Text
		default:
			m.partial = ""
			if msg.Kind == KindTool {
				m.lastTool = msg.Text
			}
			m.add(Line(msg))
		}
		cmds = append(cmds, waitDesk(m.deskCh))
	case deskDoneMsg:
		cmds = append(cmds, m.onDeskDone(msg))
	case logMsg:
		m.logText.WriteString(msg.Data)
		m.logOffset, m.logDone = msg.Offset, msg.Done
		m.refreshPager()
	case lineMsg:
		m.add(Line(msg))
	case modelMsg:
		m.model = msg.setting
		if msg.show {
			m.add(Line{Kind: KindInfo, Text: m.modelText()})
		}
	case errorMsg:
		if m.loadingHistory && !isConnErr(msg.err) {
			// History could not be read; the chat goes on from now. (When the daemon is
			// down, history loads again once it is back.)
			m.loadingHistory = false
			m.welcome()
		}
		if isConnErr(msg.err) {
			if !m.reconnecting {
				m.reconnecting = true
				cmds = append(cmds, m.reconnect(), m.scheduleTick(activeFeedInterval))
			}
			break
		}
		m.add(Line{Kind: KindError, Text: msg.err.Error()})
	case reconnectMsg:
		if msg.ok {
			m.reconnecting = false
			now := m.clock()
			cmds = append(cmds, m.pollFeed(), m.pollPanelAt(m.sessionsDue(now), now), m.scheduleTick(m.feedInterval(now)))
			if m.loadingHistory {
				cmds = append(cmds, m.loadHistory())
			}
			if m.logRun != 0 && !m.logDone {
				cmds = append(cmds, m.pollLog())
			}
		}
		// Still down: stay in reconnecting; the next tick retries.
	}
	return tea.Batch(cmds...)
}

// quit prints what is left, clears the live region and the window title, and ends the
// program. (The title the terminal had before is restored by SaveTitle's caller.)
func (m *Model) quit() tea.Cmd {
	m.quitting = true
	m.pager = nil
	return tea.Sequence(m.flush(), tea.SetWindowTitle(""), tea.Quit)
}

// Title is the window title the chat last set (for tests).
func (m *Model) Title() string { return m.title }

// openLog shows a run's log in the pager.
func (m *Model) openLog(id int64) tea.Cmd {
	m.logRun, m.logOffset, m.logDone = id, 0, false
	m.markSeen(fmt.Sprintf("run:%d", id))
	m.logText.Reset()
	return tea.Batch(m.openPager(fmt.Sprintf("run %d", id)), m.pollLog())
}

// openPager enters the alternate screen with the pager; entries that finish meanwhile
// wait, and are printed when it closes.
func (m *Model) openPager(title string) tea.Cmd {
	m.pager = newPager(title, m.width, m.height)
	m.refreshPager()
	return tea.Batch(tea.EnterAltScreen, tea.EnableMouseCellMotion)
}

func (m *Model) closePager() tea.Cmd {
	m.pager = nil
	m.logRun = 0
	return tea.Sequence(tea.ExitAltScreen, tea.DisableMouse, m.flush())
}

// refreshPager renders the pager's content again, at the current width.
func (m *Model) refreshPager() {
	if m.pager == nil {
		return
	}
	m.pager.resize(m.width, m.height)
	if m.logRun == 0 {
		m.pager.setLines(strings.Split(m.renderTranscript(), "\n"))
		return
	}
	state := "following"
	if m.logDone {
		state = "ended"
	}
	m.pager.title = fmt.Sprintf("run %d (%s)", m.logRun, state)
	text := strings.TrimRight(m.logText.String(), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(wrapText(text, m.width), "\n")
	}
	m.pager.setLines(lines)
}

// handle is what the person typed: a command, or a message for the desk.
func (m *Model) handle(text string) tea.Cmd {
	if !strings.HasPrefix(text, "/") {
		m.add(Line{Kind: KindYou, Text: text})
		return m.send(text)
	}
	f := strings.Fields(text)
	switch f[0] {
	case "/help":
		m.add(Line{Kind: KindInfo, Text: "/answer <decision> <option number or words>   answer a decision yourself\n" +
			"Tab / Shift-Tab   move to the decisions in the dock and between them; a number answers with that option (Enter confirms when asked), Enter alone answers in your own words, Esc goes back\n" +
			"/sessions   your adopted conversations; /attach <id> reopens one here, /ask <id> <question> asks it\n" +
			"/decisions   decisions waiting for you\n/log <run>   a run's live output (q to come back)\n" +
			"/auto on|off   let swarm events reach the desk on their own (on)\n/new   start a new conversation with the desk\n" +
			"/model [<name>|reset]   the desk's model: show it, set it, or go back to config.yaml\n" +
			"/quit   leave (Ctrl-C too)\n" +
			"The dock above the input: what needs you, what is broken, to review, working, and done since you last looked. Ctrl-G clears done; opening a run's log does too.\n" +
			"The conversation is printed into your terminal: scroll, search, select and copy there as usual.\n" +
			"Ctrl-O   the whole transcript: / search, n/N next/previous, g/G top/bottom, PgUp/PgDn, q or Esc back"})
	case "/quit", "/exit":
		return m.quit()
	case "/new":
		m.session = ""
		m.add(Line{Kind: KindInfo, Text: "The next message starts a new conversation with the desk."})
		return func() tea.Msg { m.api.SetSetting(m.ctx, settingSession, ""); return nil }
	case "/model":
		switch {
		case len(f) == 1:
			return m.loadModel(true)
		case len(f) == 2 && f[1] == "reset":
			return m.setModel("")
		case len(f) == 2:
			return m.setModel(f[1])
		}
		m.add(Line{Kind: KindError, Text: "usage: /model [<name>|reset]"})
	case "/auto":
		if len(f) == 2 && (f[1] == "on" || f[1] == "off") {
			m.auto = f[1] == "on"
		}
		m.add(Line{Kind: KindInfo, Text: fmt.Sprintf("Swarm events reach the desk on their own: %v.", m.auto)})
	case "/decisions":
		return func() tea.Msg {
			ds, err := m.api.Decisions(m.ctx, store.DecisionOpen)
			if err != nil {
				return errorMsg{err}
			}
			if len(ds) == 0 {
				return lineMsg{Kind: KindInfo, Text: "No decisions waiting for you."}
			}
			var b strings.Builder
			for _, d := range ds {
				b.WriteString(decisionText(d))
				b.WriteString("\n")
			}
			return lineMsg{Kind: KindDecision, Text: strings.TrimSpace(b.String())}
		}
	case "/answer":
		if len(f) < 3 {
			m.add(Line{Kind: KindError, Text: "usage: /answer <decision> <option number or words>"})
			return nil
		}
		id, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			m.add(Line{Kind: KindError, Text: "decision id must be a number"})
			return nil
		}
		answer := strings.Join(f[2:], " ")
		m.add(Line{Kind: KindYou, Text: fmt.Sprintf("answer to decision %d: %s", id, answer)})
		return m.answer(id, answer)
	case "/sessions":
		return func() tea.Msg {
			ss, err := m.api.Sessions(m.ctx, 0)
			if err != nil {
				return errorMsg{err}
			}
			if len(ss) == 0 {
				return lineMsg{Kind: KindInfo, Text: "No conversations adopted. From a shell: shepherd session import"}
			}
			var b strings.Builder
			for _, s := range ss {
				open := ""
				if s.InUse {
					open = " (may be open)"
				}
				fmt.Fprintf(&b, "%s  %-10s %s  ·  %s%s\n", s.ID[:8], s.Repo, clipTo(s.Title, 70), s.Last.Local().Format("Jan 2"), open)
			}
			b.WriteString("/attach <id> to reopen one here; /ask <id> <question> to ask it")
			return lineMsg{Kind: KindInfo, Text: b.String()}
		}
	case "/attach":
		if len(f) != 2 {
			m.add(Line{Kind: KindError, Text: "usage: /attach <conversation id>"})
			return nil
		}
		s, err := m.conversation(f[1])
		if err != nil {
			m.add(Line{Kind: KindError, Text: err.Error()})
			return nil
		}
		bin, err := exec.LookPath("claude")
		if err != nil {
			m.add(Line{Kind: KindError, Text: "claude is not on PATH"})
			return nil
		}
		if s.InUse {
			m.add(Line{Kind: KindInfo, Text: "That conversation changed in the last few minutes; if it is open in another terminal, use that one."})
		}
		m.add(Line{Kind: KindInfo, Text: fmt.Sprintf("Opening conversation %s (%s) in Claude Code; exit it to come back here.", s.ID[:8], clipTo(s.Title, 60))})
		cmd := exec.Command(bin, "--resume", s.ID)
		cmd.Dir = s.Dir
		id := s.ID[:8]
		// Print the line above before Claude Code takes the terminal.
		return tea.Sequence(m.flush(), tea.ExecProcess(cmd, func(err error) tea.Msg {
			if err != nil {
				return lineMsg{Kind: KindInfo, Text: fmt.Sprintf("Back from conversation %s (%v).", id, err)}
			}
			return lineMsg{Kind: KindInfo, Text: fmt.Sprintf("Back from conversation %s.", id)}
		}))
	case "/ask":
		if len(f) < 3 {
			m.add(Line{Kind: KindError, Text: "usage: /ask <conversation id> <question>"})
			return nil
		}
		s, err := m.conversation(f[1])
		if err != nil {
			m.add(Line{Kind: KindError, Text: err.Error()})
			return nil
		}
		q := strings.Join(f[2:], " ")
		m.add(Line{Kind: KindYou, Text: fmt.Sprintf("to conversation %s: %s", s.ID[:8], q)})
		id, title := s.ID, s.Title
		return func() tea.Msg {
			a, err := m.api.AskSession(m.ctx, id, q)
			if err != nil {
				return errorMsg{err}
			}
			return lineMsg{Kind: KindDesk, Text: fmt.Sprintf("conversation %s (%s):\n%s", id[:8], clipTo(title, 50), a.Text)}
		}
	case "/log":
		if len(f) != 2 {
			m.add(Line{Kind: KindError, Text: "usage: /log <run>"})
			return nil
		}
		id, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			m.add(Line{Kind: KindError, Text: "run id must be a number"})
			return nil
		}
		return m.openLog(id)
	default:
		m.add(Line{Kind: KindError, Text: "unknown command " + f[0] + "; /help"})
	}
	return nil
}

// answer records the person's own answer, turning a bare option number into the option.
func (m *Model) answer(id int64, answer string) tea.Cmd {
	return func() tea.Msg {
		if n, err := strconv.Atoi(answer); err == nil {
			if ds, err := m.api.Decisions(m.ctx, ""); err == nil {
				for _, d := range ds {
					if d.ID == id && n >= 1 && n <= len(d.Options) {
						answer = fmt.Sprintf("option %d: %s", n, d.Options[n-1])
					}
				}
			}
		}
		d, err := m.api.Answer(m.ctx, id, answer)
		if err != nil {
			return errorMsg{err}
		}
		if d.AnswerRun != 0 {
			return lineMsg{Kind: KindInfo, Text: fmt.Sprintf("Answered decision %d; the agent carries on as run %d.", d.ID, d.AnswerRun)}
		}
		return lineMsg{Kind: KindInfo, Text: fmt.Sprintf("Answered decision %d; the agent gets it when its turn ends.", d.ID)}
	}
}

// conversation finds an adopted conversation by id or prefix, among those last polled.
func (m *Model) conversation(prefix string) (api.SessionView, error) {
	var hit []api.SessionView
	for _, s := range m.sessions {
		if strings.HasPrefix(s.ID, prefix) {
			hit = append(hit, s)
		}
	}
	switch len(hit) {
	case 0:
		return api.SessionView{}, fmt.Errorf("no conversation starts with %s (/sessions)", prefix)
	case 1:
		return hit[0], nil
	}
	return api.SessionView{}, fmt.Errorf("%d conversations start with %s; give more of the id", len(hit), prefix)
}

func decisionText(d api.DecisionView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "decision %d from %s in lane %s: %s", d.ID, d.Agent, d.Lane, d.Question)
	for i, o := range d.Options {
		fmt.Fprintf(&b, "\n   %d. %s", i+1, o)
	}
	if d.Recommendation != "" {
		fmt.Fprintf(&b, "\n   recommends: %s", d.Recommendation)
	}
	fmt.Fprintf(&b, "\n   /answer %d <option or words>", d.ID)
	return b.String()
}

// send gives the desk a message, or queues it while the desk is mid-turn.
func (m *Model) send(text string) tea.Cmd {
	if m.busy {
		m.queue = append(m.queue, text)
		return nil
	}
	m.busy, m.busySince, m.partial, m.lastTool = true, m.clock(), "", ""
	ch := make(chan tea.Msg, 64)
	m.deskCh = ch
	session, desk := m.session, m.currentDesk()
	go func() {
		s, err := desk.Turn(m.ctx, session, text, func(l Line) { ch <- deskLineMsg(l) })
		ch <- deskDoneMsg{s, err}
	}()
	return waitDesk(ch)
}

func waitDesk(ch chan tea.Msg) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return <-ch }
}

func (m *Model) onDeskDone(msg deskDoneMsg) tea.Cmd {
	m.busy = false
	if m.partial != "" {
		// The stream ended without the whole reply: keep what came.
		m.add(Line{Kind: KindDesk, Text: strings.TrimSpace(m.partial)})
		m.partial = ""
	}
	var cmds []tea.Cmd
	if msg.err != nil {
		m.add(Line{Kind: KindError, Text: "the desk: " + msg.err.Error()})
	}
	if msg.session != "" && msg.session != m.session {
		m.session = msg.session
		s := msg.session
		cmds = append(cmds, func() tea.Msg { m.api.SetSetting(m.ctx, settingSession, s); return nil })
	}
	switch {
	case len(m.queue) > 0:
		next := m.queue[0]
		m.queue = m.queue[1:]
		cmds = append(cmds, m.send(next))
	case len(m.pending) > 0 && m.auto:
		cmds = append(cmds, m.brief())
	}
	return tea.Batch(cmds...)
}

// onFeed adds new swarm events to the thread, and briefs the desk on the ones that need
// someone to act.
func (m *Model) onFeed(f api.Feed) tea.Cmd {
	first := m.lastFeed < 0
	m.lastFeed = f.Last
	if first {
		return nil
	}
	for i, it := range f.Items {
		m.add(feedLine(it, feedEvent(f, i), true))
		m.noteOutcome(it, feedEvent(f, i))
		if autoKinds[it.Kind] {
			m.pending = append(m.pending, it.Text)
		}
	}
	if len(m.pending) > 0 && m.auto && !m.busy {
		return m.brief()
	}
	return nil
}

// brief continues the desk with the events waiting for it.
func (m *Model) brief() tea.Cmd {
	msg := "[Shepherd] Since your last turn:\n- " + strings.Join(m.pending, "\n- ") +
		"\nTell the person briefly what matters and what needs them. Route any request that needs routing. Do not start new work they have not asked for."
	m.pending = nil
	return m.send(msg)
}

// add appends to the thread; the entry is printed to scrollback at the end of the update.
func (m *Model) add(l Line) {
	m.lines = append(m.lines, l)
	m.unprinted = append(m.unprinted, l)
	if m.pager != nil && m.logRun == 0 {
		m.refreshPager()
	}
}

// renderTranscript is the whole thread as printed, for the transcript view.
func (m *Model) renderTranscript() string {
	var b strings.Builder
	for i, l := range m.lines {
		if i > 0 {
			b.WriteString("\n")
			if l.Kind != KindTool {
				b.WriteString("\n")
			}
		}
		b.WriteString(renderLine(l, m.width))
	}
	return b.String()
}

// flush prints the entries waiting for scrollback. Nothing is printed before the
// terminal's width is known, or while the pager holds the alternate screen.
func (m *Model) flush() tea.Cmd {
	if !m.ready || m.pager != nil || len(m.unprinted) == 0 {
		return nil
	}
	var parts []string
	for _, l := range m.unprinted {
		s := renderLine(l, m.width)
		// A burst of tool calls stays together; everything else gets a blank line.
		if m.printed && l.Kind != KindTool {
			s = "\n" + s
		}
		m.printed = true
		parts = append(parts, s)
	}
	m.unprinted = nil
	return m.println(strings.Join(parts, "\n"))
}

// View is the live region: the reply streaming in, the desk's status, the dock, the
// input and a line of keys. Each line is cut to the terminal's width so the region never
// wraps.
func (m *Model) View() string {
	if m.quitting || !m.ready {
		return ""
	}
	if m.pager != nil {
		return m.pager.view()
	}
	items := m.dockItems()
	var rows []string
	dock := m.dockRows(items)
	d, deciding := m.focusedDecision()
	if deciding && len(dock) > 1 && 1+len(dock)+decisionHeight(d) > m.height {
		// The decision's options come first: the dock keeps its counts.
		dock = dock[:1]
	}
	rows = append(rows, m.streaming(len(dock))...)
	rows = append(rows, m.statusLine())
	rows = append(rows, dock...)
	if deciding {
		rows = append(rows, m.decisionRows(d, max(3, m.height-len(rows)))...)
	} else {
		rows = append(rows, strings.Split(m.input.View(), "\n")...)
		if m.height >= 8 {
			rows = append(rows, styleInfo.Render(m.keysHint(items)))
		}
	}
	for i, r := range rows {
		rows[i] = fit(r, m.width)
	}
	return strings.Join(rows, "\n")
}

// keysHint is the line of keys under the input, with the dock's keys when they apply.
func (m *Model) keysHint(items []dockItem) string {
	keys := keysWithDecisions([]string{"Enter send"}, items)
	if n := counts(items); n[groupDone] > 0 {
		keys = append(keys, "Ctrl-G clear done")
	}
	return strings.Join(append(keys, "Ctrl-O transcript", "/help", "Ctrl-C quit"), " · ")
}

// streaming is the tail of the reply as it streams, short enough to leave room for the
// rest of the live region.
func (m *Model) streaming(dockRows int) []string {
	text := strings.TrimSpace(m.partial)
	if text == "" {
		return nil
	}
	lines := strings.Split(renderLine(Line{Kind: KindDesk, Text: text}, m.width), "\n")
	room := m.height - 6 - dockRows
	if room < 1 {
		room = 1
	}
	if room > 12 {
		room = 12
	}
	if len(lines) > room {
		lines = append([]string{styleInfo.Render("  …")}, lines[len(lines)-room+1:]...)
	}
	return append(lines, "")
}

func (m *Model) statusLine() string {
	var s string
	switch {
	case m.reconnecting:
		s = styleError.Render("○ reconnecting to the daemon…")
	case m.busy:
		s = styleDeskMark.Render("●") + " desk working… " + styleInfo.Render(m.clock().Sub(m.busySince).Truncate(time.Second).String())
		if n := len(m.queue); n > 0 {
			s += styleInfo.Render(fmt.Sprintf(" · %d queued", n))
		}
		if m.lastTool != "" {
			s += styleTool.Render(" · → " + m.lastTool)
		}
	default:
		s = styleInfo.Render("○ desk ready")
	}
	if name := m.deskName(); name != "" && !m.reconnecting {
		s += styleInfo.Render(" · " + name)
	}
	return s + styleInfo.Render(fmtUSD(m.spend.USD, m.spend.Budget, m.spend.Day))
}
