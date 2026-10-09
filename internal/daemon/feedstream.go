package daemon

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
)

// feedStream follows the feed as server-sent events (see api.PathFeedStream), so a
// client need not poll GET /v1/feed.
func (s *Server) feedStream(w http.ResponseWriter, r *http.Request) {
	if !s.feedStreams.take() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("%d clients already follow the feed", cap(s.feedStreams.n)))
		return
	}
	defer s.feedStreams.give()
	ctx := r.Context()
	after, ok := streamStart(w, r)
	if !ok {
		return
	}
	if after < 0 {
		last, err := s.Store.LastFeed(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		after = last
	}
	sse := newSSE(w)
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	for {
		changed := s.hub.Changed()
		items, err := s.Store.Feed(ctx, after, 200)
		if err != nil {
			s.Log.Error("feed stream", "err", err)
			return
		}
		for _, it := range items {
			ev := api.EventKind(it.Kind)
			if sse.event(it.ID, ev, api.FeedEvent{FeedItem: it, Event: ev}) != nil {
				return
			}
			after = it.ID
		}
		if len(items) == 200 {
			continue
		}
		if sse.flush() != nil {
			return
		}
		select {
		case <-changed:
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
