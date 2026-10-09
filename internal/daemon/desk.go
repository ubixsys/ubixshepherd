package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/desk"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// heartbeat is how often a stream with nothing to say sends a comment, so proxies and
// SSH port forwards keep it open and a client sees the daemon is there.
const heartbeat = 15 * time.Second

// Stream limits: each subscriber holds a connection and a goroutine.
const (
	maxDeskStreams = 16
	maxFeedStreams = 32
)

// background starts what the daemon runs besides answering requests, once: the feed
// watcher and the front desk. Close stops them.
func (s *Server) background() {
	s.bgOnce.Do(func() {
		s.quit = make(chan struct{})
		agent := s.DeskAgent
		if agent == nil {
			exe, _ := os.Executable()
			agent = desk.Claude{Shepherd: exe}
		}
		st := s.Store
		s.desks = desk.NewManager(desk.Options{
			Store: st, Agent: agent, Tokens: s, Log: s.Log,
			Spend: func(ctx context.Context, sp store.Spend) error {
				if s.Runner != nil {
					return s.Runner.Spend(ctx, sp)
				}
				return st.AddSpend(ctx, store.Spend{Day: dispatch.Today(), Source: sp.Source, USD: sp.USD})
			},
			Model: s.deskModel,
			Wake:  func() string { return s.LiveConfig().Desk.WakeMode() },
			Grace: s.DeskGrace,
		})
		s.hub = newFeedHub(s)
		s.deskStreams, s.feedStreams = newSlots(maxDeskStreams), newSlots(maxFeedStreams)
		s.bgDone = make(chan struct{})
		go func() { defer close(s.bgDone); s.hub.run(s.quit) }()
	})
}

// Close stops the front desk and the streams; the daemon's Run calls it on its way out.
func (s *Server) Close() {
	s.background()
	s.closeOnce.Do(func() {
		close(s.quit)
		<-s.bgDone
		s.desks.Close()
	})
}

// deskModel is the desk's model: the desk.model setting (shepherd chat's /model), else
// desk.model in config.yaml, else the agent's default.
func (s *Server) deskModel(ctx context.Context) string {
	if m, _ := s.Store.Setting(ctx, "desk.model"); m != "" {
		return m
	}
	return s.LiveConfig().Desk.Model
}

// deskFor finds the desk of the workspace a request names, or the only one, answering
// the request itself when it cannot.
func (s *Server) deskFor(w http.ResponseWriter, r *http.Request, wsID int64) (*desk.Desk, bool) {
	if wsID == 0 {
		wsID, _ = strconv.ParseInt(r.URL.Query().Get("workspace_id"), 10, 64)
	}
	wss, err := s.Store.Workspaces(r.Context())
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	var ws *store.Workspace
	for i := range wss {
		if wss[i].ID == wsID || (wsID == 0 && len(wss) == 1) {
			ws = &wss[i]
		}
	}
	switch {
	case ws == nil && wsID != 0:
		writeError(w, http.StatusNotFound, fmt.Errorf("no workspace %d", wsID))
		return nil, false
	case ws == nil && len(wss) == 0:
		writeError(w, http.StatusConflict, errors.New("no workspace yet: run shepherd init"))
		return nil, false
	case ws == nil:
		writeError(w, http.StatusBadRequest, fmt.Errorf("workspace_id is required: this daemon has %d workspaces", len(wss)))
		return nil, false
	}
	d, err := s.desks.For(*ws)
	if errors.Is(err, desk.ErrClosed) {
		writeError(w, http.StatusServiceUnavailable, err)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return d, true
}

func (s *Server) deskTurn(w http.ResponseWriter, r *http.Request) {
	var req api.DeskTurn
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, errors.New("say something"))
		return
	}
	d, ok := s.deskFor(w, r, req.WorkspaceID)
	if !ok {
		return
	}
	id, ahead, err := d.Send(r.Context(), req.Text)
	switch {
	case errors.Is(err, desk.ErrQueueFull):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, desk.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err)
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusAccepted, api.DeskTurnAccepted{WorkspaceID: d.Workspace().ID, Turn: id, Ahead: ahead})
	}
}

