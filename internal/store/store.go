// Package store is the only way to Shepherd's state. The daemon holds the one Store;
// clients go through the HTTP API. SQLite backs it on a single machine; a server database
// can implement the same interface when Shepherd is hosted.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write collides with something already there.
var ErrConflict = errors.New("conflict")

// Lane states.
const (
	// LaneOpening: recorded, branch and worktree being created.
	LaneOpening = "opening"
	LaneOpen    = "open"
	LaneClosed  = "closed"
)

// Workspace is a directory of repos that one Shepherd works over.
type Workspace struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Created time.Time `json:"created"`
}

// Repo is a git repository that has opted in to a workspace.
type Repo struct {
	ID          int64 `json:"id"`
	WorkspaceID int64 `json:"workspace_id"`
	// Name is the repo's path relative to the workspace, with forward slashes.
	Name   string `json:"name"`
	Path   string `json:"path"`
	Remote string `json:"remote,omitempty"`
	// Stacks are the generic markers found in the repo (go, node, php, ...), from which
	// a pack is chosen.
	Stacks  []string  `json:"stacks,omitempty"`
	Created time.Time `json:"created"`
}

// Lane is one stream of work in a repo, on its own branch and worktree.
type Lane struct {
	ID     int64  `json:"id"`
	RepoID int64  `json:"repo_id"`
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// Base is the branch the lane was cut from and lands on.
	Base     string `json:"base"`
	Worktree string `json:"worktree"`
	// Scope is the globs, relative to the repo, the lane's work stays inside.
	Scope   []string   `json:"scope"`
	State   string     `json:"state"`
	Created time.Time  `json:"created"`
	Closed  *time.Time `json:"closed,omitempty"`
	// Origin is who opened the lane, and from where. Lanes opened before Shepherd
	// recorded it have none: their Via is empty, shown as unknown.
	Origin Origin `json:"origin"`
}

// Lane origins: the surface a lane was opened through.
const (
	OriginCLI    = "cli"    // shepherd lane open, by a person or an agent's shell
	OriginMCP    = "mcp"    // the lane_open tool, from an agent's MCP client
	OriginDesk   = "desk"   // the lane_open tool, from Shepherd's front desk
	OriginImport = "import" // fold import, from a coordination file or an adopted branch
	OriginFollow = "follow" // a release of a repo this one follows
)

// Origin records who opened a lane and from where. Each field is what was known at the
// time; empty means not known, never a guess.
type Origin struct {
	Via string `json:"via,omitempty"`
	// Agent is the agent CLI behind the call (claude, copilot, cursor, opencode), when it said.
	Agent string `json:"agent,omitempty"`
	// Session is the agent's session: the run's, or the front desk's.
	Session string `json:"session,omitempty"`
	// Run is the Shepherd run the call came from, for agents Shepherd started.
	Run int64 `json:"run,omitempty"`
	// PID is the calling process: the shell that ran the CLI, or the agent that runs
	// the MCP server.
	PID int `json:"pid,omitempty"`
	// Dir is the directory the call was made from.
	Dir string `json:"dir,omitempty"`
	// Detail says more where the surface has more to say (the release a follow is for).
	Detail string `json:"detail,omitempty"`
}

// Surface is Via, or "unknown" for a lane opened before Shepherd recorded it.
func (o Origin) Surface() string {
	if o.Via == "" {
		return "unknown"
	}
	return o.Via
}

// String is the origin in one line: "mcp (claude, pid 4242)", or "unknown".
func (o Origin) String() string {
	if o.Via == "" {
		return o.Surface()
	}
	var parts []string
	if o.Agent != "" {
		parts = append(parts, o.Agent)
	}
	if o.Run != 0 {
		parts = append(parts, fmt.Sprintf("run %d", o.Run))
	}
	if o.Session != "" {
		parts = append(parts, "session "+o.Session)
	}
	if o.PID != 0 {
		parts = append(parts, fmt.Sprintf("pid %d", o.PID))
	}
	if o.Detail != "" {
		parts = append(parts, o.Detail)
	}
	if len(parts) == 0 {
		return o.Via
	}
	return o.Via + " (" + strings.Join(parts, ", ") + ")"
}

// Run states.
const (
	RunRunning     = "running"
	RunSucceeded   = "succeeded"
	RunFailed      = "failed"
	RunStopped     = "stopped"
	RunInterrupted = "interrupted" // the daemon stopped while it ran
)

