package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// The daemon's front desk (api/desk.go). Only the operator token may call these: a
// worker's or the desk's own token is refused. workspaceID 0 means the daemon's only
// workspace; with more than one the daemon asks for it.

// deskQuery is the query string for a desk call, "" when it has no parameters.
func deskQuery(workspaceID int64, more url.Values) string {
	q := url.Values{}
	for k, v := range more {
		q[k] = v
	}
	if workspaceID != 0 {
		q.Set("workspace_id", strconv.FormatInt(workspaceID, 10))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// DeskTurn sends the person's message to the front desk. The daemon accepts it at once
// and runs the turn in the background: follow FollowDesk for the reply. A daemon with
// DeskQueueMax turns waiting refuses with 429.
func (c *Client) DeskTurn(ctx context.Context, workspaceID int64, text string) (api.DeskTurnAccepted, error) {
	var out api.DeskTurnAccepted
	return out, c.do(ctx, http.MethodPost, api.PathDeskTurn, api.DeskTurn{WorkspaceID: workspaceID, Text: text}, &out)
}

// DeskHistory reads the conversation oldest first: the newest limit events before the
// seq given (0 for the newest). When More is set, ask again with before set to the
// first event's Seq. limit 0 takes the daemon's default.
func (c *Client) DeskHistory(ctx context.Context, workspaceID, before int64, limit int) (api.DeskHistory, error) {
	more := url.Values{}
	if before > 0 {
		more.Set("before", strconv.FormatInt(before, 10))
	}
	if limit > 0 {
		more.Set("limit", strconv.Itoa(limit))
	}
	var out api.DeskHistory
	return out, c.do(ctx, http.MethodGet, api.PathDeskHistory+deskQuery(workspaceID, more), nil, &out)
}

// DeskStatus says whether the desk is busy, how many turns wait and who follows it.
func (c *Client) DeskStatus(ctx context.Context, workspaceID int64) (api.DeskStatus, error) {
	var out api.DeskStatus
	return out, c.do(ctx, http.MethodGet, api.PathDeskStatus+deskQuery(workspaceID, nil), nil, &out)
}

// DeskInterrupt stops the turn in progress and reports whether there was one.
func (c *Client) DeskInterrupt(ctx context.Context, workspaceID int64) (bool, error) {
	var out api.DeskInterrupted
	err := c.do(ctx, http.MethodPost, api.PathDeskInterrupt, api.DeskWorkspace{WorkspaceID: workspaceID}, &out)
	return out.Interrupted, err
}

// DeskNew ends the conversation's session: the next turn starts a new one.
func (c *Client) DeskNew(ctx context.Context, workspaceID int64) error {
	var out api.DeskWorkspace
	return c.do(ctx, http.MethodPost, api.PathDeskNew, api.DeskWorkspace{WorkspaceID: workspaceID}, &out)
}

// DeskItem is one thing on the desk's stream: a stored event, or a Partial piece of a
// reply that is never stored (the whole reply follows as an assistant event).
type DeskItem struct {
	store.DeskEvent
	// Partial marks a piece of a reply as it streams. It has no Seq of its own.
	Partial bool
}

// FollowDesk calls fn for every desk event after the seq given, then for each new one,
// until ctx ends, fn returns an error or the stream fails in a way retrying cannot fix
// (the token may not follow the desk, or the workspace is unknown). after < 0 starts
// after the newest event: read the past with DeskHistory. Between two calls to fn the
// stream may drop; it is reopened from the last stored event, so none is missed or
// repeated (see FollowOptions).
func (c *Client) FollowDesk(ctx context.Context, workspaceID, after int64, opt FollowOptions, fn func(DeskItem) error) error {
	if after < 0 {
		h, err := c.DeskHistory(ctx, workspaceID, 0, 1)
		if err != nil {
			return err
		}
		after = 0
		if n := len(h.Events); n > 0 {
			after = h.Events[n-1].Seq
		}
	}
	path := func(after int64) string {
		return api.PathDeskStream + deskQuery(workspaceID, url.Values{"after": {strconv.FormatInt(after, 10)}})
	}
	return c.follow(ctx, path, after, opt, func(id int64, event string, data []byte) (int64, error) {
		var e store.DeskEvent
		if err := decodeEvent(data, &e); err != nil {
			return 0, fmt.Errorf("desk stream: %w", err)
		}
		if event == api.DeskPartial {
			return 0, fn(DeskItem{DeskEvent: e, Partial: true})
		}
		if id == 0 {
			id = e.Seq
		}
		return id, fn(DeskItem{DeskEvent: e})
	})
}

// FollowFeed calls fn for every feed item after the id given, then for each new one,
// with the same reconnect behavior as FollowDesk. after < 0 starts after the newest.
func (c *Client) FollowFeed(ctx context.Context, after int64, opt FollowOptions, fn func(api.FeedEvent) error) error {
	if after < 0 {
		f, err := c.Feed(ctx, -1)
		if err != nil {
			return err
		}
		after = f.Last
	}
	path := func(after int64) string {
		return api.PathFeedStream + "?after=" + strconv.FormatInt(after, 10)
	}
	return c.follow(ctx, path, after, opt, func(id int64, event string, data []byte) (int64, error) {
		var e api.FeedEvent
		if err := decodeEvent(data, &e); err != nil {
			return 0, fmt.Errorf("feed stream: %w", err)
		}
		if e.Event == "" {
			e.Event = event
		}
		if id == 0 {
			id = e.ID
		}
		return id, fn(e)
	})
}
