package api

import (
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestMRState(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "opened": MRStateOpen, "OPEN": MRStateOpen, "merged": MRStateMerged,
		"closed": MRStateClosed, "locked": MRStateUnknown, "draft?": MRStateUnknown,
	} {
		if got := MRState(in); got != want {
			t.Errorf("MRState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPipelineStatus(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "created": PipelinePending, "preparing": PipelinePending, "running": PipelineRunning,
		"success": PipelinePassed, "failed": PipelineFailed, "canceled": PipelineCanceled,
		"skipped": PipelineSkipped, "manual": PipelineUnknown, "something new": PipelineUnknown,
	} {
		if got := PipelineStatus(in); got != want {
			t.Errorf("PipelineStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEventKindIsClosed(t *testing.T) {
	valid := map[string]bool{}
	for _, e := range []string{EventLaneOpened, EventLaneClosed, EventRunStarted, EventRunEnded, EventReport,
		EventDecisionAsked, EventDecisionAnswer, EventRequest, EventRequestAttention, EventMR,
		EventRunPassed, EventRunFailed, EventRunInterrupted, EventRunQuota, EventCommit, EventGate,
		EventPipeline, EventBudget, EventTag, EventRelease, EventConfig, EventDeskRotated, EventInfo} {
		valid[e] = true
	}
	for k := range eventKinds {
		if !valid[EventKind(k)] {
			t.Errorf("kind %q maps outside the set: %q", k, EventKind(k))
		}
	}
	if EventKind(store.FeedSession) != EventInfo || EventKind("brand_new") != EventInfo {
		t.Error("unlisted kinds must be info")
	}
	for kind, want := range map[string]string{
		"pipeline": EventPipeline, "run_ended": EventRunEnded, "gate": EventGate,
		store.FeedCommit: EventCommit, store.FeedRunPassed: EventRunPassed, store.FeedRunFailed: EventRunFailed,
		store.FeedRunInterrupted: EventRunInterrupted, store.FeedRunQuota: EventRunQuota,
	} {
		if got := EventKind(kind); got != want {
			t.Errorf("EventKind(%q) = %q, want %q", kind, got, want)
		}
	}
	if EventKind(store.FeedRequestStuck) != EventRequestAttention || EventKind(store.FeedDecision) != EventDecisionAsked {
		t.Error("mapping wrong")
	}
}

func TestFeedEventFallback(t *testing.T) {
	f := Feed{Items: []store.FeedItem{{Kind: store.FeedMR}}}
	if f.Event(0) != EventMR {
		t.Errorf("Event(0) = %q", f.Event(0))
	}
}
