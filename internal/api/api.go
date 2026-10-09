// Package api holds the HTTP API's request and response types, shared by the daemon and
// its clients. Every client goes through this API, the CLI on the same machine included.
package api

import (
	"fmt"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// ClientHeader names the kind of client making a request (cli, mcp, hook), for logs.
const ClientHeader = "X-Shepherd-Client"

// Routes.
const (
	PathStatus     = "/v1/status"
	PathWorkspaces = "/v1/workspaces"
	PathResolve    = "/v1/resolve"
	PathShutdown   = "/v1/shutdown"
	PathLanes      = "/v1/lanes"
	PathFoldGC     = "/v1/fold/gc"
	PathPrePush    = "/v1/hook/pre-push"
	PathRuns       = "/v1/runs"
)

// Fold import, view, review and retire routes (POST).
const (
	PathFoldImport = "/v1/fold/import"
	PathFoldView   = "/v1/fold/view"
	PathFoldReview = "/v1/fold/review"
	PathFoldRetire = "/v1/fold/retire"
)

// FoldRetire is the body of POST /v1/fold/retire.
type FoldRetire struct {
	RepoID   int64  `json:"repo_id"`
	Worktree string `json:"worktree"`
}

// FoldImport is the body of POST /v1/fold/import.
type FoldImport struct {
	RepoID int64  `json:"repo_id"`
	File   string `json:"file,omitempty"`
	Apply  bool   `json:"apply"`
	// Adopt names branches whose worktrees become lanes though no row claims them.
	Adopt []string `json:"adopt,omitempty"`
}

// FoldView is the body of POST /v1/fold/view, and its answer.
type FoldView struct {
	RepoID int64  `json:"repo_id"`
	Write  bool   `json:"write"`
	View   string `json:"view,omitempty"`
	File   string `json:"file,omitempty"`
}

// Conversation routes.
const (
	PathSessions       = "/v1/sessions"
	PathSessionsImport = "/v1/sessions/import"
)

// PathSessionAsk is POST /v1/sessions/{id}/ask.
func PathSessionAsk(id string) string { return PathSessions + "/" + id + "/ask" }

// SessionImport is the body of POST /v1/sessions/import; RepoID 0 means every repo.
type SessionImport struct {
	RepoID int64 `json:"repo_id,omitempty"`
}

// SessionView is an adopted conversation with its repo's name and whether it looks open.
type SessionView struct {
	store.Conversation
	Repo  string `json:"repo,omitempty"`
	InUse bool   `json:"in_use"`
}

// Ask is the body of POST /v1/sessions/{id}/ask.
type Ask struct {
	Question string `json:"question"`
}

// Tag reservation routes.
const (
	PathTags        = "/v1/tags"
	PathTagsReserve = "/v1/tags/reserve"
	PathTagsRelease = "/v1/tags/release"
)

// Reserve is the body of POST /v1/tags/reserve.
type Reserve struct {
	RepoID int64  `json:"repo_id"`
	LaneID int64  `json:"lane_id,omitempty"`
	Bump   string `json:"bump"`
}

// ReleaseTag is the body of POST /v1/tags/release.
type ReleaseTag struct {
	RepoID int64  `json:"repo_id"`
	Tag    string `json:"tag"`
}

// ReservationView is a reservation with its lane's name.
type ReservationView struct {
	store.Reservation
	Lane string `json:"lane,omitempty"`
}

// PathSpend is GET (today's spend) and POST (record a front desk turn's cost).
const PathSpend = "/v1/spend"

// SpendToday answers GET /v1/spend.
type SpendToday struct {
	Day string `json:"day"`
	// USD is today's total in dollars, Copilot's credits priced at CreditUSD.
	USD       float64                `json:"usd"`
	Budget    float64                `json:"budget"`
	CreditUSD float64                `json:"credit_usd"`
	BySource  map[string]store.Spend `json:"by_source"`
}

// PathFeed is GET /v1/feed?after=N (or after=latest for the newest id only).
const PathFeed = "/v1/feed"

// PathSettings holds client settings: GET and PUT /v1/settings/{key}.
const PathSettings = "/v1/settings"

// Feed answers GET /v1/feed: items after the given id, and the id to ask after next.
//
// Events is parallel to Items (same length, same order): Events[i] is the event of
// Items[i], one of the Event* values, mapped by EventKind from the item's kind. Draw
// glyphs and colours from the event; Items[i].Kind is the store's raw kind and may grow.
// Use Event(i) rather than indexing Events, so an older daemon's answer still works.
type Feed struct {
	Items  []store.FeedItem `json:"items"`
	Events []string         `json:"events"`
	Last   int64            `json:"last"`
}

// Event returns the event of item i, mapping its kind when the daemon sent no events.
func (f Feed) Event(i int) string {
	if i < len(f.Events) && f.Events[i] != "" {
		return f.Events[i]
	}
	return EventKind(f.Items[i].Kind)
}

// Setting is a setting's value.
type Setting struct {
	Value string `json:"value"`
}

// PathRequests lists requests between lanes (GET).
const PathRequests = "/v1/requests"

func PathRunRequests(id int64) string  { return fmt.Sprintf("%s/%d/requests", PathRuns, id) }
func PathRequestRoute(id int64) string { return fmt.Sprintf("%s/%d/route", PathRequests, id) }

// Route is the body of POST /v1/requests/{id}/route.
type Route struct {
	Lane  string `json:"lane"`
	Agent string `json:"agent,omitempty"`
}

// RequestView is a request with where it came from.
type RequestView struct {
	store.Request
	FromAgent string `json:"from_agent"`
	FromLane  string `json:"from_lane"`
	Repo      string `json:"repo"`
}

// PathDecisions lists decisions (GET).
const PathDecisions = "/v1/decisions"

func PathDecisionAnswer(id int64) string { return fmt.Sprintf("%s/%d/answer", PathDecisions, id) }
func PathRunEvents(id int64) string      { return fmt.Sprintf("%s/%d/events", PathRuns, id) }
func PathRunDecisions(id int64) string   { return fmt.Sprintf("%s/%d/decisions", PathRuns, id) }

// Answer is the body of POST /v1/decisions/{id}/answer.
type Answer struct {
	Answer string `json:"answer"`
}

// DecisionView is a decision with where it came from.
type DecisionView struct {
	store.Decision
	Agent string `json:"agent"`
	Lane  string `json:"lane"`
	Repo  string `json:"repo"`
}

// RunEvents answers GET /v1/runs/{id}/events.
type RunEvents struct {
	Events    []store.Event    `json:"events"`
	Decisions []store.Decision `json:"decisions"`
}

func PathRun(id int64) string     { return fmt.Sprintf("%s/%d", PathRuns, id) }
func PathRunLog(id int64) string  { return fmt.Sprintf("%s/%d/log", PathRuns, id) }
func PathRunStop(id int64) string { return fmt.Sprintf("%s/%d/stop", PathRuns, id) }

// RunView is a run with its lane's and repo's names.
type RunView struct {
	store.Run
	Lane     string `json:"lane"`
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
}

// RunLog answers GET /v1/runs/{id}/log?offset=N: the next piece of the log.
type RunLog struct {
	Data   string `json:"data"`
	Offset int64  `json:"offset"`
	// Done: the run has ended and Data reaches the end of its log.
	Done bool `json:"done"`
}

// PathRepoHook is POST /v1/repos/{id}/hook.
func PathRepoHook(id int64) string { return fmt.Sprintf("/v1/repos/%d/hook", id) }

// PathLaneClose is POST /v1/lanes/{id}/close.
func PathLaneClose(id int64) string { return fmt.Sprintf("%s/%d/close", PathLanes, id) }

// Runtime is what a running daemon writes to its runtime file so clients can find it.
type Runtime struct {
	Addr    string    `json:"addr"`
	PID     int       `json:"pid"`
	Token   string    `json:"token"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
}

// Status answers GET /v1/status.
type Status struct {
	Version    string             `json:"version"`
	PID        int                `json:"pid"`
	Started    time.Time          `json:"started"`
	Store      string             `json:"store"`
	Config     string             `json:"config"`
	Workspaces []WorkspaceSummary `json:"workspaces"`
}

// WorkspaceSummary is one workspace in Status.
type WorkspaceSummary struct {
	store.Workspace
	Repos int `json:"repos"`
	Lanes int `json:"lanes"`
}

// SaveWorkspace is the body of POST /v1/workspaces.
type SaveWorkspace struct {
	Name  string       `json:"name"`
	Path  string       `json:"path"`
	Repos []store.Repo `json:"repos"`
}

// WorkspaceDetail answers POST and GET on workspaces.
type WorkspaceDetail struct {
	store.Workspace
	Repos []store.Repo `json:"repos"`
}

// Resolution answers GET /v1/resolve?path=..., saying what a directory belongs to.
// Fields are empty from the bottom up: a path in a workspace but no repo has only
// Workspace set.
type Resolution struct {
	Path      string           `json:"path"`
	Workspace *store.Workspace `json:"workspace,omitempty"`
	Repo      *store.Repo      `json:"repo,omitempty"`
	Lane      *store.Lane      `json:"lane,omitempty"`
	// Profile is the repo's effective profile when Repo is set.
	Profile *config.Profile `json:"profile,omitempty"`
}

// LaneView is a lane with its repo's name, as lists show it, and what the forge last
// said about its branch.
//
// The forge fields are Shepherd's last known state, refreshed by the watcher; a forge
// that cannot be reached leaves them as they were. They are absent (not guessed) for a
// lane with no merge request, and pipeline fields are absent without a pipeline.
// MRState is one of the MRState* values, PipelineStatus one of the Pipeline* values.
// A badge such as "!34 · pipeline failed" reads mr and pipeline_status.
type LaneView struct {
	store.Lane
	Repo           string `json:"repo"`
	MR             int    `json:"mr,omitempty"`
	MRState        string `json:"mr_state,omitempty"`
	MRURL          string `json:"mr_url,omitempty"`
	Pipeline       int64  `json:"pipeline,omitempty"`
	PipelineStatus string `json:"pipeline_status,omitempty"`
}

// PathLaneShip is POST /v1/lanes/{id}/ship: push the lane's committed work and open or
// update its merge request, after Shepherd's own checks.
func PathLaneShip(id int64) string { return fmt.Sprintf("%s/%d/ship", PathLanes, id) }

// PathLaneScope is POST /v1/lanes/{id}/scope.
func PathLaneScope(id int64) string { return fmt.Sprintf("%s/%d/scope", PathLanes, id) }

// Rescope is the body of POST /v1/lanes/{id}/scope.
type Rescope struct {
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

// CloseLane is the body of POST /v1/lanes/{id}/close.
type CloseLane struct {
	Force bool `json:"force"`
}

// PrePush is the body of POST /v1/hook/pre-push.
type PrePush struct {
	// Path is the worktree git ran the hook in.
	Path string `json:"path"`
	// Remote is the hook's first argument: the remote's name, or its URL when the push
	// names none. An older hook leaves it out, and a push from a running agent's lane is
	// then refused.
	Remote string         `json:"remote,omitempty"`
	Refs   []fold.PushRef `json:"refs"`
}

// RepoHook is the body of POST /v1/repos/{id}/hook: install, uninstall or status.
type RepoHook struct {
	Action string `json:"action"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
