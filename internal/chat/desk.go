// Package chat is shepherd chat: the one conversation. The person talks to a front desk,
// an agent session Shepherd runs and resumes at the workspace root with Shepherd's
// operator tools and no way to edit files; the swarm's events arrive in the same thread.
package chat

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Line kinds in the thread.
const (
	KindYou      = "you"
	KindDesk     = "desk"
	KindTool     = "tool"
	KindEvent    = "event"
	KindDecision = "decision"
	KindInfo     = "info"
	KindError    = "error"
	// KindCost carries a turn's cost in dollars as its text; the chat records it and
	// does not show it.
	KindCost = "cost"
	// KindPartial carries a piece of the desk's reply as it streams; the whole reply
	// follows as a KindDesk line.
	KindPartial = "partial"
)

// Line is one entry in the thread.
type Line struct {
	Kind string
	Text string
	// Event is the swarm event a line from the feed reports, one of the api.Event*
	// values; it picks the line's glyph and colour.
	Event string
}

// DeskBrief is the front desk's standing instruction, given once per conversation.
const DeskBrief = `You are the front desk of uBixShepherd: the one conversation between the person and a swarm of AI coding agents (Claude Code, Copilot, Cursor) working in lanes across the repos of this workspace. You are a coordinator, not a coder.

- Delegate the work. To change a repo: open a lane (lane_open, with a scope that fits the job), then start an agent in it (lane_run) with a clear brief. Choose the agent that fits; say which and why in a few words. Follow up on a lane with run_continue rather than starting over.
- You cannot edit files or run commands yourself, and should not try. You may read files to plan.
- Decisions agents hold for the person (decision_list) are theirs: bring them up with the options and the recommendation, and record an answer (decision_answer) only with the person's own words.
- Requests between lanes that Shepherd could not route (request_list, needs_routing) are yours to route with request_route, opening a lane first if needed; close one that has gone stale with request_close, saying why.
- Messages starting with [Shepherd] are events from the swarm, not the person. Tell the person briefly what matters, act where it is yours to (routing, follow-ups on work they asked for), and do not start new work they have not asked for.
- Be brief. The person reads a thread with many agents in it: lead with what happened and what needs them.`

// Desk runs the front desk's turns.
type Desk interface {
	// Turn sends a message and calls emit for each line of the reply as it streams. It
	// returns the session, to resume on the next turn.
	Turn(ctx context.Context, session, message string, emit func(Line)) (string, error)
}

// ClaudeDesk is a front desk on Claude Code, headless, resumed turn by turn.
type ClaudeDesk struct {
	// Bin is the claude executable; Shepherd is the shepherd binary that serves the
	// operator tools; Dir is the workspace root the desk works from.
	Bin, Shepherd, Dir string
	Model              string
	// Projects is where Claude Code keeps its sessions, to read history from; empty
	// means its usual place.
	Projects string
}

// Args builds one turn's command line. The message is not on it: Turn writes it to
// claude's standard input, where -p with no prompt argument reads it, so it cannot be
// read with ps or matched by a pkill -f pattern.
func (d ClaudeDesk) Args(session string, newSession bool) []string {
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"shepherd": map[string]any{"command": d.Shepherd, "args": []string{"mcp"}},
	}})
	a := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if newSession {
		a = append(a, "--session-id", session, "--append-system-prompt", DeskBrief)
	} else {
		a = append(a, "--resume", session)
	}
	// Shepherd's operator tools and reading; no edits, no shell.
	a = append(a, "--strict-mcp-config", "--mcp-config", string(mcp),
		"--allowedTools", "mcp__shepherd", "Read", "Grep", "Glob",
		"--disallowedTools", "Edit", "Write", "Bash", "NotebookEdit")
	if d.Model != "" {
		a = append(a, "--model", d.Model)
	}
	return a
}

// WithModel is the desk on another model; "" is Claude Code's default.
func (d ClaudeDesk) WithModel(model string) Desk {
	d.Model = model
	return d
}

// Name says what the desk runs on, for the status line.
func (d ClaudeDesk) Name() string {
	if d.Model != "" {
		return "claude · " + d.Model
	}
	return "claude"
}

// Turn runs one turn of the conversation. An empty session starts a new one.
func (d ClaudeDesk) Turn(ctx context.Context, session, message string, emit func(Line)) (string, error) {
	newSession := session == ""
	if newSession {
		var err error
		if session, err = newUUID(); err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, d.Bin, d.Args(session, newSession)...)
	cmd.Dir = d.Dir
	cmd.Stdin = strings.NewReader(message)
	cmd.Env = append(os.Environ(), "SHEPHERD_CLIENT=desk")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return session, err
	}
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return session, err
	}
	got := Parse(out, emit)
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return session, fmt.Errorf("%v: %s", err, lastLine(msg))
		}
		if !got {
			return session, err
		}
	}
	return session, nil
}

// Parse reads Claude Code's stream-json and emits the thread's lines: the desk's text as
// it streams and then whole, a short line per tool call, and an error when the turn
// fails. It reports whether any reply came through.
func Parse(r io.Reader, emit func(Line)) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	got := false
	for sc.Scan() {
		var m struct {
			Type    string  `json:"type"`
			Subtype string  `json:"subtype"`
			IsError bool    `json:"is_error"`
			Result  string  `json:"result"`
			Cost    float64 `json:"total_cost_usd"`
			Event   struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Text  string          `json:"text"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "stream_event":
			if m.Event.Type == "content_block_delta" && m.Event.Delta.Type == "text_delta" && m.Event.Delta.Text != "" {
				emit(Line{Kind: KindPartial, Text: m.Event.Delta.Text})
			}
		case "assistant":
			for _, c := range m.Message.Content {
				switch c.Type {
				case "text":
					if t := strings.TrimSpace(c.Text); t != "" {
						emit(Line{Kind: KindDesk, Text: t})
						got = true
					}
				case "tool_use":
					// ToolSearch is Claude Code loading its own tool list: noise here.
					if c.Name != "ToolSearch" {
						emit(Line{Kind: KindTool, Text: toolLine(c.Name, c.Input)})
					}
					got = true
				}
			}
		case "result":
			if m.Cost > 0 {
				emit(Line{Kind: KindCost, Text: strconv.FormatFloat(m.Cost, 'f', -1, 64)})
			}
			if m.IsError || (m.Subtype != "" && m.Subtype != "success") {
				text := m.Result
				if text == "" {
					text = m.Subtype
				}
				emit(Line{Kind: KindError, Text: "the desk's turn failed: " + text})
			}
		}
	}
	return got
}

// toolLine is "lane_open feat/login (scope src/auth/**)"-style shorthand for a call.
func toolLine(name string, input json.RawMessage) string {
	name = strings.TrimPrefix(name, "mcp__shepherd__")
	var args map[string]any
	json.Unmarshal(input, &args)
	var parts []string
	for _, k := range []string{"repo", "name", "lane", "agent", "id", "path", "pattern", "file_path"} {
		if v, ok := args[k]; ok {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	if sc, ok := args["scope"].([]any); ok {
		var s []string
		for _, g := range sc {
			s = append(s, fmt.Sprint(g))
		}
		parts = append(parts, "scope "+strings.Join(s, ","))
	}
	for _, k := range []string{"task", "message", "answer"} {
		if v, ok := args[k].(string); ok {
			parts = append(parts, quoteShort(v, 70))
		}
	}
	return strings.TrimSpace(name + " " + strings.Join(parts, " "))
}

func quoteShort(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n-3] + "..."
	}
	return `"` + s + `"`
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
