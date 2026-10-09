package api

import "github.com/ubixsys/ubixshepherd/internal/store"

// PathFeedStream is GET /v1/feed/stream: the feed as server-sent events, so clients need
// not poll GET /v1/feed. Each item is sent as
//
//	id: <item id>
//	event: <its Event* kind>
//	data: FeedEvent as JSON
//
// Resume with ?after=ID or the Last-Event-ID header; with neither (or after=latest) it
// starts after the newest item. A comment line (": ping") is sent every 15 seconds.
// The operator and the front desk may follow it.
const PathFeedStream = "/v1/feed/stream"

// FeedEvent is one item on the feed stream, with its event kind.
type FeedEvent struct {
	store.FeedItem
	Event string `json:"event"`
}
