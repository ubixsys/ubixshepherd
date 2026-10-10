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
	"strings"
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
	// redial, when set, finds the daemon again by other means than the runtime file: the
	// way a remote client follows a restarted daemon (new tunnel, new port, new token).
	redial func() (base, token string, err error)
	// host names the machine the daemon is on when it is not this one, for messages.
	host string
	// workspace and repo stand in for the current directory (see RemoteCwd).
	workspace, repo string
}

// RemoteCwd is the "current directory" of a command that has none on the daemon's
// machine: a remote one, or one given a workspace by name. Resolve turns it into the
// workspace (and repo) the client was told to act on.
const RemoteCwd = "(remote)"

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

// NewRemote returns a client for a daemon reached by a transport that can be set up
// again: redial returns the base URL and token to use now (opening a new tunnel, say),
// and is what Redial calls. host labels the machine in error messages.
func NewRemote(host, base, token string, redial func() (base, token string, err error)) *Client {
	c := New(base, token)
	c.host, c.redial = host, redial
	return c
}

// SetScope names the workspace, and optionally the repo, that stand for the current
// directory when a command has none (see RemoteCwd).
func (c *Client) SetScope(workspace, repo string) {
	c.mu.Lock()
	c.workspace, c.repo = workspace, repo
	c.mu.Unlock()
}

// Host is the machine a remote client talks to, empty for a local one.
func (c *Client) Host() string {
	if c == nil {
		return ""
	}
	return c.host
}

// Redial finds the daemon again and points the client at it. The daemon picks a new
// port and token on each start; chat calls this after a connection failure. A local
// client re-reads the runtime file; a remote one re-resolves and re-tunnels.
func (c *Client) Redial() error {
	if c != nil && c.redial != nil {
		base, token, err := c.redial()
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.base, c.token = base, token
		c.mu.Unlock()
		return nil
	}
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
		Name: c.Name, base: c.base, token: c.token, runtime: c.runtime, host: c.host,
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

// LaneHistory lists lanes with their runs' count and cost and how closed ones ended.
// state is "closed" (the default), "open" or "all"; a zero since does not filter.
func (c *Client) LaneHistory(ctx context.Context, workspaceID int64, state string, since time.Time) ([]api.LaneRecord, error) {
	var out []api.LaneRecord
	v := url.Values{"workspace_id": {fmt.Sprint(workspaceID)}}
	if state != "" {
		v.Set("state", state)
	}
	if !since.IsZero() {
		v.Set("since", since.UTC().Format(time.RFC3339))
	}
	return out, c.do(ctx, http.MethodGet, api.PathHistoryLanes+"?"+v.Encode(), nil, &out)
}

// RunHistoryQuery filters Client.RunHistory; zero fields do not filter.
type RunHistoryQuery struct {
	WorkspaceID int64
	RepoID      int64
	LaneID      int64
	Agent       string
	State       string
	Since       time.Time
	Limit       int
}

// RunHistory lists runs newest first with the total cost of every run that matches.
func (c *Client) RunHistory(ctx context.Context, q RunHistoryQuery) (api.RunHistory, error) {
	var out api.RunHistory
	v := url.Values{"workspace_id": {fmt.Sprint(q.WorkspaceID)}}
	for k, n := range map[string]int64{"repo_id": q.RepoID, "lane_id": q.LaneID, "limit": int64(q.Limit)} {
		if n != 0 {
			v.Set(k, fmt.Sprint(n))
		}
	}
	for k, s := range map[string]string{"agent": q.Agent, "state": q.State} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	return out, c.do(ctx, http.MethodGet, api.PathHistoryRuns+"?"+v.Encode(), nil, &out)
}

func (c *Client) Run(ctx context.Context, id int64) (api.RunView, error) {
	var out api.RunView
	return out, c.do(ctx, http.MethodGet, api.PathRun(id), nil, &out)
}

func (c *Client) RunLog(ctx context.Context, id, offset int64) (api.RunLog, error) {
	var out api.RunLog
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("%s?offset=%d", api.PathRunLog(id), offset), nil, &out)
}

// LaneConversation reads a lane's conversation: items after the cursor (0 for all), at
// most limit of them (0 for the daemon's default).
func (c *Client) LaneConversation(ctx context.Context, laneID int64, after, limit int) (convo.Thread, error) {
	var out convo.Thread
	q := fmt.Sprintf("%s?after=%d&limit=%d", api.PathLaneConversation(laneID), after, limit)
	return out, c.do(ctx, http.MethodGet, q, nil, &out)
}

