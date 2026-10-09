// Package client talks to a running daemon over the HTTP API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/daemon"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// ErrNoDaemon means no daemon is running for this Shepherd home.
var ErrNoDaemon = errors.New("the shepherd daemon is not running (start it with: shepherd daemon start)")

// Client calls one daemon.
type Client struct {
	// Name says who is calling (cli, mcp, hook), for the daemon's log.
	Name  string
	mu    sync.RWMutex
	base  string
	token string
	http  *http.Client
	// runtime is the daemon.json path FromRuntime remembered, so Redial can pick up a
	// new address after the daemon restarts.
	runtime string
}

// New returns a client for the daemon at base (for example http://127.0.0.1:7400).
func New(base, token string) *Client {
	// Lane calls fetch from remotes, so the ceiling is generous; callers that only probe
	// pass a context with a short deadline.
	return &Client{base: base, token: token, http: &http.Client{Timeout: 5 * time.Minute}}
}

// FromRuntime returns a client for the daemon described by the runtime file at path.
func FromRuntime(path string) (*Client, error) {
	rt, err := daemon.ReadRuntime(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDaemon
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c := New("http://"+rt.Addr, rt.Token)
	c.runtime = path
	return c, nil
}

// Redial re-reads the runtime file and points the client at the address there. The
// daemon picks a new port on each start; chat uses this after a connection failure.
func (c *Client) Redial() error {
	if c == nil || c.runtime == "" {
		return ErrNoDaemon
	}
	rt, err := daemon.ReadRuntime(c.runtime)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoDaemon
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", c.runtime, err)
	}
	c.mu.Lock()
	c.base = "http://" + rt.Addr
	c.token = rt.Token
	c.mu.Unlock()
	return nil
}

// Addr is the daemon base URL the client is currently using (for tests and status).
func (c *Client) Addr() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.base
}

func (c *Client) Status(ctx context.Context) (api.Status, error) {
	var out api.Status
	return out, c.do(ctx, http.MethodGet, api.PathStatus, nil, &out)
}

// Shutdown asks the daemon to stop.
func (c *Client) Shutdown(ctx context.Context) error {
	var out struct{}
	return c.do(ctx, http.MethodPost, api.PathShutdown, nil, &out)
}

func (c *Client) Lanes(ctx context.Context, workspaceID, repoID int64) ([]api.LaneView, error) {
	var out []api.LaneView
	q := fmt.Sprintf("%s?workspace_id=%d", api.PathLanes, workspaceID)
	if repoID != 0 {
		q += fmt.Sprintf("&repo_id=%d", repoID)
	}
	return out, c.do(ctx, http.MethodGet, q, nil, &out)
}

func (c *Client) OpenLane(ctx context.Context, req fold.OpenRequest) (fold.Opened, error) {
	var out fold.Opened
	return out, c.do(ctx, http.MethodPost, api.PathLanes, req, &out)
}

func (c *Client) RescopeLane(ctx context.Context, id int64, add, remove []string) (store.Lane, error) {
	var out store.Lane
	return out, c.do(ctx, http.MethodPost, api.PathLaneScope(id), api.Rescope{Add: add, Remove: remove}, &out)
}

func (c *Client) CloseLane(ctx context.Context, id int64, force bool) (fold.CloseResult, error) {
	var out fold.CloseResult
	return out, c.do(ctx, http.MethodPost, api.PathLaneClose(id), api.CloseLane{Force: force}, &out)
}

// ShipLane pushes a lane's committed work after the daemon runs the repo's gate, so it
// waits as long as a gate may take.
func (c *Client) ShipLane(ctx context.Context, id int64) (dispatch.Shipped, error) {
	var out dispatch.Shipped
	c.mu.RLock()
	long := &Client{
		Name: c.Name, base: c.base, token: c.token, runtime: c.runtime,
		http: &http.Client{Timeout: dispatch.GateTimeout + 5*time.Minute},
	}
	c.mu.RUnlock()
	return out, long.do(ctx, http.MethodPost, api.PathLaneShip(id), struct{}{}, &out)
}

func (c *Client) FoldGC(ctx context.Context, workspaceID int64) ([]fold.Stale, error) {
	var out []fold.Stale
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("%s?workspace_id=%d", api.PathFoldGC, workspaceID), nil, &out)
}

func (c *Client) PrePush(ctx context.Context, req api.PrePush) (fold.Verdict, error) {
	var out fold.Verdict
	return out, c.do(ctx, http.MethodPost, api.PathPrePush, req, &out)
}

