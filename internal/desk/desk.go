package desk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// ErrQueueFull refuses a turn when the queue is at its bound.
var ErrQueueFull = errors.New("the desk has too many turns waiting; wait for it, or interrupt it")

// ErrClosed refuses a turn once the daemon is stopping.
var ErrClosed = errors.New("the daemon is stopping")

// Tokens mint the token each turn's operator tools call the daemon with. The daemon
// provides them.
type Tokens interface {
	// MintDesk returns a desk token; human says the turn was started by the person, so
	// its tools may record their answer to a decision.
	MintDesk(human bool, session string) (string, error)
	// RevokeToken ends one token.
	RevokeToken(token string)
	// URL is the daemon's API base, http://host:port.
	URL() string
}

// Options configure a Manager. Zero values take the defaults below.
type Options struct {
	Store  store.Store
	Agent  Agent
	Tokens Tokens
	// Spend records a turn's cost; nil writes it to the store directly.
	Spend func(ctx context.Context, sp store.Spend) error
	// Model is the model a turn uses: the desk.model setting, else config's.
	Model func(ctx context.Context) string
	// Wake is desk.wake: config.WakeAttached, WakeAlways or WakeNever.
	Wake func() string
	Log  *slog.Logger
	// Grace is how long after the last client leaves the desk still wakes on its own.
	Grace time.Duration
	// QueueMax bounds the turns waiting; DigestMax the events waiting for a client.
	QueueMax, DigestMax int
	// Keep is how many events each workspace's conversation keeps.
	Keep int
}

// Defaults.
const (
	DefaultGrace     = 30 * time.Second
	DefaultQueueMax  = 16
	DefaultDigestMax = 20
	DefaultKeep      = 5000
)

// Manager holds the desks, one per workspace, created when first used.
type Manager struct {
	o      Options
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	desks  map[int64]*Desk
	wg     sync.WaitGroup
	closed bool
}

