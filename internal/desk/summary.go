package desk

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// A new desk session starts from a summary Shepherd builds from its own records, not
// from the old session: the shepherd is not a sheep. A model may add a notes paragraph
// (desk.summary_model), and the summary works without it.

// Continuation is added to the brief of a session seeded with a summary.
const Continuation = `This session continues the front desk's earlier conversation with the person: the earlier session was ended to keep each turn small (or the person started a new one), and you are its successor. The person's thread goes on unchanged for them.

The first message starts with a summary Shepherd built from its own records when this session started: standing rules, lanes, runs, what waits for the person, today's spend, recent runs, and the last turns of the thread. Take it as given: do not re-derive the facts in it with tools, and do not tell the person about the change of session unless they ask. Check a fact with a tool only when you are about to act on it or the person asks for something it does not cover.`

// summaryHeader opens the summary, and summaryEnd closes it, so the seeded message reads
// as two parts: the summary, then the message that started the turn.
const (
	summaryHeader = "[Shepherd] Summary for the new session, built by Shepherd from its records at %s."
	summaryEnd    = "[Shepherd] End of the summary. The message that started this turn follows."
)

// Limits on each part of the summary, inside its overall cap.
const (
	lineMax      = 240  // one item: a lane, run, decision or request
	briefMax     = 400  // a repo's standing brief
	turnTextMax  = 1200 // one message of a quoted turn
	listMax      = 12   // items per list
	recentRuns   = 5    // completed runs listed
	answeredKept = 5    // answered decisions listed
	notesMax     = 1200 // the model's notes
	historyScan  = 400  // desk events read to find the last turns
)

// cutMark ends a summary cut to its cap.
const cutMark = "\n[... cut to desk.summary_chars ...]"

// summaryInput is what a summary is built from, gathered from the store.
type summaryInput struct {
	now      time.Time
	cfg      config.Config
	model    string // the desk.model setting
	repos    []store.Repo
	lanes    map[int64]laneAt // open lanes by id
	running  []store.Run
	reports  map[int64]string // a running run's latest report
	recent   []store.Run      // completed, newest first
	open     []store.Decision
	answered []store.Decision // newest first
	requests []store.Request
	spend    map[string]store.Spend
	turns    []quotedTurn // oldest first
}

type laneAt struct {
	lane store.Lane
	repo string
}

type quotedTurn struct {
	origin, opener, reply, end string
}

// gather reads what the summary needs. before is the turn being seeded, left out of the
// quoted turns: it follows the summary as the message itself.
func (d *Desk) gather(ctx context.Context, before int64) (summaryInput, error) {
	o := d.m.o
	st := o.Store
	in := summaryInput{now: time.Now().UTC(), cfg: o.Config(), lanes: map[int64]laneAt{}, reports: map[int64]string{}}
	in.model, _ = st.Setting(ctx, "desk.model")
	var err error
	if in.repos, err = st.Repos(ctx, d.ws.ID); err != nil {
		return in, err
	}
	for _, r := range in.repos {
		ls, err := st.Lanes(ctx, r.ID)
		if err != nil {
			return in, err
		}
		for _, l := range ls {
			in.lanes[l.ID] = laneAt{lane: l, repo: r.Name}
		}
	}
	// Runs of any lane, closed ones included: a recent run's lane may be closed.
	mine := map[int64]bool{}
	runs, err := st.Runs(ctx, 0, "", 200)
	if err != nil {
		return in, err
	}
	laneRepo := map[int64]string{}
	for _, r := range runs {
		at, ok := in.lanes[r.LaneID]
		if !ok {
			l, err := st.Lane(ctx, r.LaneID)
			if err != nil {
				continue
			}
			rp, err := st.Repo(ctx, l.RepoID)
			if err != nil || rp.WorkspaceID != d.ws.ID {
				continue
			}
			at = laneAt{lane: l, repo: rp.Name}
		}
		laneRepo[r.LaneID] = at.repo
		mine[r.ID] = true
		if r.State == store.RunRunning {
			in.running = append(in.running, r)
		} else if len(in.recent) < recentRuns {
			in.recent = append(in.recent, r)
		}
	}
	for _, r := range in.running {
		evs, err := st.Events(ctx, r.ID)
		if err != nil {
			return in, err
		}
		for _, e := range evs {
			if e.Kind == "report" {
				in.reports[r.ID] = strings.TrimSpace(e.Status + ": " + e.Text)
			}
		}
	}
	// A decision or request belongs to this workspace through its run; one whose run
	// is not among the recent ones is kept, rather than lost from the summary.
	ours := func(run int64) bool {
		if mine[run] {
			return true
		}
		r, err := st.Run(ctx, run)
		if err != nil {
			return true
		}
		_, ok := laneRepo[r.LaneID]
		if !ok {
			l, err := st.Lane(ctx, r.LaneID)
			if err != nil {
				return true
			}
			rp, err := st.Repo(ctx, l.RepoID)
			ok = err != nil || rp.WorkspaceID == d.ws.ID
		}
		mine[run] = ok
		return ok
	}
	ds, err := st.Decisions(ctx, "")
	if err != nil {
		return in, err
	}
	for i := len(ds) - 1; i >= 0; i-- {
		switch dc := ds[i]; {
		case !ours(dc.RunID):
		case dc.State == store.DecisionOpen:
			in.open = append([]store.Decision{dc}, in.open...)
		case len(in.answered) < answeredKept:
			in.answered = append(in.answered, dc)
		}
	}
	reqs, err := st.Requests(ctx, store.RequestPending, store.RequestNeedsRouting, store.RequestRouted, store.RequestReplyReady)
	if err != nil {
		return in, err
	}
	for _, q := range reqs {
		if ours(q.FromRun) {
			in.requests = append(in.requests, q)
		}
	}
	if in.spend, err = st.SpendOn(ctx, dispatch.Today()); err != nil {
		return in, err
	}
	n := in.cfg.Desk.SummaryTurnCount()
	if n == 0 {
		return in, nil
	}
	hist, err := st.DeskHistory(ctx, d.ws.ID, before, historyScan)
	if err != nil {
		return in, err
	}
	in.turns = quoteTurns(hist, n)
	return in, nil
}

