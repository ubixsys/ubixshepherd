package convo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// The read side of a lane's conversation: the whole dialogue of every run of a lane, read
// from each agent's own session store and normalized into one thread. It is read-only:
// nothing here opens a session file for writing, and every piece of text is redacted
// before it is returned.

// Item kinds.
const (
	KindRun      = "run"      // a run boundary: Run, Agent, State, Time (start), End
	KindUser     = "user"     // the person's message or Shepherd's brief
	KindAgent    = "agent"    // the agent's own text (markdown)
	KindThinking = "thinking" // reasoning the agent produced; Text may be empty
	KindTool     = "tool"     // a tool call: Tool, Summary, Input, Output, Status
)

// Tool statuses in an Item's Status.
const (
	ToolRunning  = "running"   // called, no result yet, and the run is still going
	ToolOK       = "ok"        // finished
	ToolError    = "error"     // finished with an error
	ToolNoResult = "no_result" // the run ended without a result
)

// Default and maximum sizes, in bytes. A field longer than its cap is clipped, Truncated
// is set, and the item's full text is one Expand away (up to HardCap).
const (
	DefaultTextCap   = 16 << 10
	DefaultOutputCap = 8 << 10
	HardCap          = 2 << 20
	DefaultLimit     = 400
	MaxLimit         = 2000
)