// NewManager returns a Manager; Close stops it.
func NewManager(o Options) *Manager {
	if o.Grace == 0 {
		o.Grace = DefaultGrace
	}
	if o.QueueMax == 0 {
		o.QueueMax = DefaultQueueMax
	}
	if o.DigestMax == 0 {
		o.DigestMax = DefaultDigestMax
	}
	if o.Keep == 0 {
		o.Keep = DefaultKeep
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Model == nil {
		o.Model = func(context.Context) string { return "" }
	}
	if o.Wake == nil {
		o.Wake = func() string { return config.WakeAttached }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{o: o, ctx: ctx, cancel: cancel, desks: map[int64]*Desk{}}
}

// For returns the workspace's desk, starting it on first use. A desk starting after a
// daemon stopped mid-turn closes the turns it left open.
func (m *Manager) For(ws store.Workspace) (*Desk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if d, ok := m.desks[ws.ID]; ok {
		return d, nil
	}
	d := &Desk{m: m, ws: ws, kick: make(chan struct{}, 1), changed: make(chan struct{}), subs: map[*Sub]bool{}}
	if err := d.recover(m.ctx); err != nil {
		return nil, err
	}
	m.desks[ws.ID] = d
	m.wg.Add(1)
	go d.loop()
	return d, nil
}

// Wake tells every desk about new feed items; those that need someone to act wake it
// (see wakes), as desk.wake allows.
func (m *Manager) Wake(items []store.FeedItem) {
	var texts []string
	for _, it := range items {
		if wakes(it.Kind) {
			texts = append(texts, it.Text)
		}
	}
	if len(texts) == 0 || m.o.Wake() == config.WakeNever {
		return
	}
	m.mu.Lock()
	desks := make([]*Desk, 0, len(m.desks))
	for _, d := range m.desks {
		desks = append(desks, d)
	}
	m.mu.Unlock()
	for _, d := range desks {
		d.note(texts)
	}
}

// wakes says whether a feed item continues the desk on its own: a run ended, a decision
// waits for the person, or a request needs routing. The desk's own conversation never
// reaches the feed, so it cannot wake itself.
func wakes(kind string) bool {
	switch api.EventKind(kind) {
	case api.EventRunEnded, api.EventRunPassed, api.EventRunFailed, api.EventRunInterrupted,
		api.EventRunQuota, api.EventDecisionAsked, api.EventRequestAttention:
		return true
	}
	return false
}

// Close interrupts every turn, ends every stream and waits for the desks to stop.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	desks := m.desks
	m.mu.Unlock()
	m.cancel()
	for _, d := range desks {
		d.mu.Lock()
		d.wakeLocked()
		d.mu.Unlock()
	}
	m.wg.Wait()
}

// Done is closed when the manager is closing: streams end on it.
func (m *Manager) Done() <-chan struct{} { return m.ctx.Done() }

// Desk is one workspace's conversation.
type Desk struct {
	m  *Manager
	ws store.Workspace

	mu    sync.Mutex
	queue []turn
	cur   *turn
	// stop interrupts the turn in progress.
	stop        context.CancelFunc
	interrupted bool
	// gen counts new conversations, so a turn cut short by one does not keep its session.
	gen int
	// pending are the swarm's events waiting for the desk; dropped counts the oldest
	// let go once there were DigestMax.
	pending    []string
	dropped    int
	subs       map[*Sub]bool
	lastDetach time.Time
	// changed is closed, and replaced, whenever an event is stored.
	changed chan struct{}
	kick    chan struct{}
}

type turn struct {
	id     int64
	origin string
	text   string
}

// Sub is a client following the desk: it counts as attached while subscribed, and
// receives the partial replies, which are never stored.
type Sub struct {
	Live chan store.DeskEvent
}

func settingSession(ws int64) string { return fmt.Sprintf("desk.daemon.%d.session", ws) }
func settingCost(ws int64) string    { return fmt.Sprintf("desk.daemon.%d.cost", ws) }

// Workspace is the workspace whose conversation this is.
func (d *Desk) Workspace() store.Workspace { return d.ws }

// Session is the agent session the conversation resumes, "" before its first turn.
func (d *Desk) Session(ctx context.Context) string {
	s, _ := d.m.o.Store.Setting(ctx, settingSession(d.ws.ID))
	return s
}

// Send queues the person's message as a turn and returns the turn's id.
func (d *Desk) Send(ctx context.Context, text string) (int64, int, error) {
	if strings.TrimSpace(text) == "" {
		return 0, 0, errors.New("say something")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m.ctx.Err() != nil {
		return 0, 0, ErrClosed
	}
	if len(d.queue) >= d.m.o.QueueMax {
		return 0, len(d.queue), ErrQueueFull
	}
	e, err := d.appendLocked(ctx, store.DeskEvent{Kind: store.DeskUser, Text: text, Origin: store.DeskByHuman})
	if err != nil {
		return 0, 0, err
	}
	d.queue = append(d.queue, turn{id: e.Turn, origin: store.DeskByHuman, text: text})
	ahead := len(d.queue) - 1
	if d.cur != nil {
		ahead++
	}
	d.wakeLocked()
	return e.Turn, ahead, nil
}

// Interrupt stops the turn in progress, if any. Queued turns still run.
func (d *Desk) Interrupt() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur == nil {
		return false
	}
	d.interrupted = true
	d.stop()
	return true
}

// New starts a new conversation: the turn in progress is interrupted, those queued are
// dropped, and the next turn starts a new session with the brief. The history stays.
func (d *Desk) New(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur != nil {
		d.interrupted = true
		d.stop()
	}
	for _, t := range d.queue {
		d.appendLocked(ctx, store.DeskEvent{Kind: store.DeskTurnEnd, Turn: t.id, Origin: t.origin, Text: "dropped: a new conversation was started"})
	}
	d.queue = nil
	d.pending, d.dropped = nil, 0
	d.gen++
	if err := d.m.o.Store.SetSetting(ctx, settingSession(d.ws.ID), ""); err != nil {
		return err
	}
	_, err := d.appendLocked(ctx, store.DeskEvent{Kind: store.DeskNew, Origin: store.DeskByHuman})
	return err
}

// Subscribe attaches a client. Events waiting as a digest are delivered now.
func (d *Desk) Subscribe() *Sub {
	s := &Sub{Live: make(chan store.DeskEvent, 256)}
	d.mu.Lock()
	d.subs[s] = true
	d.wakeLocked()
	d.mu.Unlock()
	return s
}

// Unsubscribe detaches a client.
func (d *Desk) Unsubscribe(s *Sub) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.subs[s] {
		delete(d.subs, s)
		d.lastDetach = time.Now()
	}
}

// Attached is how many clients follow the desk.
func (d *Desk) Attached() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.subs)
}

// Changed is closed when the next event is stored.
func (d *Desk) Changed() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changed
}

// Busy says whether a turn is in progress, and how many wait.
func (d *Desk) Busy() (bool, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur != nil, len(d.queue)
}

