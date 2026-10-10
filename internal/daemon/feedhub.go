package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

// feedPoll is how often the daemon looks for new feed items. Runs, the watcher and the
// fold write the feed straight to the store, so the daemon reads it back to wake the
// front desk and the feed's streams.
const feedPoll = 500 * time.Millisecond

// feedHub follows the feed: it wakes the front desk for the items that need it, and
// tells the feed's streams that there is more to read.
type feedHub struct {
	s       *Server
	st      store.Store
	mu      sync.Mutex
	changed chan struct{}
	last    int64
}

// newFeedHub starts after the newest item: what came before was the last daemon's to
// tell.
func newFeedHub(s *Server) *feedHub {
	h := &feedHub{s: s, st: s.Store, changed: make(chan struct{})}
	h.last, _ = h.st.LastFeed(context.Background())
	return h
}

// Changed is closed when new items arrive.
func (h *feedHub) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *feedHub) run(quit <-chan struct{}) {
	t := time.NewTicker(feedPoll)
	defer t.Stop()
	for {
		select {
		case <-quit:
			return
		case <-t.C:
			h.poll()
		}
	}
}

func (h *feedHub) poll() {
	for {
		items, err := h.st.Feed(context.Background(), h.last, 200)
		if err != nil {
			h.s.Log.Debug("read the feed", "err", err)
			return
		}
		if len(items) == 0 {
			return
		}
		h.last = items[len(items)-1].ID
		h.s.desks.Wake(items)
		h.mu.Lock()
		close(h.changed)
		h.changed = make(chan struct{})
		h.mu.Unlock()
		if len(items) < 200 {
			return
		}
	}
}
