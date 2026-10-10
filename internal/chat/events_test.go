package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

var allEvents = []string{
	api.EventLaneOpened, api.EventLaneClosed, api.EventRunStarted, api.EventRunEnded, api.EventReport,
	api.EventDecisionAsked, api.EventDecisionAnswer, api.EventRequest, api.EventRequestAttention,
	api.EventMR, api.EventPipeline, api.EventBudget, api.EventTag, api.EventRelease, api.EventConfig, api.EventInfo,
	kindRunPassed, kindRunFailed, kindRunInterrupted, kindRunQuota, kindCommit, kindGate, kindDeskRotated,
}

// Every event has its own glyph, one cell wide, so a line of events reads without colour.
func TestEveryEventHasItsOwnGlyph(t *testing.T) {
	seen := map[string]string{}
	for _, e := range allEvents {
		mk, ok := eventMarks[e]
		if !ok {
			t.Errorf("no mark for %s", e)
			continue
		}
		if w := ansi.StringWidth(mk.glyph); w != 1 {
			t.Errorf("%s: glyph %q is %d wide", e, mk.glyph, w)
		}
		if other, dup := seen[mk.glyph]; dup {
			t.Errorf("%s and %s share %q", e, other, mk.glyph)
		}
		seen[mk.glyph] = e
	}
	if len(eventMarks) != len(allEvents) {
		t.Errorf("%d marks for %d events", len(eventMarks), len(allEvents))
	}
	if markFor("something new").glyph != eventMarks[api.EventInfo].glyph {
		t.Error("an unknown event does not read as info")
	}
}

func TestEventLinesCarryTheirGlyph(t *testing.T) {
	defer usePalette(pal)
	usePalette(newPalette(false))
	m, _, _ := newTestModel()
	printed := capturePrints(m)
	m.lastFeed = 0
	m.Update(feedMsg(api.Feed{
		Last: 3,
		Items: []store.FeedItem{
			{ID: 1, Kind: store.FeedRunStarted, Text: "run 3: copilot started in lane api"},
			{ID: 2, Kind: store.FeedPipeline, Text: "!207 fix/crash: pipeline failed"},
			{ID: 3, Kind: store.FeedDecision, Text: "decision 4 from claude: raise the price?", Ref: 4},
		},
		Events: []string{api.EventRunStarted, api.EventPipeline, api.EventDecisionAsked},
	}))
	out := ansi.Strip(strings.Join(*printed, "\n"))
	for _, want := range []string{"▸ run 3: copilot", "◎ !207 fix/crash", "? decision 4"} {
		if !strings.Contains(out, want) {
			t.Errorf("thread lacks %q:\n%s", want, out)
		}
	}
	// An older daemon sends no events: the client's mapping fills them in.
	m.Update(feedMsg(api.Feed{Last: 4, Items: []store.FeedItem{{ID: 4, Kind: store.FeedMR, Text: "!214 merged"}}}))
	if out := ansi.Strip(strings.Join(*printed, "\n")); !strings.Contains(out, "◆ !214 merged") {
		t.Errorf("thread:\n%s", out)
	}
}

// Every way a run can end wakes the desk, from an older daemon (run_ended) or a newer
// one (the outcome kinds); a commit or the gate alone does not.
func TestFinishedRunsWakeTheDesk(t *testing.T) {
	for _, c := range []struct {
		kind string
		wake bool
	}{
		{store.FeedRunEnded, true},
		{kindRunPassed, true},
		{kindRunFailed, true},
		{kindRunInterrupted, true},
		{kindRunQuota, true},
		{kindCommit, false},
		{kindGate, false},
		{store.FeedRunStarted, false},
	} {
		t.Run(c.kind, func(t *testing.T) {
			m, d, a := newTestModel()
			drive(t, m, m.pollFeed())
			a.feed = []store.FeedItem{{ID: 1, Kind: c.kind, Text: "run 3: claude in lane api, item " + c.kind, Ref: 3}}
			drive(t, m, m.pollFeed())
			woke := len(d.got) == 1 && strings.HasPrefix(d.got[0], "[Shepherd]") && strings.Contains(d.got[0], "item "+c.kind)
			if woke != c.wake || len(d.got) > 1 {
				t.Errorf("%s: desk got %q, want woken %v", c.kind, d.got, c.wake)
			}
		})
	}
}

// The newer kinds get their marks whether the daemon names their events or leaves the
// mapping to a client whose api does not know them yet.
func TestNewerKindsCarryTheirGlyph(t *testing.T) {
	defer usePalette(pal)
	usePalette(newPalette(false))
	items := []store.FeedItem{
		{ID: 1, Kind: kindRunPassed, Text: "run 3 succeeded"},
		{ID: 2, Kind: kindRunFailed, Text: "run 4 failed"},
		{ID: 3, Kind: kindRunInterrupted, Text: "run 5 stopped"},
		{ID: 4, Kind: kindRunQuota, Text: "run 5: out of quota"},
		{ID: 5, Kind: kindCommit, Text: "run 3: 2 commit(s)"},
		{ID: 6, Kind: kindGate, Text: "gate failed: lint"},
		{ID: 7, Kind: "something_newer", Text: "a kind nobody knows"},
	}
	want := []string{"✓ run 3 succeeded", "✗ run 4 failed", "↯ run 5 stopped", "∅ run 5: out of quota", "* run 3: 2 commit(s)", "◇ gate failed: lint", "· a kind nobody knows"}
	for _, events := range [][]string{
		{kindRunPassed, kindRunFailed, kindRunInterrupted, kindRunQuota, kindCommit, kindGate, api.EventInfo},
		nil, // the client maps the kinds itself
	} {
		m, _, _ := newTestModel()
		printed := capturePrints(m)
		m.lastFeed = 0
		m.Update(feedMsg(api.Feed{Last: 7, Items: items, Events: events}))
		out := ansi.Strip(strings.Join(*printed, "\n"))
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("events %v: thread lacks %q:\n%s", events != nil, w, out)
			}
		}
	}
	// Without colour, a failed run is still the loudest: bold, where cut-short runs are not.
	p := newPalette(false)
	if !eventMarks[kindRunFailed].tone(p).GetBold() || eventMarks[kindRunInterrupted].tone(p).GetBold() || eventMarks[kindRunQuota].tone(p).GetBold() {
		t.Error("failed is not the loudest run outcome")
	}
	p = newPalette(true)
	if eventMarks[kindRunInterrupted].tone(p).GetForeground() != groupMarks[groupBroken].tone(p).GetForeground() {
		t.Error("an interrupted run is not in the dock's broken tone")
	}
}