// LaneConversationItem reads one item of a lane's conversation with its full text.
func (c *Client) LaneConversationItem(ctx context.Context, laneID int64, seq int) (convo.Item, error) {
	var th convo.Thread
	q := fmt.Sprintf("%s?expand=%d", api.PathLaneConversation(laneID), seq)
	if err := c.do(ctx, http.MethodGet, q, nil, &th); err != nil {
		return convo.Item{}, err
	}
	if len(th.Items) == 0 {
		return convo.Item{}, fmt.Errorf("item %d is not in lane %d's conversation", seq, laneID)
	}
	return th.Items[0], nil
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

// SettingInfo reads a setting with the configured value it overrides, if any.
func (c *Client) SettingInfo(ctx context.Context, key string) (api.Setting, error) {
	var out api.Setting
	return out, c.do(ctx, http.MethodGet, api.PathSettings+"/"+key, nil, &out)
}

func (c *Client) SetSetting(ctx context.Context, key, value string) error {
	var out api.Setting
	return c.do(ctx, http.MethodPut, api.PathSettings+"/"+key, api.Setting{Value: value}, &out)
}

func (c *Client) SpendToday(ctx context.Context) (api.SpendToday, error) {
	var out api.SpendToday
	return out, c.do(ctx, http.MethodGet, api.PathSpend, nil, &out)
}

// Usage returns the tokens and context sizes of the days from to to inclusive
// ("2006-01-02"); "" for either is today.
func (c *Client) Usage(ctx context.Context, from, to string) (store.UsageStats, error) {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	var out store.UsageStats
	err := c.do(ctx, http.MethodGet, api.PathUsage+"?"+q.Encode(), nil, &out)
	return out, err
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
	if path == RemoteCwd {
		return c.resolveScope(ctx)
	}
	return out, c.do(ctx, http.MethodGet, api.PathResolve+"?path="+url.QueryEscape(path), nil, &out)
}

// resolveScope answers for RemoteCwd: the workspace and repo the client was told to act
// on, or nothing when it was told none.
func (c *Client) resolveScope(ctx context.Context) (api.Resolution, error) {
	c.mu.RLock()
	wsName, repoName := c.workspace, c.repo
	c.mu.RUnlock()
	if wsName == "" {
		if repoName != "" {
			return api.Resolution{Path: RemoteCwd}, errors.New("a repo needs its workspace: add --workspace NAME")
		}
		return api.Resolution{Path: RemoteCwd}, nil
	}
	all, err := c.Workspaces(ctx)
	if err != nil {
		return api.Resolution{}, err
	}
	var names []string
	for _, w := range all {
		names = append(names, w.Name)
		if w.Name != wsName {
			continue
		}
		dir := w.Path
		if repoName != "" {
			dir = ""
			var repos []string
			for _, r := range w.Repos {
				repos = append(repos, r.Name)
				if r.Name == repoName {
					dir = r.Path
				}
			}
			if dir == "" {
				return api.Resolution{}, fmt.Errorf("no repo %q in workspace %s (have: %s)", repoName, wsName, strings.Join(repos, ", "))
			}
		}
		var res api.Resolution
		return res, c.do(ctx, http.MethodGet, api.PathResolve+"?path="+url.QueryEscape(dir), nil, &res)
	}
	return api.Resolution{}, fmt.Errorf("no workspace %q on this daemon (have: %s)", wsName, strings.Join(names, ", "))
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
		if c.host != "" {
			return &unreachable{host: c.host, err: err}
		}
		// A stale runtime file from a daemon that died without cleaning up.
		return fmt.Errorf("%w (%v)", ErrNoDaemon, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return &refused{host: c.host}
	}
	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return fmt.Errorf("daemon: %s", e.Error)
		}
		return fmt.Errorf("daemon: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// unreachable is a remote daemon that did not answer through its tunnel. It matches
// ErrNoDaemon so a caller that reconnects on that (the chat) does so over ssh as well.
type unreachable struct {
	host string
	err  error
}

func (e *unreachable) Error() string {
	return fmt.Sprintf("cannot reach the daemon on %s through the ssh tunnel (%v): is it running? "+
		"a daemon that restarted is picked up by running the command again", e.host, e.err)
}
func (e *unreachable) Is(target error) bool { return target == ErrNoDaemon }
func (e *unreachable) Unwrap() error        { return e.err }

// refused is a daemon that did not accept the token, which usually means it restarted
// and wrote a new one. It also matches ErrNoDaemon: reading the new token is a redial.
type refused struct{ host string }

func (e *refused) Error() string {
	where := "the daemon"
	if e.host != "" {
		where = "the daemon on " + e.host
	}
	return where + " refused the access token (it probably restarted; a redial reads the new one)"
}
func (e *refused) Is(target error) bool { return target == ErrNoDaemon }

// ProjectBrief reads a project's approved brief with its age and stale flag, and its
// open draft.
func (c *Client) ProjectBrief(ctx context.Context, project string) (api.BriefView, error) {
	var out api.BriefView
	return out, c.do(ctx, http.MethodGet, api.PathProjectBrief(project), nil, &out)
}

// DraftBrief records a draft brief for a project. Only the person's own token or the
// front desk may; a draft is never given to an agent until the person approves it.
func (c *Client) DraftBrief(ctx context.Context, project, text string) (store.ProjectBrief, error) {
	var out store.ProjectBrief
	return out, c.do(ctx, http.MethodPost, api.PathProjectBriefDraft(project), api.DraftBrief{Text: text}, &out)
}

// ApproveBrief approves a project's open draft with the person's words. The daemon
// refuses it from an agent's token.
func (c *Client) ApproveBrief(ctx context.Context, project string, id int64, words string) (store.ProjectBrief, error) {
	var out store.ProjectBrief
	return out, c.do(ctx, http.MethodPost, api.PathProjectBriefApprove(project), api.ApproveBrief{ID: id, Words: words}, &out)
}
