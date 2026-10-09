package watch

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

// An unreachable forge host is skipped for a wait that doubles from a minute to 15,
// once per host however many lanes it has, and is logged once going down and once
// coming back. An answer that refuses (a 4xx) is not a host down.
func TestUnreachableHostBacksOff(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.CreateLane(f.ctx, store.Lane{RepoID: f.lane.RepoID, Name: "feat/y", Branch: "feat/y", Base: "main",
		Scope: []string{"y/**"}, Worktree: f.lane.Worktree + "-y", State: store.LaneOpen}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	f.w.Log = slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	f.w.Clock = func() time.Time { return now }
	check := func() int {
		t.Helper()
		f.f.calls = 0
		f.w.Check(f.ctx)
		return f.f.calls
	}

	f.f.err = errors.New("glab api --hostname gl.example.com x: dial tcp: lookup gl.example.com: no such host")
	if n := check(); n != 1 {
		t.Errorf("first failure: %d calls, want 1 (the host, not each lane)", n)
	}
	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		now = now.Add(wait - time.Second)
		if n := check(); n != 0 {
			t.Errorf("step %d: host asked %s into a %s wait", i, wait-time.Second, wait)
		}
		now = now.Add(time.Second)
		if n := check(); n != 1 {
			t.Errorf("step %d: %d calls after the %s wait, want 1", i, n, wait)
		}
	}
	if c := strings.Count(logs.String(), "unreachable"); c != 1 {
		t.Errorf("down logged %d times:\n%s", c, logs.String())
	}

	f.f.err = nil
	now = now.Add(15 * time.Minute)
	if n := check(); n != 2 {
		t.Errorf("back: %d calls, want both lanes", n)
	}
	if !strings.Contains(logs.String(), "forge host is back") {
		t.Errorf("recovery not logged:\n%s", logs.String())
	}
	// The wait starts again from a minute.
	f.f.err = errors.New("502 Bad Gateway (HTTP 502)")
	check()
	now = now.Add(time.Minute)
	if n := check(); n != 1 {
		t.Errorf("after a reset the wait is a minute again: %d calls", n)
	}

	// A refusal is an answer: every lane is asked on every poll, and nothing backs off.
	f.f.err = nil
	now = now.Add(time.Hour)
	check()
	f.f.err = errors.New("glab api x: 404 Not Found (HTTP 404)")
	for range 3 {
		if n := check(); n != 2 {
			t.Errorf("a 404 backed off: %d calls", n)
		}
	}
}