// quoteTurns is the last n turns of a stretch of history: what started each, the desk's
// reply, and how the turn ended when not done.
func quoteTurns(hist []store.DeskEvent, n int) []quotedTurn {
	var order []int64
	turns := map[int64]*quotedTurn{}
	for _, e := range hist {
		switch e.Kind {
		case store.DeskUser, store.DeskSystem:
			turns[e.Turn] = &quotedTurn{origin: e.Origin, opener: e.Text}
			order = append(order, e.Turn)
		case store.DeskAssistant:
			if t := turns[e.Turn]; t != nil {
				t.reply = strings.TrimSpace(t.reply + "\n" + e.Text)
			}
		case store.DeskTurnEnd:
			if t := turns[e.Turn]; t != nil && e.Text != "done" {
				t.end = e.Text
			}
		}
	}
	if len(order) > n {
		order = order[len(order)-n:]
	}
	out := make([]quotedTurn, 0, len(order))
	for _, id := range order {
		out = append(out, *turns[id])
	}
	return out
}

// render writes the summary within max characters, redacted. The records come first, in
// order of what the desk most needs; the quoted turns fill what room is left, newest
// first; the notes, if any, come last of all.
func (in summaryInput) render(max int, notes string) string {
	var b strings.Builder
	fmt.Fprintf(&b, summaryHeader+"\n", in.now.Format("2006-01-02 15:04 UTC"))
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n" + title + "\n")
		more := 0
		if len(lines) > listMax {
			more, lines = len(lines)-listMax, lines[:listMax]
		}
		for _, l := range lines {
			b.WriteString("- " + clip(l, lineMax) + "\n")
		}
		if more > 0 {
			fmt.Fprintf(&b, "- (%d more not listed)\n", more)
		}
	}

	section("Waiting for the person (decisions; only the person answers them):", in.decisionLines())
	section("Requests between lanes, not yet replied:", in.requestLines())
	section("Runs in flight:", in.runningLines())
	section("Open lanes:", in.laneLines())
	section("Standing rules (config.yaml and settings):", in.ruleLines())
	section("Decisions the person already answered (newest first):", in.answeredLines())
	section("Recent finished runs (newest first):", in.recentLines())
	if line := in.spendLine(); line != "" {
		b.WriteString("\n" + line + "\n")
	}
	if notes = strings.TrimSpace(notes); notes != "" {
		b.WriteString("\nNotes on the conversation so far (written by a model from the thread; the records above win where they differ):\n" + clip(notes, notesMax) + "\n")
	}
	head := redact.String(b.String())
	end := "\n" + summaryEnd
	room := max - len(head) - len(end)
	if room < 0 {
		return cut(head, max-len(end)-len(cutMark)) + cutMark + end
	}
	// The quoted turns, newest first while they fit, printed oldest first.
	var quoted []string
	used := len("\nThe last turns of the thread, verbatim (oldest first):\n")
	for i := len(in.turns) - 1; i >= 0; i-- {
		q := redact.String(in.turns[i].String())
		if used+len(q) > room {
			// The newest turn matters most: keep what fits of it.
			if i == len(in.turns)-1 && room-used > 200 {
				quoted = []string{cut(q, room-used-4) + "...\n"}
			}
			break
		}
		used += len(q)
		quoted = append([]string{q}, quoted...)
	}
	if len(quoted) > 0 {
		head += "\nThe last turns of the thread, verbatim (oldest first):\n" + strings.Join(quoted, "")
	}
	return head + end
}

func (t quotedTurn) String() string {
	who := "The person"
	if t.origin == store.DeskBySystem {
		who = "Shepherd"
	}
	s := fmt.Sprintf("\n%s:\n%s\n", who, indent(clip(t.opener, turnTextMax)))
	if t.reply != "" {
		s += "The desk:\n" + indent(clip(t.reply, turnTextMax)) + "\n"
	}
	if t.end != "" {
		s += "(the turn ended: " + t.end + ")\n"
	}
	return s
}