// Run is one agent started in one lane, and what came of it.
type Run struct {
	ID     int64  `json:"id"`
	LaneID int64  `json:"lane_id"`
	Agent  string `json:"agent"`
	Model  string `json:"model,omitempty"`
	Prompt string `json:"prompt"`
	State  string `json:"state"`
	PID    int    `json:"pid,omitempty"`
	// Log is the path of the run's output, redacted.
	Log      string `json:"log"`
	StartSHA string `json:"start_sha"`
	EndSHA   string `json:"end_sha,omitempty"`
	// Commits made during the run, and any files they changed outside the lane's scope.
	Commits  int        `json:"commits"`
	Outside  []string   `json:"outside,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`
	// Session is the agent's own conversation id; a run that continues another shares
	// it, and Parent is the run it follows.
	Session string `json:"session,omitempty"`
	Parent  int64  `json:"parent,omitempty"`
	// CostUSD is what the agent reported in dollars (Claude Code); Credits what it
	// reported in credits (Copilot). Zero when it reports nothing (Cursor, a local OpenCode model). Always this
	// run's own cost.
	CostUSD float64 `json:"cost_usd,omitempty"`
	Credits float64 `json:"credits,omitempty"`
	// SessionUSD and SessionCredits are the session's total as the agent reported it at
	// the end of this run, for a CLI that reports the session's cost rather than the
	// run's (Copilot): the next run of the session costs what it adds beyond them.
	SessionUSD     float64 `json:"session_usd,omitempty"`
	SessionCredits float64 `json:"session_credits,omitempty"`
}

// Spend is money spent on one day by one source (an agent, or the desk). In totals from
// SpendOn, Ref counts the entries.
type Spend struct {
	Day     string  `json:"day"`
	Source  string  `json:"source"`
	Ref     int64   `json:"ref,omitempty"`
	USD     float64 `json:"usd"`
	Credits float64 `json:"credits,omitempty"`
}

// Event is something an agent told Shepherd during a run.
type Event struct {
	ID    int64  `json:"id"`
	RunID int64  `json:"run_id"`
	Kind  string `json:"kind"` // report
	// Status is a report's: progress, done or blocked.
	Status  string    `json:"status,omitempty"`
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
}

// Decision states.
const (
	DecisionOpen     = "open"
	DecisionAnswered = "answered"
)