func (c *Client) RepoHook(ctx context.Context, repoID int64, action string) (fold.HookState, error) {
	var out fold.HookState
	return out, c.do(ctx, http.MethodPost, api.PathRepoHook(repoID), api.RepoHook{Action: action}, &out)
}

func (c *Client) StartRun(ctx context.Context, req dispatch.StartRequest) (api.RunView, error) {
	var out api.RunView
	return out, c.do(ctx, http.MethodPost, api.PathRuns, req, &out)
}

func (c *Client) Runs(ctx context.Context, laneID int64, state string, limit int) ([]api.RunView, error) {
	var out []api.RunView
	q := fmt.Sprintf("%s?lane_id=%d&state=%s&limit=%d", api.PathRuns, laneID, url.QueryEscape(state), limit)
	return out, c.do(ctx, http.MethodGet, q, nil, &out)
}

func (c *Client) Run(ctx context.Context, id int64) (api.RunView, error) {
	var out api.RunView
	return out, c.do(ctx, http.MethodGet, api.PathRun(id), nil, &out)
}

func (c *Client) RunLog(ctx context.Context, id, offset int64) (api.RunLog, error) {
	var out api.RunLog
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("%s?offset=%d", api.PathRunLog(id), offset), nil, &out)
}

func (c *Client) AddEvent(ctx context.Context, runID int64, e store.Event) (store.Event, error) {
	var out store.Event
	return out, c.do(ctx, http.MethodPost, api.PathRunEvents(runID), e, &out)
}

func (c *Client) RunEvents(ctx context.Context, runID int64) (api.RunEvents, error) {
	var out api.RunEvents
	return out, c.do(ctx, http.MethodGet, api.PathRunEvents(runID), nil, &out)
}

func (c *Client) Ask(ctx context.Context, runID int64, d store.Decision) (store.Decision, error) {
	var out store.Decision
	return out, c.do(ctx, http.MethodPost, api.PathRunDecisions(runID), d, &out)
}

func (c *Client) Decisions(ctx context.Context, state string) ([]api.DecisionView, error) {
	var out []api.DecisionView
	return out, c.do(ctx, http.MethodGet, api.PathDecisions+"?state="+url.QueryEscape(state), nil, &out)
}

func (c *Client) Answer(ctx context.Context, id int64, answer string) (store.Decision, error) {
	var out store.Decision
	return out, c.do(ctx, http.MethodPost, api.PathDecisionAnswer(id), api.Answer{Answer: answer}, &out)
}

func (c *Client) RequestHelp(ctx context.Context, runID int64, q store.Request) (store.Request, error) {
	var out store.Request
	return out, c.do(ctx, http.MethodPost, api.PathRunRequests(runID), q, &out)
}

func (c *Client) Requests(ctx context.Context, states string) ([]api.RequestView, error) {
	var out []api.RequestView
	return out, c.do(ctx, http.MethodGet, api.PathRequests+"?state="+url.QueryEscape(states), nil, &out)
}

func (c *Client) CloseRequest(ctx context.Context, id int64, why string) (store.Request, error) {
	var out store.Request
	return out, c.do(ctx, http.MethodPost, api.PathRequestClose(id), api.CloseRequest{Why: why}, &out)
}

func (c *Client) RouteRequest(ctx context.Context, id int64, lane, agent string) (store.Request, error) {
	var out store.Request
	return out, c.do(ctx, http.MethodPost, api.PathRequestRoute(id), api.Route{Lane: lane, Agent: agent}, &out)
}

// Feed returns feed items after an id; after < 0 asks only for the newest id.
func (c *Client) Feed(ctx context.Context, after int64) (api.Feed, error) {
	var out api.Feed
	q := fmt.Sprintf("%s?after=%d", api.PathFeed, after)
	if after < 0 {
		q = api.PathFeed + "?after=latest"
	}
	err := c.do(ctx, http.MethodGet, q, nil, &out)
	if len(out.Events) != len(out.Items) {
		// An older daemon sent no events: map the kinds here, so callers can rely on them.
		out.Events = make([]string, len(out.Items))
		for i, it := range out.Items {
			out.Events[i] = api.EventKind(it.Kind)
		}
	}
	return out, err
}

func (c *Client) Setting(ctx context.Context, key string) (string, error) {
	var out api.Setting
	return out.Value, c.do(ctx, http.MethodGet, api.PathSettings+"/"+key, nil, &out)
}

