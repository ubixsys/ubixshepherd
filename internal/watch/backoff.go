package watch

import (
	"time"

	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// A forge host that cannot be reached is skipped for a while, so a laptop offline or a
// forge down does not fail every lane on every poll. The wait doubles from
// MinBackoff to MaxBackoff and resets on the first success.
const (
	MinBackoff = time.Minute
	MaxBackoff = 15 * time.Minute
)

// hostState is a forge host that did not answer.
type hostState struct {
	since time.Time     // when it went down
	until time.Time     // skipped until then
	wait  time.Duration // the current wait
}

func (w *Watcher) now() time.Time {
	if w.Clock != nil {
		return w.Clock()
	}
	return time.Now()
}

// hostOf is the forge host a repo's remote names, "" when it names none.
func hostOf(repo store.Repo) string {
	r, err := forge.ParseRemote(repo.Remote)
	if err != nil {
		return ""
	}
	return r.Host
}

// skipHost says whether a host is in its wait after failing to answer.
func (w *Watcher) skipHost(host string) bool {
	h := w.down[host]
	return h != nil && w.now().Before(h.until)
}

// result records how a call to a host went: a failure to reach it starts or doubles its
// wait, logged once when it goes down; a success (or an answer, even a refusal) ends
// the wait, logged once when it comes back. It reports whether the host was reached.
func (w *Watcher) result(host string, err error) bool {
	if host == "" {
		return true
	}
	if !forge.Unreachable(err) {
		if h := w.down[host]; h != nil {
			delete(w.down, host)
			w.Log.Info("watch: forge host is back", "host", host, "down_for", w.now().Sub(h.since).Round(time.Second))
		}
		return true
	}
	if w.down == nil {
		w.down = map[string]*hostState{}
	}
	h := w.down[host]
	if h == nil {
		h = &hostState{since: w.now(), wait: MinBackoff}
		w.down[host] = h
		w.Log.Warn("watch: forge host unreachable; skipping it, backing off up to 15m", "host", host, "err", err)
	} else {
		h.wait = min(2*h.wait, MaxBackoff)
	}
	h.until = w.now().Add(h.wait)
	return false
}