// Decision is a question an agent holds for a person.
type Decision struct {
	ID             int64    `json:"id"`
	RunID          int64    `json:"run_id"`
	Question       string   `json:"question"`
	Options        []string `json:"options,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	// Why says why the agent judged it the person's call.
	Why    string `json:"why,omitempty"`
	State  string `json:"state"`
	Answer string `json:"answer,omitempty"`
	// AnswerRun is the run that carried the answer back into the agent's session.
	AnswerRun int64      `json:"answer_run,omitempty"`
	Created   time.Time  `json:"created"`
	Answered  *time.Time `json:"answered,omitempty"`
}

// Request states.
const (
	// RequestPending: waiting for the asker to end its turn and the target lane to be free.
	RequestPending = "pending"
	// RequestNeedsRouting: Shepherd cannot route it by rule; the front desk or the
	// person must say which lane and agent.
	RequestNeedsRouting = "needs_routing"
	// RequestRouted: the target agent is working on it.
	RequestRouted = "routed"
	// RequestReplyReady: answered, waiting for the asker's lane to be free.
	RequestReplyReady = "reply_ready"
	// RequestReplied: the reply went back into the asker's conversation.
	RequestReplied = "replied"
	RequestFailed  = "failed"
	// RequestClosed: closed without a reply, by the person or because it went to them
	// as a decision. Its note says which.
	RequestClosed = "closed"
)

// Request is one agent asking, through Shepherd, for something from another lane.
type Request struct {
	ID      int64  `json:"id"`
	FromRun int64  `json:"from_run"`
	Kind    string `json:"kind"` // question, handoff, review
	// Lane is the target lane's name, once known.
	Lane    string `json:"lane,omitempty"`
	Message string `json:"message"`
	State   string `json:"state"`
	// Agent is the target agent, once chosen.
	Agent     string `json:"agent,omitempty"`
	TargetRun int64  `json:"target_run,omitempty"`
	Reply     string `json:"reply,omitempty"`
	ReplyRun  int64  `json:"reply_run,omitempty"`
	// Depth counts the requests in a chain: an agent answering one may ask in turn.
	Depth   int       `json:"depth"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Feed kinds.
const (
	FeedLaneOpened     = "lane_opened"
	FeedLaneClosed     = "lane_closed"
	FeedRunStarted     = "run_started"
	FeedRunEnded       = "run_ended" // older rows, and a run Shepherd could not classify
	FeedRunPassed      = "run_passed"
	FeedRunFailed      = "run_failed"
	FeedRunInterrupted = "run_interrupted" // stopped by the person, or cut short by the daemon stopping
	FeedRunQuota       = "run_quota"       // the agent is out of quota and held
	FeedCommit         = "commit"
	FeedGate           = "gate" // Shepherd's own check before a push, and the push that follows
	FeedReport         = "report"
	FeedDecision       = "decision"
	FeedDecisionAnswer = "decision_answered"
	FeedRequest        = "request"
	FeedRequestRouted  = "request_routed"
	FeedRequestStuck   = "request_needs_routing"
	FeedRequestReplied = "request_replied"
	FeedRequestFailed  = "request_failed"
	FeedMR             = "mr"
	FeedBudget         = "budget"
	FeedTag            = "tag"
	FeedSession        = "session"
	FeedPipeline       = "pipeline" // the forge's pipeline only
	FeedRelease        = "release"
)

// FeedItem is one thing that happened across the swarm, as a line for the person's
// thread. Ref is the run, decision or request it is about.
type FeedItem struct {
	ID      int64     `json:"id"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	Ref     int64     `json:"ref,omitempty"`
	Created time.Time `json:"created"`
}

// Desk event kinds: the closed set of what the daemon's front-desk conversation records.
const (
	DeskUser      = "user"       // the person's message; its Seq is its turn's id
	DeskSystem    = "system"     // what Shepherd told the desk on its own (a wake-up)
	DeskTurnStart = "turn_start" // a turn began
	DeskAssistant = "assistant"  // the desk's reply, whole
	DeskTool      = "tool"       // a tool call, in short
	DeskCost      = "cost"       // what the turn cost, in dollars, as text
	DeskError     = "error"      // the turn failed, or something around it did
	DeskTurnEnd   = "turn_end"   // a turn ended; Text says how when not done
	DeskNew       = "new"        // the person started a new conversation
)

// Desk event origins: who started the turn an event belongs to.
const (
	DeskByHuman  = "human"  // the person, through an operator client
	DeskBySystem = "system" // the daemon, waking the desk for the swarm's events
)

// DeskEvent is one entry in a workspace's front-desk conversation.
type DeskEvent struct {
	Seq         int64  `json:"seq"`
	WorkspaceID int64  `json:"workspace_id"`
	Kind        string `json:"kind"`
	Text        string `json:"text,omitempty"`
	// Turn is the turn it belongs to: the Seq of the message that started it.
	Turn    int64     `json:"turn,omitempty"`
	Origin  string    `json:"origin,omitempty"`
	Created time.Time `json:"created"`
}

// LaneForge is what the forge last said about a lane's branch.
type LaneForge struct {
	LaneID         int64  `json:"lane_id"`
	MR             int    `json:"mr,omitempty"`
	MRState        string `json:"mr_state,omitempty"`
	MRURL          string `json:"mr_url,omitempty"`
	Pipeline       int64  `json:"pipeline,omitempty"`
	PipelineStatus string `json:"pipeline_status,omitempty"`
	// FixTries counts the times Shepherd asked the lane's agent to fix a failed pipeline.
	FixTries int `json:"fix_tries,omitempty"`
	// GateTries counts gate failures handed back to the agent before a push.
	GateTries int `json:"gate_tries,omitempty"`
	// MergeSHA is the commit that landed the lane, once the forge reports it merged.
	MergeSHA string `json:"merge_sha,omitempty"`
}

// Reservation states.
const (
	TagReserved = "reserved"
	// TagPushed: the lane pushed the tag; SHA is the commit it points at.
	TagPushed = "pushed"
	// TagVerified: the tag contains the lane's merge.
	TagVerified = "verified"
	TagReleased = "released" // given back
)

// Reservation is a release version handed to a lane before it tags.
type Reservation struct {
	ID      int64     `json:"id"`
	RepoID  int64     `json:"repo_id"`
	LaneID  int64     `json:"lane_id,omitempty"`
	Tag     string    `json:"tag"`
	State   string    `json:"state"`
	SHA     string    `json:"sha,omitempty"`
	Created time.Time `json:"created"`
}

// Conversation is an agent session a person had outside Shepherd, adopted so it can be
// listed, reopened and asked questions. Dir is where it was started and resumes.
type Conversation struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	RepoID   int64     `json:"repo_id,omitempty"`
	Dir      string    `json:"dir"`
	Title    string    `json:"title"`
	Branches []string  `json:"branches,omitempty"`
	File     string    `json:"file,omitempty"`
	Started  time.Time `json:"started"`
	Last     time.Time `json:"last"`
}

// Store is Shepherd's state.
type Store interface {
	// SaveWorkspace creates the workspace at ws.Path, or renames the one already there,
	// and adds or updates each repo by path. Repos not listed are left as they are.
	SaveWorkspace(ctx context.Context, ws Workspace, repos []Repo) (Workspace, error)
	Workspaces(ctx context.Context) ([]Workspace, error)
	Repos(ctx context.Context, workspaceID int64) ([]Repo, error)
	Repo(ctx context.Context, id int64) (Repo, error)
	// Lanes returns a repo's lanes that are not closed.
	Lanes(ctx context.Context, repoID int64) ([]Lane, error)
	Lane(ctx context.Context, id int64) (Lane, error)
	// CreateLane records a lane; ErrConflict if its name or worktree is taken by a lane
	// that is not closed.
	CreateLane(ctx context.Context, l Lane) (Lane, error)
	SetLaneState(ctx context.Context, id int64, state string) error
	SetLaneScope(ctx context.Context, id int64, scope []string) error
	// DeleteLane forgets a lane that never opened.
	DeleteLane(ctx context.Context, id int64) error

	CreateRun(ctx context.Context, r Run) (Run, error)
	UpdateRun(ctx context.Context, r Run) error
	Run(ctx context.Context, id int64) (Run, error)
	// Runs returns the newest first; laneID 0 means every lane, state "" every state.
	Runs(ctx context.Context, laneID int64, state string, limit int) ([]Run, error)

	AddEvent(ctx context.Context, e Event) (Event, error)
	Events(ctx context.Context, runID int64) ([]Event, error)
	CreateDecision(ctx context.Context, d Decision) (Decision, error)
	Decision(ctx context.Context, id int64) (Decision, error)
	// Decisions returns decisions in a state ("" for all), oldest first.
	Decisions(ctx context.Context, state string) ([]Decision, error)
	// AnswerDecision records the answer; ErrConflict if the decision is not open.
	AnswerDecision(ctx context.Context, id int64, answer string) (Decision, error)
	SetDecisionRun(ctx context.Context, id, runID int64) error

	CreateRequest(ctx context.Context, q Request) (Request, error)
	UpdateRequest(ctx context.Context, q Request) error
	Request(ctx context.Context, id int64) (Request, error)
	// Requests returns requests in any of the states (all when none), oldest first.
	Requests(ctx context.Context, states ...string) ([]Request, error)

	AddFeed(ctx context.Context, kind, text string, ref int64) error
	// Feed returns items after an id, oldest first.
	Feed(ctx context.Context, after int64, limit int) ([]FeedItem, error)
	LastFeed(ctx context.Context) (int64, error)
	// Setting returns "" for a key never set.
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error

	CreateReservation(ctx context.Context, r Reservation) (Reservation, error)
	// Reservations returns a repo's live reservations (not released), oldest first.
	Reservations(ctx context.Context, repoID int64) ([]Reservation, error)
	// SetReservation changes a reservation's state, and its commit when sha is not "".
	SetReservation(ctx context.Context, id int64, state, sha string) error

	PutConversation(ctx context.Context, c Conversation) error
	// Conversations returns adopted conversations, most recent first; repoID 0 for all.
	Conversations(ctx context.Context, repoID int64) ([]Conversation, error)

	AddSpend(ctx context.Context, sp Spend) error
	// SpendOn totals a day's spend by source.
	SpendOn(ctx context.Context, day string) (map[string]Spend, error)

	// AddDeskEvent appends to a workspace's front-desk conversation and returns the
	// event with its sequence number and time. A user or system message with no Turn
	// starts one: its Turn is its own Seq.
	AddDeskEvent(ctx context.Context, e DeskEvent) (DeskEvent, error)
	// DeskEvents returns a workspace's desk events after seq, oldest first.
	DeskEvents(ctx context.Context, workspaceID, after int64, limit int) ([]DeskEvent, error)
	// DeskHistory returns up to limit of a workspace's desk events before seq (0 for
	// the newest), oldest first.
	DeskHistory(ctx context.Context, workspaceID, before int64, limit int) ([]DeskEvent, error)
	// TrimDeskEvents keeps a workspace's newest keep desk events and deletes the rest.
	TrimDeskEvents(ctx context.Context, workspaceID int64, keep int) error

	// LaneForge returns a zero value (with LaneID set) for a lane the forge never saw.
	LaneForge(ctx context.Context, laneID int64) (LaneForge, error)
	PutLaneForge(ctx context.Context, f LaneForge) error
	// Driver names the backing database, for status.
	Driver() string
	Close() error
}