func (c *Client) SetSetting(ctx context.Context, key, value string) error {
	var out api.Setting
	return c.do(ctx, http.MethodPut, api.PathSettings+"/"+key, api.Setting{Value: value}, &out)
}

func (c *Client) SpendToday(ctx context.Context) (api.SpendToday, error) {
	var out api.SpendToday
	return out, c.do(ctx, http.MethodGet, api.PathSpend, nil, &out)
}

func (c *Client) AddSpend(ctx context.Context, sp store.Spend) error {
	var out store.Spend
	return c.do(ctx, http.MethodPost, api.PathSpend, sp, &out)
}

func (c *Client) FoldImport(ctx context.Context, req api.FoldImport) (fold.ImportPlan, error) {
	var out fold.ImportPlan
	return out, c.do(ctx, http.MethodPost, api.PathFoldImport, req, &out)
}

func (c *Client) FoldReview(ctx context.Context, repoID int64) ([]fold.Review, error) {
	var out []fold.Review
	return out, c.do(ctx, http.MethodPost, api.PathFoldReview, api.FoldImport{RepoID: repoID}, &out)
}

func (c *Client) FoldRetire(ctx context.Context, repoID int64, worktree string) (fold.Review, error) {
	var out fold.Review
	return out, c.do(ctx, http.MethodPost, api.PathFoldRetire, api.FoldRetire{RepoID: repoID, Worktree: worktree}, &out)
}

func (c *Client) FoldView(ctx context.Context, req api.FoldView) (api.FoldView, error) {
	var out api.FoldView
	return out, c.do(ctx, http.MethodPost, api.PathFoldView, req, &out)
}

func (c *Client) ImportSessions(ctx context.Context, repoID int64) ([]api.SessionView, error) {
	var out []api.SessionView
	return out, c.do(ctx, http.MethodPost, api.PathSessionsImport, api.SessionImport{RepoID: repoID}, &out)
}

func (c *Client) Sessions(ctx context.Context, repoID int64) ([]api.SessionView, error) {
	var out []api.SessionView
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("%s?repo_id=%d", api.PathSessions, repoID), nil, &out)
}

func (c *Client) AskSession(ctx context.Context, id, question string) (convo.Answer, error) {
	var out convo.Answer
	return out, c.do(ctx, http.MethodPost, api.PathSessionAsk(id), api.Ask{Question: question}, &out)
}

func (c *Client) ReserveTag(ctx context.Context, req api.Reserve) (store.Reservation, error) {
	var out store.Reservation
	return out, c.do(ctx, http.MethodPost, api.PathTagsReserve, req, &out)
}

func (c *Client) Tags(ctx context.Context, repoID int64) ([]api.ReservationView, error) {
	var out []api.ReservationView
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("%s?repo_id=%d", api.PathTags, repoID), nil, &out)
}

func (c *Client) ReleaseTag(ctx context.Context, repoID int64, tag string) error {
	var out api.ReleaseTag
	return c.do(ctx, http.MethodPost, api.PathTagsRelease, api.ReleaseTag{RepoID: repoID, Tag: tag}, &out)
}

func (c *Client) StopRun(ctx context.Context, id int64) error {
	var out struct{}
	return c.do(ctx, http.MethodPost, api.PathRunStop(id), nil, &out)
}

func (c *Client) Workspaces(ctx context.Context) ([]api.WorkspaceDetail, error) {
	var out []api.WorkspaceDetail
	return out, c.do(ctx, http.MethodGet, api.PathWorkspaces, nil, &out)
}

func (c *Client) SaveWorkspace(ctx context.Context, req api.SaveWorkspace) (api.WorkspaceDetail, error) {
	var out api.WorkspaceDetail
	return out, c.do(ctx, http.MethodPost, api.PathWorkspaces, req, &out)
}

func (c *Client) Resolve(ctx context.Context, path string) (api.Resolution, error) {
	var out api.Resolution
	return out, c.do(ctx, http.MethodGet, api.PathResolve+"?path="+url.QueryEscape(path), nil, &out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	c.mu.RLock()
	base, token := c.base, c.token
	c.mu.RUnlock()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.Name != "" {
		req.Header.Set(api.ClientHeader, c.Name)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A stale runtime file from a daemon that died without cleaning up.
		return fmt.Errorf("%w (%v)", ErrNoDaemon, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return fmt.Errorf("daemon: %s", e.Error)
		}
		return fmt.Errorf("daemon: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
