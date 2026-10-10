package api

import "github.com/ubixsys/ubixshepherd/internal/store"

// Event kinds: the closed set a client maps to one glyph and colour each. A feed
// item's raw kind (the store's) may grow; its event never leaves this set. The set only
// grows, and a value keeps its meaning:
//
//	lane_opened, lane_closed, run_started
//	run_ended          legacy: a run ended with no outcome in the kind (rows from before
//	                   the outcome kinds, and the daemon's recovery of a lost run)
//	run_passed         a run ended and succeeded
//	run_failed         a run ended and failed
//	run_interrupted    a run was stopped, or cut short by the daemon stopping
//	run_quota          the agent is out of quota; Shepherd holds its runs
//	commit             a run ended having made commits
//	gate               Shepherd's own check before a push: running, passed, failed, and
//	                   the push that follows
//	pipeline           the forge's pipeline only: pass, fail or hand-back (old rows that
//	                   described the gate stay pipeline)
//	report, decision_asked, decision_answered, request, request_attention,
//	mr, budget, tag, release, config, desk_rotated
//	info               anything else, and any kind a newer daemon adds
const (
	EventLaneOpened       = "lane_opened"
	EventLaneClosed       = "lane_closed"
	EventRunStarted       = "run_started"
	EventRunEnded         = "run_ended"
	EventRunPassed        = "run_passed"
	EventRunFailed        = "run_failed"
	EventRunInterrupted   = "run_interrupted"
	EventRunQuota         = "run_quota"
	EventCommit           = "commit"
	EventGate             = "gate"           // Shepherd's own pre-push check and push
	EventReport           = "report"         // an agent's typed progress report
	EventDecisionAsked    = "decision_asked" // waits for the person
	EventDecisionAnswer   = "decision_answered"
	EventRequest          = "request"           // between lanes, moving normally
	EventRequestAttention = "request_attention" // stuck or failed: the person or front desk must act
	EventMR               = "mr"                // a merge request opened, updated, merged or closed
	EventPipeline         = "pipeline"          // the forge's pipeline: pass, fail or hand-back
	EventBudget           = "budget"
	EventTag              = "tag"
	EventRelease          = "release"
	EventConfig           = "config"
	EventDeskRotated      = "desk_rotated" // the front desk started a new session, seeded with a summary
	EventInfo             = "info"         // anything else, such as a session note
)

// eventKinds is the one place a store feed kind becomes an event.
var eventKinds = map[string]string{
	store.FeedLaneOpened:     EventLaneOpened,
	store.FeedLaneClosed:     EventLaneClosed,
	store.FeedRunStarted:     EventRunStarted,
	store.FeedRunEnded:       EventRunEnded,
	store.FeedRunPassed:      EventRunPassed,
	store.FeedRunFailed:      EventRunFailed,
	store.FeedRunInterrupted: EventRunInterrupted,
	store.FeedRunQuota:       EventRunQuota,
	store.FeedCommit:         EventCommit,
	store.FeedGate:           EventGate,
	store.FeedReport:         EventReport,
	store.FeedDecision:       EventDecisionAsked,
	store.FeedDecisionAnswer: EventDecisionAnswer,
	store.FeedRequest:        EventRequest,
	store.FeedRequestRouted:  EventRequest,
	store.FeedRequestReplied: EventRequest,
	store.FeedRequestStuck:   EventRequestAttention,
	store.FeedRequestFailed:  EventRequestAttention,
	store.FeedMR:             EventMR,
	store.FeedPipeline:       EventPipeline,
	store.FeedBudget:         EventBudget,
	store.FeedTag:            EventTag,
	store.FeedRelease:        EventRelease,
	"config":                 EventConfig,      // the daemon's own kind (daemon.FeedConfig)
	"desk_rotated":           EventDeskRotated, // the daemon desk's own kind (desk.FeedRotated)
}

// EventKind maps a store feed kind to its event; any kind not listed is EventInfo.
func EventKind(feedKind string) string {
	if e, ok := eventKinds[feedKind]; ok {
		return e
	}
	return EventInfo
}