func (in summaryInput) decisionLines() []string {
	var out []string
	for _, dc := range in.open {
		l := fmt.Sprintf("decision %d (run %d), waiting %s: %s", dc.ID, dc.RunID, age(in.now, dc.Created), oneLine(dc.Question))
		if len(dc.Options) > 0 {
			l += " Options: " + oneLine(strings.Join(dc.Options, "; ")) + "."
		}
		if dc.Recommendation != "" {
			l += " Recommends: " + oneLine(dc.Recommendation)
		}
		out = append(out, l)
	}
	return out
}

func (in summaryInput) requestLines() []string {
	var out []string
	for _, q := range in.requests {
		l := fmt.Sprintf("request %d, %s from run %d, %s", q.ID, q.Kind, q.FromRun, q.State)
		if q.Lane != "" {
			l += " to lane " + q.Lane
		}
		out = append(out, l+": "+oneLine(q.Message))
	}
	return out
}

func (in summaryInput) runningLines() []string {
	var out []string
	for _, r := range in.running {
		l := fmt.Sprintf("run %d %s in %s, running %s: %s", r.ID, r.Agent, in.laneName(r.LaneID), age(in.now, r.Started), oneLine(r.Prompt))
		if rep := in.reports[r.ID]; rep != "" {
			l = clip(l, lineMax/2) + " | last report: " + oneLine(rep)
		}
		out = append(out, l)
	}
	return out
}

func (in summaryInput) laneLines() []string {
	var out []string
	for _, at := range in.lanes {
		l := at.lane
		out = append(out, fmt.Sprintf("%s %s: %s, scope %s, opened %s ago via %s", at.repo, l.Name, l.State, strings.Join(l.Scope, ", "), age(in.now, l.Created), l.Origin.Surface()))
	}
	sort.Strings(out)
	return out
}

func (in summaryInput) ruleLines() []string {
	var out []string
	if in.model != "" {
		out = append(out, "desk model (set with /model): "+in.model)
	} else if in.cfg.Desk.Model != "" {
		out = append(out, "desk model: "+in.cfg.Desk.Model)
	}
	out = append(out, "desk.wake: "+in.cfg.Desk.WakeMode())
	for _, r := range in.repos {
		p := in.cfg.Profile(r.Name)
		a := p.Autonomy
		l := fmt.Sprintf("repo %s: base %s, push %s, merge %s, tag %s, deploy %s, permission mode %s", r.Name, p.BaseBranch, a.Push, a.Merge, a.Tag, a.Deploy, p.Agent.PermissionMode)
		if a.PlanFirst != nil && *a.PlanFirst {
			l += ", plan first"
		}
		if p.Gate != "" {
			l += ", gate " + p.Gate
		}
		if p.Brief != "" {
			l += ". Brief: " + clip(oneLine(p.Brief), briefMax)
		}
		out = append(out, l)
	}
	return out
}

func (in summaryInput) answeredLines() []string {
	var out []string
	for _, dc := range in.answered {
		out = append(out, fmt.Sprintf("decision %d (run %d): %s Answer: %s", dc.ID, dc.RunID, oneLine(dc.Question), oneLine(dc.Answer)))
	}
	return out
}

func (in summaryInput) recentLines() []string {
	var out []string
	for _, r := range in.recent {
		l := fmt.Sprintf("run %d %s in %s: %s, %d commit(s)", r.ID, r.Agent, in.laneName(r.LaneID), r.State, r.Commits)
		if r.CostUSD > 0 {
			l += fmt.Sprintf(", $%.2f", r.CostUSD)
		}
		if r.Error != "" {
			l += ", error " + oneLine(r.Error)
		}
		out = append(out, l+": "+oneLine(r.Prompt))
	}
	return out
}

func (in summaryInput) laneName(id int64) string {
	if at, ok := in.lanes[id]; ok {
		return at.repo + " " + at.lane.Name
	}
	return fmt.Sprintf("lane %d (closed)", id)
}

func (in summaryInput) spendLine() string {
	if len(in.spend) == 0 {
		return ""
	}
	total := 0.0
	var parts []string
	for src, sp := range in.spend {
		total += sp.USD
		parts = append(parts, fmt.Sprintf("%s $%.2f", src, sp.USD))
	}
	sort.Strings(parts)
	l := fmt.Sprintf("Spend today: $%.2f (%s)", total, strings.Join(parts, ", "))
	if b := in.cfg.Daemon.Budget; b != nil && *b > 0 {
		l += fmt.Sprintf(" of a $%.2f daily budget", *b)
	}
	return l + "."
}

func age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero() || d < 0:
		return "a moment"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

// clip shortens s to n bytes at most, on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cut(s, n-3) + "..."
}

// cut is the first n bytes of s, backed off to a rune boundary.
func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