// Item is one entry of a lane's thread. Seq numbers items from 1 in thread order and is
// stable while the transcripts only grow, so a client may merge by Seq.
type Item struct {
	Seq  int        `json:"seq"`
	Kind string     `json:"kind"`
	Run  int64      `json:"run"`
	Time *time.Time `json:"time,omitempty"`
	Text string     `json:"text,omitempty"`

	// Tool calls.
	Tool    string `json:"tool,omitempty"`
	Summary string `json:"summary,omitempty"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	Status  string `json:"status,omitempty"`

	// Truncated says Text, Input or Output was clipped; Bytes is the length of the
	// longest of them before clipping.
	Truncated bool `json:"truncated,omitempty"`
	Bytes     int  `json:"bytes,omitempty"`

	// Run boundaries. Note, when set, says why the run has no transcript below it.
	Agent   string     `json:"agent,omitempty"`
	State   string     `json:"state,omitempty"`
	End     *time.Time `json:"end,omitempty"`
	Session string     `json:"session,omitempty"`
	Note    string     `json:"note,omitempty"`
}

// Thread answers a read of a lane's conversation.
type Thread struct {
	// Items is every run boundary, and the items after the request's cursor.
	Items []Item `json:"items"`
	// Cursor is the seq to ask after next. Items up to it are final; later ones may still
	// change (a tool call waiting for its result), and come again.
	Cursor int `json:"cursor"`
	// More says the page was full: ask again with after set to Cursor.
	More bool `json:"more"`
	// Running says a run of the lane is still going, so more may arrive.
	Running bool `json:"running"`
}

// Options select what to read.
type Options struct {
	// After returns only items with a larger Seq (run boundaries are always returned).
	After int
	// Limit is the most non-boundary items to return; 0 means DefaultLimit.
	Limit int
	// Expand, when non-zero, returns that one item with its full text, up to HardCap.
	Expand int
}

// Source says where each agent keeps its sessions. Zero fields are the agents' defaults.
type Source struct {
	// ClaudeProjects is Claude Code's projects directory.
	ClaudeProjects string
	// OpenCodeDB is OpenCode's database file.
	OpenCodeDB string
}

func (s Source) claude() string {
	if s.ClaudeProjects != "" {
		return s.ClaudeProjects
	}
	return ClaudeHome()
}

func (s Source) opencode() string {
	if s.OpenCodeDB != "" {
		return s.OpenCodeDB
	}
	return OpenCodeDB()
}

// entry is one normalized piece of a transcript, before it is placed in a run.
type entry struct {
	id      string // unique within an agent's store: a repeated one is a copy
	t       time.Time
	kind    string
	text    string
	tool    string
	summary string
	input   string
	output  string
	status  string // tools: ToolOK, ToolError or ToolRunning (no result yet)
}

// sessionKey names one agent session.
type sessionKey struct{ agent, id string }

var sessionID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// NoTranscript is why an agent has no readable transcript.
func noTranscript(agent string) string {
	switch agent {
	case "copilot":
		return "no transcript for copilot: Shepherd does not read Copilot CLI's session store; see this run's log"
	case "cursor":
		return "no transcript for cursor: Cursor's agent keeps its chat in a store Shepherd does not read; see this run's log"
	}
	return fmt.Sprintf("no transcript for %s; see this run's log", agent)
}

// Read builds a lane's thread from its runs (any order). A run whose session cannot be
// found or read still gets its boundary, with a Note saying why.
func Read(ctx context.Context, src Source, runs []store.Run, opt Options) Thread {
	runs = append([]store.Run(nil), runs...)
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].Started.Equal(runs[j].Started) {
			return runs[i].Started.Before(runs[j].Started)
		}
		return runs[i].ID < runs[j].ID
	})
	var th Thread
	var items []Item
	read := map[sessionKey][]entry{}
	notes := map[sessionKey]string{}
	for _, r := range runs {
		if r.Session == "" {
			continue
		}
		k := sessionKey{r.Agent, r.Session}
		if _, ok := read[k]; ok {
			continue
		}
		if _, ok := notes[k]; ok {
			continue
		}
		es, note := load(ctx, src, k)
		if note != "" {
			notes[k] = note
		} else {
			read[k] = es
		}
	}
	// Runs of one session split its entries by time: a run owns those from its start up to
	// the start of the session's next run, the first owning everything before.
	byKey := map[sessionKey][]int{}
	for i, r := range runs {
		if r.Session != "" {
			k := sessionKey{r.Agent, r.Session}
			byKey[k] = append(byKey[k], i)
		}
	}
	seen := map[string]bool{}
	for i, r := range runs {
		if r.State == store.RunRunning {
			th.Running = true
		}
		b := Item{Kind: KindRun, Run: r.ID, Agent: r.Agent, State: r.State, Session: r.Session}
		st := r.Started
		b.Time = &st
		b.End = r.Ended
		k := sessionKey{r.Agent, r.Session}
		switch {
		case !isKnown(r.Agent):
			b.Note = noTranscript(r.Agent)
		case r.Session == "":
			b.Note = "this run has no recorded session, so there is no transcript; see this run's log"
		case notes[k] != "":
			b.Note = notes[k]
		}
		items = append(items, b)
		if b.Note != "" {
			continue
		}
		idx := byKey[k]
		var lo, hi time.Time
		for n, j := range idx {
			if j != i {
				continue
			}
			if n > 0 {
				lo = r.Started
			}
			if n+1 < len(idx) {
				hi = runs[idx[n+1]].Started
			}
		}
		for _, e := range read[k] {
			if (!lo.IsZero() && e.t.Before(lo)) || (!hi.IsZero() && !e.t.Before(hi)) {
				continue
			}
			if e.id != "" {
				id := k.agent + "\x00" + e.id
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			it := Item{Kind: e.kind, Run: r.ID, Text: e.text, Tool: e.tool, Summary: e.summary,
				Input: e.input, Output: e.output, Status: e.status}
			if !e.t.IsZero() {
				t := e.t
				it.Time = &t
			}
			if it.Kind == KindTool && it.Status == ToolRunning && r.State != store.RunRunning {
				it.Status = ToolNoResult
			}
			items = append(items, it)
		}
	}
	for i := range items {
		items[i].Seq = i + 1
	}
	return page(items, opt, th)
}

func isKnown(agent string) bool { return agent == "claude" || agent == "opencode" }

// load reads one session's entries, redacted, with a note when it cannot.
func load(ctx context.Context, src Source, k sessionKey) ([]entry, string) {
	if !sessionID.MatchString(k.id) {
		return nil, "the run's session id is not one Shepherd can look up"
	}
	switch k.agent {
	case "claude":
		return loadClaude(src.claude(), k.id)
	case "opencode":
		return loadOpenCode(ctx, src.opencode(), k.id)
	}
	return nil, noTranscript(k.agent)
}

// page selects the items a read returns, caps their text, and settles the cursor.
func page(items []Item, opt Options, th Thread) Thread {
	if opt.Expand > 0 {
		for _, it := range items {
			if it.Seq == opt.Expand {
				th.Items = []Item{capItem(it, HardCap, HardCap)}
				th.Cursor = opt.After
				return th
			}
		}
		th.Items = []Item{}
		return th
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	th.Items = []Item{}
	th.Cursor = opt.After
	pending := false // a tool call before this point is still waiting for its result
	n := 0
	for _, it := range items {
		if it.Kind == KindRun {
			th.Items = append(th.Items, it)
			continue
		}
		if it.Seq <= opt.After {
			continue
		}
		if n >= limit {
			th.More = true
			break
		}
		n++
		if it.Kind == KindTool && it.Status == ToolRunning {
			pending = true
		}
		th.Items = append(th.Items, capItem(it, DefaultTextCap, DefaultOutputCap))
		if !pending {
			th.Cursor = it.Seq
		}
	}
	if th.Cursor < opt.After {
		th.Cursor = opt.After
	}
	return th
}

// capItem clips an item's long fields.
func capItem(it Item, textCap, outCap int) Item {
	longest := 0
	for _, f := range []*string{&it.Text, &it.Input, &it.Output} {
		if len(*f) > longest {
			longest = len(*f)
		}
	}
	c := func(s *string, n int) {
		if len(*s) > n {
			*s = clipBytes(*s, n)
			it.Truncated = true
		}
	}
	c(&it.Text, textCap)
	c(&it.Input, textCap)
	c(&it.Output, outCap)
	if it.Truncated {
		it.Bytes = longest
	}
	return it
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// red redacts transcript text.
func red(s string) string { return redact.String(s) }

// summarize is a tool call's one-line summary: the input value that says most.
func summarize(tool string, in map[string]any) string {
	for _, k := range []string{"command", "cmd", "file_path", "filePath", "path", "pattern", "query", "url", "description", "prompt", "text", "name"} {
		if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
			return oneLine(v, 140)
		}
	}
	return ""
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// cache keeps the last few sessions read, so following a running lane does not parse a
// large transcript from scratch on every poll. An entry is valid while its files'
// size and modification time are unchanged.
var cache = struct {
	sync.Mutex
	m map[string]cached
}{m: map[string]cached{}}

type cached struct {
	stamp   string
	entries []entry
}

const cacheMax = 8

func stamp(files ...string) string {
	var b strings.Builder
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", filepath.Base(f), fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

func cacheGet(key, st string) ([]entry, bool) {
	cache.Lock()
	defer cache.Unlock()
	c, ok := cache.m[key]
	return c.entries, ok && c.stamp == st
}

func cachePut(key, st string, es []entry) {
	cache.Lock()
	defer cache.Unlock()
	if len(cache.m) >= cacheMax {
		for k := range cache.m {
			delete(cache.m, k)
			break
		}
	}
	cache.m[key] = cached{st, es}
}
