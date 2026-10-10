// Package store is the only way to Shepherd's state. The daemon holds the one Store;
// clients go through the HTTP API. SQLite backs it on a single machine; a server database
// can implement the same interface when Shepherd is hosted.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write collides with something already there.
var ErrConflict = errors.New("conflict")

// ErrSelfApproval is returned when a brief is approved by whoever drafted it. It is a
// conflict: the draft is left as it was.
var ErrSelfApproval = fmt.Errorf("%w: a draft cannot be approved by its drafter", ErrConflict)

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

// Usage kinds: what a usage record measures.
const (
	UsageRun  = "run"  // one agent run
	UsageDesk = "desk" // one turn of the front desk
)

// LargeContext is the prompt size, in tokens, past which a request needed more than a
// 200k window: the line a stats count of "over 200k" is drawn at.
const LargeContext = 200_000

// Usage is the tokens and context one agent run or one front desk turn used, with the
// cost it added. Fields an agent CLI does not report are zero: Requests is 0 when no
// token counts were read at all, so a record can tell "nothing reported" from "small".
type Usage struct {
	ID  int64  `json:"id,omitempty"`
	Day string `json:"day"`
	// Kind is UsageRun or UsageDesk. RunID is the run's id for a run; WorkspaceID and
	// Turn are the desk's workspace and turn id for a turn.
	Kind        string `json:"kind"`
	RunID       int64  `json:"run_id,omitempty"`
	WorkspaceID int64  `json:"workspace_id,omitempty"`
	Turn        int64  `json:"turn,omitempty"`
	Agent       string `json:"agent"`
	// Model is the model the CLI reported, else the one asked for, else "".
	Model string `json:"model,omitempty"`
	// ContextWindow is the window the model ran with, where the CLI reports it.
	ContextWindow int `json:"context_window,omitempty"`
	// Requests counts the model requests the tokens were summed over.
	Requests int `json:"requests,omitempty"`
	// The four token counts, summed over the run's or turn's requests: Input is the
	// uncached prompt, CacheRead and CacheCreation the cached prompt read and written.
	Input         int64 `json:"input_tokens,omitempty"`
	CacheRead     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreation int64 `json:"cache_creation_tokens,omitempty"`
	Output        int64 `json:"output_tokens,omitempty"`
	// PeakContext is the largest single request's prompt in the run or turn: its
	// uncached input plus cache read plus cache creation.
	PeakContext int64 `json:"peak_context,omitempty"`
	// Compactions counts the times the CLI summarised the conversation to make room.
	Compactions int `json:"compactions,omitempty"`
	// CostUSD and Credits are what this run or turn added, as Run and Spend record them.
	CostUSD float64   `json:"cost_usd,omitempty"`
	Credits float64   `json:"credits,omitempty"`
	Created time.Time `json:"created,omitempty"`
}

// Prompt is the tokens the prompts of all requests held, cached or not.
func (u Usage) Prompt() int64 { return u.Input + u.CacheRead + u.CacheCreation }

// Measured says the CLI reported token counts for this record.
func (u Usage) Measured() bool { return u.Requests > 0 || u.Prompt()+u.Output > 0 }

// UsageTotals add up usage records. Count is every record; Measured those with token
// counts, and the peak, the over-200k count and the tokens cover only those.
type UsageTotals struct {
	Count         int   `json:"count"`
	Measured      int   `json:"measured"`
	Input         int64 `json:"input_tokens"`
	CacheRead     int64 `json:"cache_read_tokens"`
	CacheCreation int64 `json:"cache_creation_tokens"`
	Output        int64 `json:"output_tokens"`
	// PeakContext is the largest peak of any record; Over200k counts the records whose
	// peak was above LargeContext. ContextWindow is the largest window reported.
	PeakContext   int64   `json:"peak_context"`
	Over200k      int     `json:"over_200k"`
	ContextWindow int     `json:"context_window,omitempty"`
	Compactions   int     `json:"compactions"`
	CostUSD       float64 `json:"cost_usd"`
	Credits       float64 `json:"credits,omitempty"`
}

// Add folds one record into the totals.
func (t *UsageTotals) Add(u Usage) {
	t.Count++
	t.CostUSD += u.CostUSD
	t.Credits += u.Credits
	t.ContextWindow = max(t.ContextWindow, u.ContextWindow)
	if !u.Measured() {
		return
	}
	t.Measured++
	t.Input += u.Input
	t.CacheRead += u.CacheRead
	t.CacheCreation += u.CacheCreation
	t.Output += u.Output
	t.Compactions += u.Compactions
	t.PeakContext = max(t.PeakContext, u.PeakContext)
	if u.PeakContext > LargeContext {
		t.Over200k++
	}
}

