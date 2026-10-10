package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

func sessionStore(t *testing.T) *sqlite.DB {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestWebSessionsSurviveARestart(t *testing.T) {
	st := sessionStore(t)
	now := time.Now()
	a := &webAuth{st: st, now: func() time.Time { return now }}
	val, ws, err := a.start("4321")
	if err != nil {
		t.Fatal(err)
	}
	gone, _, _ := a.start("4321")
	other, _, _ := a.start("4321")

	// The raw value is never stored.
	rows, err := st.LiveWebSessions(context.Background(), now)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
	for _, r := range rows {
		if r.Hash == val || r.Hash == gone || r.Hash == other {
			t.Fatal("the session value was stored")
		}
	}

	// A signed-out session is gone for the next instance.
	goneWS, _ := a.session(gone)
	if a.end(goneWS) != 1 {
		t.Fatal("sign-out ended nothing")
	}

	b := &webAuth{st: st, now: func() time.Time { return now.Add(time.Hour) }}
	got, ok := b.session(val)
	if !ok || got.csrf != ws.csrf || !got.expires.Equal(ws.expires.Truncate(time.Nanosecond)) {
		t.Fatalf("session after restart = %+v, %v", got, ok)
	}
	if _, ok := b.session(gone); ok {
		t.Error("a signed-out session came back")
	}
	if _, ok := b.session(other); !ok {
		t.Error("a live session was lost")
	}
	if p := b.lastPort(); p != "4321" {
		t.Errorf("lastPort = %q", p)
	}

	// Past its expiry a session is not accepted, and is pruned from the store.
	c := &webAuth{st: st, now: func() time.Time { return now.Add(webSessionTTL + time.Minute) }}
	if _, ok := c.session(val); ok {
		t.Error("an expired session was accepted")
	}
	if rows, _ := st.LiveWebSessions(context.Background(), now); len(rows) != 0 {
		t.Errorf("expired rows left: %d", len(rows))
	}
}

func TestWebSessionsEndAllAndCap(t *testing.T) {
	st := sessionStore(t)
	now := time.Now()
	tick := now
	clock := func() time.Time { tick = tick.Add(time.Second); return tick }
	a := &webAuth{st: st, now: clock}
	var vals []string
	for i := 0; i < maxWebSessions+3; i++ {
		v, _, err := a.start("1")
		if err != nil {
			t.Fatal(err)
		}
		vals = append(vals, v)
	}
	if rows, _ := st.LiveWebSessions(context.Background(), now); len(rows) != maxWebSessions {
		t.Fatalf("stored sessions = %d, want %d", len(rows), maxWebSessions)
	}
	b := &webAuth{st: st, now: clock}
	for i, v := range vals {
		_, ok := b.session(v)
		if want := i >= 3; ok != want {
			t.Errorf("session %d live = %v, want %v", i, ok, want)
		}
	}
	if n := b.end(nil); n != maxWebSessions {
		t.Errorf("ended %d", n)
	}
	c := &webAuth{st: st, now: clock}
	if _, ok := c.session(vals[len(vals)-1]); ok {
		t.Error("a session survived sign-out of all")
	}
}

func TestWebSessionLastSeenIsThrottled(t *testing.T) {
	st := sessionStore(t)
	now := time.Now().UTC()
	cur := now
	a := &webAuth{st: st, now: func() time.Time { return cur }}
	val, _, _ := a.start("1")
	seen := func() time.Time {
		rows, _ := st.LiveWebSessions(context.Background(), now)
		return rows[0].LastSeen
	}
	cur = now.Add(10 * time.Second)
	a.session(val)
	if !seen().Equal(now) {
		t.Errorf("last-seen written within the throttle: %v", seen())
	}
	cur = now.Add(webSeenEvery + time.Second)
	a.session(val)
	if !seen().Equal(cur) {
		t.Errorf("last-seen = %v, want %v", seen(), cur)
	}
}

// A second server over the same store accepts the first one's browser session.
func TestWebServerSessionsPersist(t *testing.T) {
	st := sessionStore(t)
	s1, err := NewServer(st, config.Default(), "/cfg", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	val, _, err := s1.web.start("1")
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := NewServer(st, config.Default(), "/cfg", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := s2.web.session(val); !ok {
		t.Error("the new server does not know the session")
	}
}