func (s *Server) deskHistory(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deskFor(w, r, 0)
	if !ok {
		return
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	es, err := s.Store.DeskHistory(r.Context(), d.Workspace().ID, before, limit+1)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := api.DeskHistory{Events: []store.DeskEvent{}}
	if len(es) > limit {
		out.More, es = true, es[1:]
	}
	if es != nil {
		out.Events = es
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deskStatus(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deskFor(w, r, 0)
	if !ok {
		return
	}
	busy, queued := d.Busy()
	writeJSON(w, http.StatusOK, api.DeskStatus{
		WorkspaceID: d.Workspace().ID, Busy: busy, Queued: queued, Attached: d.Attached(),
		Session: d.Session(r.Context()), Model: s.deskModel(r.Context()), Wake: s.LiveConfig().Desk.WakeMode(),
	})
}

func (s *Server) deskInterrupt(w http.ResponseWriter, r *http.Request) {
	var req api.DeskWorkspace
	if !decodeOptional(w, r, &req) {
		return
	}
	d, ok := s.deskFor(w, r, req.WorkspaceID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.DeskInterrupted{Interrupted: d.Interrupt()})
}

func (s *Server) deskNew(w http.ResponseWriter, r *http.Request) {
	var req api.DeskWorkspace
	if !decodeOptional(w, r, &req) {
		return
	}
	d, ok := s.deskFor(w, r, req.WorkspaceID)
	if !ok {
		return
	}
	if err := d.New(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// decodeOptional is decode for a body that may be empty.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return decode(w, r, v)
}

// deskStream follows the desk's conversation as server-sent events (see api.PathDeskStream).
func (s *Server) deskStream(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deskFor(w, r, 0)
	if !ok {
		return
	}
	if !s.deskStreams.take() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("%d clients already follow the desk", maxDeskStreams))
		return
	}
	defer s.deskStreams.give()
	ctx := r.Context()
	after, ok := streamStart(w, r)
	if !ok {
		return
	}
	if after < 0 {
		latest, err := s.Store.DeskHistory(ctx, d.Workspace().ID, 0, 1)
		if err != nil {
			s.fail(w, err)
			return
		}
		after = 0
		if len(latest) > 0 {
			after = latest[0].Seq
		}
	}
	sub := d.Subscribe()
	defer d.Unsubscribe(sub)
	sse := newSSE(w)
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	for {
		changed := d.Changed()
		es, err := s.Store.DeskEvents(ctx, d.Workspace().ID, after, 200)
		if err != nil {
			s.Log.Error("desk stream", "err", err)
			return
		}
		for _, e := range es {
			if sse.event(e.Seq, e.Kind, e) != nil {
				return
			}
			after = e.Seq
		}
		if len(es) == 200 {
			continue
		}
		if sse.flush() != nil {
			return
		}
		select {
		case <-changed:
		case e := <-sub.Live:
			if sse.event(0, api.DeskPartial, e) != nil || sse.flush() != nil {
				return
			}
		case <-beat.C:
			if sse.ping() != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		}
	}
}

// streamStart writes the stream's headers and says where to start: after the id in
// Last-Event-ID or ?after=, or -1 for after the newest.
func streamStart(w http.ResponseWriter, r *http.Request) (int64, bool) {
	from := r.Header.Get("Last-Event-ID")
	if from == "" {
		from = r.URL.Query().Get("after")
	}
	var after int64 = -1
	if from != "" && from != "latest" {
		n, err := strconv.ParseInt(from, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("after %q: want an id, or latest", from))
			return 0, false
		}
		after = n
	}
	return after, true
}

// sse writes server-sent events. Nothing in it depends on the Host the client used, so
// it works the same through an SSH port forward to the loopback API.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// A stream outlives the server's write timeout, if it has one.
	rc.SetWriteDeadline(time.Time{})
	return &sse{w: w, rc: rc}
}

// event writes one event; an id of 0 sends none, so a client's Last-Event-ID stays at
// the last stored event.
func (e *sse) event(id int64, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var buf strings.Builder
	if id != 0 {
		fmt.Fprintf(&buf, "id: %d\n", id)
	}
	fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", kind, b)
	_, err = e.w.Write([]byte(buf.String()))
	return err
}

func (e *sse) ping() error {
	if _, err := e.w.Write([]byte(": ping\n\n")); err != nil {
		return err
	}
	return e.flush()
}

func (e *sse) flush() error { return e.rc.Flush() }

// streamSlots counts a kind of stream against its limit.
type streamSlots struct {
	n chan struct{}
}

func newSlots(max int) streamSlots { return streamSlots{n: make(chan struct{}, max)} }

func (c *streamSlots) take() bool {
	select {
	case c.n <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *streamSlots) give() { <-c.n }
