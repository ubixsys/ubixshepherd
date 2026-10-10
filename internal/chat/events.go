package chat

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/ubixsys/ubixshepherd/internal/api"
)

// eventMark is how the thread marks one kind of swarm event: a glyph that carries the
// meaning alone, so the thread reads the same without colour, and a tone from the
// palette. Most events are quiet; only what waits on the person stands out.
type eventMark struct {
	glyph string
	tone  func(palette) lipgloss.Style
}

var (
	toneMuted  = func(p palette) lipgloss.Style { return p.muted }
	toneAccent = func(p palette) lipgloss.Style { return p.accent }
	toneWarn   = func(p palette) lipgloss.Style { return p.warn }
	toneOK     = func(p palette) lipgloss.Style { return p.ok }
	toneBad    = func(p palette) lipgloss.Style { return p.bad }
	toneBroken = func(p palette) lipgloss.Style { return p.broken }
)

// eventMarks covers every event kind. A run's outcome has its own mark: passed is
// quietly positive, failed the loudest, interrupted and out of quota broken. run_ended
// is a run whose outcome the item does not say (an older daemon, or a recovered run).
// The gate and the forge's pipeline pass, fail or hand back under one kind each, so
// their marks are neutral and the text tells.
var eventMarks = map[string]eventMark{
	api.EventLaneOpened:       {"+", toneMuted},
	api.EventLaneClosed:       {"−", toneMuted},
	api.EventRunStarted:       {"▸", toneAccent},
	api.EventRunEnded:         {"■", toneAccent},
	kindRunPassed:             {"✓", toneOK},
	kindRunFailed:             {"✗", toneBad},
	kindRunInterrupted:        {"↯", toneBroken},
	kindRunQuota:              {"∅", toneBroken},
	kindCommit:                {"*", toneMuted},
	kindGate:                  {"◇", toneAccent},
	api.EventReport:           {"»", toneMuted},
	api.EventDecisionAsked:    {"?", toneWarn},
	api.EventDecisionAnswer:   {"↳", toneMuted},
	api.EventRequest:          {"⇄", toneMuted},
	api.EventRequestAttention: {"!", toneWarn},
	api.EventMR:               {"◆", toneAccent},
	api.EventPipeline:         {"◎", toneAccent},
	api.EventBudget:           {"$", toneWarn},
	api.EventTag:              {"#", toneMuted},
	api.EventRelease:          {"▲", toneMuted},
	api.EventConfig:           {"~", toneMuted},
	kindDeskRotated:           {"↻", toneMuted},
	api.EventInfo:             {"·", toneMuted},
}

// feedEvent is the event of item i: the daemon's, or, when this build's api does not
// know a newer kind and calls it info, the kind itself if the chat has a mark for it.
func feedEvent(f api.Feed, i int) string {
	ev := f.Event(i)
	if ev == api.EventInfo {
		if k := f.Items[i].Kind; k != api.EventInfo {
			if _, ok := eventMarks[k]; ok {
				return k
			}
		}
	}
	return ev
}

// markFor is an event's mark; an event this chat does not know reads as info.
func markFor(event string) eventMark {
	if mk, ok := eventMarks[event]; ok {
		return mk
	}
	return eventMarks[api.EventInfo]
}

// eventGlyph is an event's glyph, drawn in its tone.
func eventGlyph(event string) string {
	mk := markFor(event)
	return mk.tone(pal).Render(mk.glyph)
}
