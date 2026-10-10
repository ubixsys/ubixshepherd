package dispatch

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

// UsageReader gathers what an agent's output says about the tokens and context it used.
// The runner feeds it every line the agent prints; Usage is read once, at the end.
type UsageReader interface {
	Feed(line string)
	// Usage is the record so far: only the token, context and model fields are set.
	Usage() store.Usage
}

// ClaudeUsage reads Claude Code's stream-json for tokens and context, for a run or for a
// front desk turn. fresh says the session began with this invocation: only then are the
// result's own totals this invocation's (on a resumed session Claude Code reports the
// whole session's), so otherwise the totals are summed from the requests it printed.
// Either way the peak context comes from the requests.
type ClaudeUsage struct {
	fresh bool

	reqs        map[string]*claudeRequest
	order       []string
	anon        int
	model       string
	compactions int
	result      *claudeResult
}

// NewClaudeUsage returns a reader; fresh says the session is new.
func NewClaudeUsage(fresh bool) *ClaudeUsage {
	return &ClaudeUsage{fresh: fresh, reqs: map[string]*claudeRequest{}}
}

// claudeRequest is one model request, which Claude Code prints once per content block:
// the counts are the largest seen, as the early copies can be partial.
type claudeRequest struct {
	in, cacheRead, cacheWrite, out int64
	main                           bool // not a subagent's
}

type claudeTokens struct {
	Input         int64 `json:"input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	Output        int64 `json:"output_tokens"`
}

type claudeResult struct {
	usage claudeTokens
	by    map[string]claudeModelUsage
}

type claudeModelUsage struct {
	CostUSD       float64 `json:"costUSD"`
	ContextWindow int     `json:"contextWindow"`
}

// Feed reads one line; anything that is not stream-json is ignored.
func (c *ClaudeUsage) Feed(line string) {
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return
	}
	var m struct {
		Type            string                      `json:"type"`
		Subtype         string                      `json:"subtype"`
		ParentToolUseID string                      `json:"parent_tool_use_id"`
		Usage           claudeTokens                `json:"usage"`
		ModelUsage      map[string]claudeModelUsage `json:"modelUsage"`
		Message         struct {
			ID    string       `json:"id"`
			Model string       `json:"model"`
			Usage claudeTokens `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &m) != nil {
		return
	}
	switch m.Type {
	case "assistant":
		u := m.Message.Usage
		if m.Message.Model == "<synthetic>" || u == (claudeTokens{}) {
			return
		}
		id := m.Message.ID
		if id == "" {
			c.anon++
			id = "anon-" + strconv.Itoa(c.anon)
		}
		r := c.reqs[id]
		if r == nil {
			r = &claudeRequest{}
			c.reqs[id] = r
			c.order = append(c.order, id)
		}
		r.in, r.cacheRead = max(r.in, u.Input), max(r.cacheRead, u.CacheRead)
		r.cacheWrite, r.out = max(r.cacheWrite, u.CacheCreation), max(r.out, u.Output)
		r.main = m.ParentToolUseID == ""
		if r.main && m.Message.Model != "" {
			c.model = m.Message.Model
		}
	case "system":
		if m.Subtype == "compact_boundary" {
			c.compactions++
		}
	case "result":
		c.result = &claudeResult{usage: m.Usage, by: m.ModelUsage}
	}
}

// Usage is what the stream said. Requests is 0 when it carried no counts at all.
func (c *ClaudeUsage) Usage() store.Usage {
	var u store.Usage
	u.Model, u.Compactions = c.model, c.compactions
	for _, id := range c.order {
		r := c.reqs[id]
		u.Requests++
		u.Input += r.in
		u.CacheRead += r.cacheRead
		u.CacheCreation += r.cacheWrite
		u.Output += r.out
		if r.main {
			u.PeakContext = max(u.PeakContext, r.in+r.cacheRead+r.cacheWrite)
		}
	}
	if res := c.result; res != nil {
		if c.fresh || u.Requests == 0 {
			t := res.usage
			if t.Input+t.CacheRead+t.CacheCreation+t.Output > 0 {
				u.Input, u.CacheRead, u.CacheCreation, u.Output = t.Input, t.CacheRead, t.CacheCreation, t.Output
			}
		}
		u.Model, u.ContextWindow = c.modelAndWindow(res)
	}
	return u
}

// modelAndWindow is the model the run's own requests used and the window it ran with.
// With no request to name the model, it is the one that cost most.
func (c *ClaudeUsage) modelAndWindow(res *claudeResult) (string, int) {
	if mu, ok := res.by[c.model]; ok {
		return c.model, mu.ContextWindow
	}
	names := make([]string, 0, len(res.by))
	for n := range res.by {
		names = append(names, n)
	}
	sort.Strings(names)
	best := ""
	for _, n := range names {
		switch {
		case c.model != "" && strings.HasPrefix(n, c.model):
			// The result may name the model with a suffix, "claude-opus-4-5[1m]".
			return n, res.by[n].ContextWindow
		case c.model == "" && (best == "" || res.by[n].CostUSD > res.by[best].CostUSD):
			best = n
		}
	}
	if best == "" {
		return c.model, 0
	}
	return best, res.by[best].ContextWindow
}

// OpenCodeUsage reads the token counts OpenCode prints at the end of each step. A step
// is one model request. window is the context the run's model was configured with.
type OpenCodeUsage struct {
	window int
	u      store.Usage
	ended  bool // from a step_finish: not to be overwritten by an "ended" line
}

// NewOpenCodeUsage returns a reader; window 0 when the model's context is not known.
func NewOpenCodeUsage(window int) *OpenCodeUsage { return &OpenCodeUsage{window: window} }

var openCodeEndedLine = regexp.MustCompile(`opencode ended \([^)]*\): (\d+) tokens in, (\d+) out`)

// Feed reads a step_finish event, or the "opencode ended" line Shepherd writes from them.
func (o *OpenCodeUsage) Feed(line string) {
	if m := openCodeEndedLine.FindStringSubmatch(line); m != nil && !o.ended {
		o.u.Input, _ = strconv.ParseInt(m[1], 10, 64)
		o.u.Output, _ = strconv.ParseInt(m[2], 10, 64)
		o.u.PeakContext = max(o.u.PeakContext, o.u.Input)
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return
	}
	var e struct {
		Type string `json:"type"`
		Part struct {
			Tokens struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
				Cache  struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if json.Unmarshal([]byte(line), &e) != nil || e.Type != "step_finish" {
		return
	}
	t := e.Part.Tokens
	if !o.ended {
		o.u = store.Usage{} // the events are the better source
		o.ended = true
	}
	o.u.Requests++
	o.u.Input += t.Input
	o.u.CacheRead += t.Cache.Read
	o.u.CacheCreation += t.Cache.Write
	o.u.Output += t.Output
	o.u.PeakContext = max(o.u.PeakContext, t.Input+t.Cache.Read+t.Cache.Write)
}

// Usage is what the steps added up to.
func (o *OpenCodeUsage) Usage() store.Usage {
	u := o.u
	if u.Measured() {
		u.ContextWindow = o.window
	}
	return u
}