// note keeps the swarm's events for the desk, newest winning past DigestMax.
func (d *Desk) note(texts []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = append(d.pending, texts...)
	if over := len(d.pending) - d.m.o.DigestMax; over > 0 {
		d.pending = d.pending[over:]
		d.dropped += over
	}
	d.wakeLocked()
}

// mayWakeLocked says whether desk.wake lets the desk take a turn on its own now.
func (d *Desk) mayWakeLocked() bool {
	switch d.m.o.Wake() {
	case config.WakeAlways:
		return true
	case config.WakeNever:
		return false
	}
	return len(d.subs) > 0 || (!d.lastDetach.IsZero() && time.Since(d.lastDetach) < d.m.o.Grace)
}

func (d *Desk) wakeLocked() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// loop runs turns one at a time until the manager closes.
func (d *Desk) loop() {
	defer d.m.wg.Done()
	for {
		t, ok := d.next()
		if !ok {
			return
		}
		d.run(t)
	}
}

// next waits for the next turn: the person's first, then a digest of the swarm's events
// when the desk may wake.
func (d *Desk) next() (turn, bool) {
	for {
		d.mu.Lock()
		if d.m.ctx.Err() != nil {
			d.mu.Unlock()
			return turn{}, false
		}
		if len(d.queue) > 0 {
			t := d.queue[0]
			d.queue = d.queue[1:]
			d.mu.Unlock()
			return t, true
		}
		if len(d.pending) > 0 && d.mayWakeLocked() {
			text := digest(d.pending, d.dropped)
			d.pending, d.dropped = nil, 0
			e, err := d.appendLocked(d.m.ctx, store.DeskEvent{Kind: store.DeskSystem, Text: text, Origin: store.DeskBySystem})
			d.mu.Unlock()
			if err != nil {
				d.m.o.Log.Error("desk: record a wake-up", "workspace", d.ws.Name, "err", err)
				continue
			}
			return turn{id: e.Turn, origin: store.DeskBySystem, text: text}, true
		}
		d.mu.Unlock()
		// Waiting events may become deliverable when the grace period after the last
		// client left runs out (they then wait for the next) or a client attaches.
		select {
		case <-d.kick:
		case <-d.m.ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// digest is the one message the swarm's waiting events become.
func digest(items []string, dropped int) string {
	msg := "[Shepherd] Since your last turn:\n- " + strings.Join(items, "\n- ")
	if dropped > 0 {
		msg += fmt.Sprintf("\n(and %d earlier event(s), left out)", dropped)
	}
	return msg + "\nTell the person briefly what matters and what needs them. Route any request that needs routing. Do not start new work they have not asked for."
}

// run runs one turn and records what it produced.
func (d *Desk) run(t turn) {
	o := d.m.o
	ctx, stop := context.WithCancel(d.m.ctx)
	defer stop()
	d.mu.Lock()
	d.cur, d.stop, d.interrupted = &t, stop, false
	gen := d.gen
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.cur, d.stop = nil, nil
		d.mu.Unlock()
	}()
	bg := context.WithoutCancel(ctx)
	add := func(kind, text string) {
		if _, err := d.append(bg, store.DeskEvent{Kind: kind, Text: text, Turn: t.id, Origin: t.origin}); err != nil {
			o.Log.Error("desk: record", "kind", kind, "err", err)
		}
	}
	end := func(how string) {
		add(store.DeskTurnEnd, how)
		if err := o.Store.TrimDeskEvents(bg, d.ws.ID, o.Keep); err != nil {
			o.Log.Error("desk: trim history", "err", err)
		}
	}
	add(store.DeskTurnStart, "")

	session := d.Session(bg)
	fresh := session == ""
	if fresh {
		var err error
		if session, err = newUUID(); err != nil {
			add(store.DeskError, err.Error())
			end("failed")
			return
		}
	}
	tok, err := o.Tokens.MintDesk(t.origin == store.DeskByHuman, session)
	if err != nil {
		add(store.DeskError, "the desk's token: "+err.Error())
		end("failed")
		return
	}
	defer o.Tokens.RevokeToken(tok)

	var partial strings.Builder
	produced := false
	emit := func(l Line) {
		switch l.Kind {
		case KindPartial:
			partial.WriteString(l.Text)
			d.live(store.DeskEvent{WorkspaceID: d.ws.ID, Kind: KindPartial, Text: l.Text, Turn: t.id, Origin: t.origin, Created: time.Now().UTC()})
			return
		case store.DeskCost:
			d.cost(bg, session, l.Text, add)
			return
		}
		partial.Reset()
		produced = true
		add(l.Kind, redact.String(l.Text))
	}
	model := o.Model(bg)
	err = o.Agent.Turn(ctx, Spec{Session: session, New: fresh, Message: t.text, Dir: d.ws.Path, Model: model,
		Env: []string{dispatch.EnvToken + "=" + tok, dispatch.EnvURL + "=" + o.Tokens.URL()}}, emit)
	if rest := strings.TrimSpace(partial.String()); rest != "" {
		// The stream ended without the whole reply: keep what came.
		add(store.DeskAssistant, redact.String(rest))
		produced = true
	}
	d.mu.Lock()
	interrupted, renewed := d.interrupted, d.gen != gen
	d.mu.Unlock()
	// A new session exists once the agent has said anything in it.
	if fresh && !renewed && (err == nil || produced) {
		if serr := o.Store.SetSetting(bg, settingSession(d.ws.ID), session); serr != nil {
			o.Log.Error("desk: keep the session", "err", serr)
		}
	}
	switch {
	case interrupted:
		end("interrupted")
	case d.m.ctx.Err() != nil:
		end("interrupted: the daemon stopped")
	case err != nil:
		add(store.DeskError, "the desk: "+redact.String(err.Error()))
		end("failed")
	default:
		end("done")
	}
}

// cost records what the turn added to the session's cost. Claude Code reports the
// session's total, so the turn's own is the difference from the last total recorded for
// this session (dispatch.SessionDelta).
func (d *Desk) cost(ctx context.Context, session, total string, add func(kind, text string)) {
	o := d.m.o
	usd, err := strconv.ParseFloat(total, 64)
	if err != nil || usd <= 0 {
		return
	}
	before := 0.0
	if last, _ := o.Store.Setting(ctx, settingCost(d.ws.ID)); last != "" {
		if s, v, ok := strings.Cut(last, " "); ok && s == session {
			before, _ = strconv.ParseFloat(v, 64)
		}
	}
	o.Store.SetSetting(ctx, settingCost(d.ws.ID), session+" "+total)
	delta := dispatch.SessionDelta(usd, before)
	if delta <= 0 {
		return
	}
	sp := store.Spend{Source: store.OriginDesk, USD: delta}
	if o.Spend != nil {
		err = o.Spend(ctx, sp)
	} else {
		sp.Day = dispatch.Today()
		err = o.Store.AddSpend(ctx, sp)
	}
	if err != nil {
		o.Log.Error("desk: record spend", "err", err)
	}
	add(store.DeskCost, strconv.FormatFloat(delta, 'f', -1, 64))
}

func (d *Desk) append(ctx context.Context, e store.DeskEvent) (store.DeskEvent, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.appendLocked(ctx, e)
}

// appendLocked stores an event and tells the streams.
func (d *Desk) appendLocked(ctx context.Context, e store.DeskEvent) (store.DeskEvent, error) {
	e.WorkspaceID = d.ws.ID
	e, err := d.m.o.Store.AddDeskEvent(ctx, e)
	if err != nil {
		return e, err
	}
	close(d.changed)
	d.changed = make(chan struct{})
	return e, nil
}

// live sends a partial reply to every client following, dropping it for one too slow
// to keep up: the whole reply is stored and follows.
func (d *Desk) live(e store.DeskEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for s := range d.subs {
		select {
		case s.Live <- e:
		default:
		}
	}
}

// recover closes the turns a stopped daemon left open, so a client never waits on one.
func (d *Desk) recover(ctx context.Context) error {
	recent, err := d.m.o.Store.DeskHistory(ctx, d.ws.ID, 0, 200)
	if err != nil {
		return err
	}
	open := map[int64]string{}
	var order []int64
	for _, e := range recent {
		switch e.Kind {
		case store.DeskUser, store.DeskSystem:
			open[e.Turn] = e.Origin
			order = append(order, e.Turn)
		case store.DeskTurnEnd:
			delete(open, e.Turn)
		}
	}
	for _, id := range order {
		if origin, ok := open[id]; ok {
			if _, err := d.append(ctx, store.DeskEvent{Kind: store.DeskTurnEnd, Turn: id, Origin: origin, Text: "interrupted: the daemon stopped"}); err != nil {
				return err
			}
		}
	}
	return nil
}