// UsageGroup is the totals of one agent and model, or one lane.
type UsageGroup struct {
	// Key names the group: "claude opus-4" for a model, "repo/lane" for a lane.
	Key   string `json:"key"`
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Lane  string `json:"lane,omitempty"`
	UsageTotals
}

// UsageStats is a range of days' usage, from to to inclusive ("2006-01-02").
type UsageStats struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Total covers every record; Runs the agent runs and Desk the front desk's turns.
	Total UsageTotals `json:"total"`
	Runs  UsageTotals `json:"runs"`
	Desk  UsageTotals `json:"desk"`
	// ByModel groups the runs by agent and model; ByLane by lane, in repo and lane
	// order. DeskByModel groups the desk's turns by model. All are biggest cost first
	// (lanes by repo and name).
	ByModel     []UsageGroup `json:"by_model,omitempty"`
	ByLane      []UsageGroup `json:"by_lane,omitempty"`
	DeskByModel []UsageGroup `json:"desk_by_model,omitempty"`
}

// UsageRow is a usage record with the lane it ran in, for rolling up.
type UsageRow struct {
	Usage
	Repo, Lane string
}

// RollupUsage totals rows into stats for the range from to to.
func RollupUsage(from, to string, rows []UsageRow) UsageStats {
	out := UsageStats{From: from, To: to}
	models := map[string]*UsageGroup{}
	lanes := map[string]*UsageGroup{}
	desk := map[string]*UsageGroup{}
	group := func(m map[string]*UsageGroup, key string, init UsageGroup) *UsageGroup {
		g := m[key]
		if g == nil {
			init.Key = key
			g = &init
			m[key] = g
		}
		return g
	}
	for _, r := range rows {
		out.Total.Add(r.Usage)
		model := r.Model
		if model == "" {
			model = "default model"
		}
		if r.Kind == UsageDesk {
			out.Desk.Add(r.Usage)
			group(desk, model, UsageGroup{Agent: r.Agent, Model: r.Model}).Add(r.Usage)
			continue
		}
		out.Runs.Add(r.Usage)
		group(models, r.Agent+" "+model, UsageGroup{Agent: r.Agent, Model: r.Model}).Add(r.Usage)
		name := "(no lane)"
		init := UsageGroup{}
		if r.Lane != "" {
			name = r.Repo + "/" + r.Lane
			init = UsageGroup{Repo: r.Repo, Lane: r.Lane}
		}
		group(lanes, name, init).Add(r.Usage)
	}
	flat := func(m map[string]*UsageGroup, byCost bool) []UsageGroup {
		var gs []UsageGroup
		for _, g := range m {
			gs = append(gs, *g)
		}
		sort.Slice(gs, func(i, j int) bool {
			if byCost && gs[i].CostUSD != gs[j].CostUSD {
				return gs[i].CostUSD > gs[j].CostUSD
			}
			return gs[i].Key < gs[j].Key
		})
		return gs
	}
	out.ByModel, out.ByLane, out.DeskByModel = flat(models, true), flat(lanes, false), flat(desk, true)
	return out
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

// Request kinds. The first three are between lanes; the last three are the typed kinds
// of a request addressed to a project (see docs/projects.md).
const (
	RequestQuestion = "question"
	RequestHandoff  = "handoff"
	RequestReview   = "review"
	RequestBug      = "bug"
	RequestFeature  = "feature"
	RequestNotice   = "notice"
)

// Request is one agent asking, through Shepherd, for something from another lane, or
// from another project.
type Request struct {
	ID      int64  `json:"id"`
	FromRun int64  `json:"from_run"`
	Kind    string `json:"kind"` // question, handoff, review; bug, feature, notice to a project
	// Project is the receiving project of a request addressed to a project, and
	// FromProject the asker's; Evidence, Touches and Version are its typed fields, which
	// intake rules match on. All are empty on a request between lanes, and on every
	// request recorded before projects existed.
	Project     string   `json:"project,omitempty"`
	FromProject string   `json:"from_project,omitempty"`
	Evidence    []string `json:"evidence,omitempty"`
	Touches     []string `json:"touches,omitempty"`
	Version     string   `json:"version,omitempty"`
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

// History states for LaneHistoryFilter.State.
const (
	HistoryClosed = "closed"
	HistoryOpen   = "open"
	HistoryAll    = "all"
)

// LaneHistoryFilter selects lanes for LaneHistory. Zero fields do not filter, except
// State, where "" means closed.
type LaneHistoryFilter struct {
	WorkspaceID int64
	RepoID      int64
	LaneID      int64
	// State is HistoryClosed, HistoryOpen or HistoryAll. An open lane is one not closed.
	State string
	// Since keeps closed lanes that closed at or after it; open lanes are not filtered.
	Since time.Time
	Limit int
}

// LaneSummary is a lane with its repo's name, its runs' count and cost, and the forge's
// last word on its branch. Closed lanes come newest closed first, others by name.
type LaneSummary struct {
	Lane
	Repo string `json:"repo"`
	// Runs counts every run in the lane; CostUSD and Credits sum their own costs.
	Runs    int       `json:"runs"`
	CostUSD float64   `json:"cost_usd"`
	Credits float64   `json:"credits"`
	Forge   LaneForge `json:"forge"`
}

// RunHistoryFilter selects runs for RunHistory. Zero fields do not filter.
type RunHistoryFilter struct {
	WorkspaceID int64
	RepoID      int64
	LaneID      int64
	Agent       string
	// State is a run state, or "" for every state.
	State string
	// Since keeps runs that started at or after it.
	Since time.Time
	// Limit caps the runs returned (not the totals); 0 means 1000.
	Limit int
}

// RunSummary is a run without its prompt, with where it ran.
type RunSummary struct {
	ID        int64      `json:"id"`
	LaneID    int64      `json:"lane_id"`
	Repo      string     `json:"repo"`
	Lane      string     `json:"lane"`
	LaneState string     `json:"lane_state"`
	Agent     string     `json:"agent"`
	Model     string     `json:"model,omitempty"`
	State     string     `json:"state"`
	Commits   int        `json:"commits"`
	CostUSD   float64    `json:"cost_usd,omitempty"`
	Credits   float64    `json:"credits,omitempty"`
	Started   time.Time  `json:"started"`
	Ended     *time.Time `json:"ended,omitempty"`
	// Task is the first line of the run's prompt, clipped.
	Task string `json:"task"`
}

// HistoryLane names a lane that has runs in a range, for a filter.
type HistoryLane struct {
	ID   int64  `json:"id"`
	Repo string `json:"repo"`
	Name string `json:"name"`
}

// RunHistory is RunHistory's answer.
type RunHistory struct {
	Runs []RunSummary
	// Count, CostUSD and Credits cover every matching run, not just those in Runs.
	Count   int
	CostUSD float64
	Credits float64
	// Agents and Lanes are what the range (since, workspace, repo, state) holds, before
	// the agent and lane filters, so a filter can offer the other choices.
	Agents []string
	Lanes  []HistoryLane
}

// Brief states.
const (
	// BriefDraft: written, waiting for the person. Never given to an agent.
	BriefDraft = "draft"
	// BriefApproved: the project's current brief. At most one per project.
	BriefApproved = "approved"
	// BriefSuperseded: replaced by a later approval, or by a newer draft before it was
	// approved. Kept as history.
	BriefSuperseded = "superseded"
)

// ProjectBrief is a project's goal and current focus: text with an author and an approval.
type ProjectBrief struct {
	ID      int64  `json:"id"`
	Project string `json:"project"`
	Text    string `json:"text"`
	State   string `json:"state"`
	// DraftedBy and ApprovedBy name who did it, as the caller identifies them (the desk,
	// a run as "run:<id>", the person). The two must differ.
	DraftedBy  string     `json:"drafted_by"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	Created    time.Time  `json:"created"`
	Approved   *time.Time `json:"approved,omitempty"`
}

// Age is the wall-clock time since the brief was approved; zero for a brief that is not
// approved.
func (b ProjectBrief) Age(now time.Time) time.Duration {
	if b.Approved == nil || now.Before(*b.Approved) {
		return 0
	}
	return now.Sub(*b.Approved)
}

// Stale reports whether an approved brief is older than max. A brief exactly max old is
// not stale yet; a draft or a zero max is never stale.
func (b ProjectBrief) Stale(now time.Time, max time.Duration) bool {
	return b.Approved != nil && max > 0 && b.Age(now) > max
}

// RepoSpend is what the runs in one repo cost over a range of days.
type RepoSpend struct {
	RepoID  int64   `json:"repo_id"`
	Repo    string  `json:"repo"`
	Runs    int64   `json:"runs"`
	USD     float64 `json:"usd"`
	Credits float64 `json:"credits,omitempty"`
}

// SpendRollup is a range of days' spend split into what a budget needs: each repo (the
// caller sums a project's members), the front desk as its own line, and what belongs to
// neither. Every entry of the range is in exactly one part, so the parts sum to the
// workspace total. The store knows no projects; membership is configuration.
type SpendRollup struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Repos has one entry for each repo that has a run's cost in the range, ordered by name.
	Repos []RepoSpend `json:"repos,omitempty"`
	// Desk is the front desk's spend. It is charged to no repo.
	Desk Spend `json:"desk"`
	// Other is spend with no repo and not the desk's: an adopted session's cost.
	Other Spend `json:"other"`
}

// Total is the whole range's spend in dollars and credits.
func (r SpendRollup) Total() (usd, credits float64) {
	usd, credits = r.Desk.USD+r.Other.USD, r.Desk.Credits+r.Other.Credits
	for _, rs := range r.Repos {
		usd += rs.USD
		credits += rs.Credits
	}
	return usd, credits
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
	// SaveBriefDraft records a draft brief for a project. An earlier draft that was never
	// approved becomes superseded, so a project has at most one open draft.
	SaveBriefDraft(ctx context.Context, project, text, draftedBy string) (ProjectBrief, error)
	// ApproveBrief approves a draft and supersedes the project's previous approved brief,
	// in one step. ErrNotFound for an unknown id; ErrConflict if it is not a draft;
	// ErrSelfApproval if approvedBy is empty or is who drafted it.
	ApproveBrief(ctx context.Context, id int64, approvedBy string) (ProjectBrief, error)
	ProjectBrief(ctx context.Context, id int64) (ProjectBrief, error)
	// ApprovedBrief returns a project's approved brief; ErrNotFound if it has none.
	ApprovedBrief(ctx context.Context, project string) (ProjectBrief, error)
	// PendingBrief returns a project's open draft; ErrNotFound if it has none.
	PendingBrief(ctx context.Context, project string) (ProjectBrief, error)
	// BriefHistory returns a project's briefs of every state, newest first.
	BriefHistory(ctx context.Context, project string) ([]ProjectBrief, error)

	// SpendRollup splits the spend of the days from to to inclusive ("2006-01-02") by repo,
	// the desk and the rest.
	SpendRollup(ctx context.Context, from, to string) (SpendRollup, error)

	// AddUsage records a run's or a desk turn's usage; a run has at most one record, and
	// adding another replaces it.
	AddUsage(ctx context.Context, u Usage) error
	// RunUsage returns a run's usage; ErrNotFound if none was recorded.
	RunUsage(ctx context.Context, runID int64) (Usage, error)
	// UsageStats totals the usage of the days from to to inclusive ("2006-01-02").
	UsageStats(ctx context.Context, from, to string) (UsageStats, error)

	// DeskEvents returns a workspace's desk events after seq, oldest first.
	DeskEvents(ctx context.Context, workspaceID, after int64, limit int) ([]DeskEvent, error)
	// DeskHistory returns up to limit of a workspace's desk events before seq (0 for
	// the newest), oldest first.
	DeskHistory(ctx context.Context, workspaceID, before int64, limit int) ([]DeskEvent, error)
	// TrimDeskEvents keeps a workspace's newest keep desk events and deletes the rest.
	TrimDeskEvents(ctx context.Context, workspaceID int64, keep int) error

	// LaneHistory returns lanes with their run counts, cost and forge state, for the
	// history views: read-only, closed lanes included.
	LaneHistory(ctx context.Context, f LaneHistoryFilter) ([]LaneSummary, error)
	// RunHistory returns runs newest first with the cost of every run that matches, and
	// what the range holds to filter by. Read-only.
	RunHistory(ctx context.Context, f RunHistoryFilter) (RunHistory, error)

	// LaneForge returns a zero value (with LaneID set) for a lane the forge never saw.
	LaneForge(ctx context.Context, laneID int64) (LaneForge, error)
	PutLaneForge(ctx context.Context, f LaneForge) error
	// Driver names the backing database, for status.
	Driver() string
	Close() error
}

// Tokens writes a token count the short way: 950, 187k, 1.2M.
func Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 10_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	case n >= 1000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
	}
	return fmt.Sprint(n)
}

// Line is a record's usage on one line: model, peak context against the window, the
// token counts and compactions.
func (u Usage) Line() string {
	var parts []string
	if u.Model != "" {
		parts = append(parts, u.Model)
	}
	if u.Requests > 0 {
		parts = append(parts, fmt.Sprintf("%d request(s)", u.Requests))
	}
	if u.PeakContext > 0 {
		peak := "peak context " + Tokens(u.PeakContext)
		if u.ContextWindow > 0 {
			peak += " of a " + Tokens(int64(u.ContextWindow)) + " window"
		}
		parts = append(parts, peak)
	} else if u.ContextWindow > 0 {
		parts = append(parts, "window "+Tokens(int64(u.ContextWindow)))
	}
	parts = append(parts, fmt.Sprintf("in %s, cache read %s, cache write %s, out %s",
		Tokens(u.Input), Tokens(u.CacheRead), Tokens(u.CacheCreation), Tokens(u.Output)))
	if u.Compactions > 0 {
		parts = append(parts, fmt.Sprintf("%d compaction(s)", u.Compactions))
	}
	return strings.Join(parts, "; ")
}
