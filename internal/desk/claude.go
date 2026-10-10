// Package desk is the front desk the daemon runs: one conversation per workspace, held
// in the store, that the terminal and the browser follow as thin clients. A turn is an
// agent session (Claude Code, headless) resumed with the person's message or with what
// Shepherd has to tell it, one turn at a time.
//
// The turn runner here is a copy of shepherd chat's local desk (internal/chat/desk.go),
// which keeps working on its own until the chat moves onto this one.
package desk

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
	"time"

	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// KindPartial is a piece of the desk's reply as it streams. Partials are sent live and
// never stored: the whole reply follows as a store.DeskAssistant event.
const KindPartial = "partial"

// Line is one thing a turn produced: a store.Desk* kind, or KindPartial.
type Line struct {
	Kind string
	Text string
}

// Spec is one turn to run.
type Spec struct {
	// Session is the agent session to resume, or to create when New.
	Session string
	New     bool
	Message string
	// Dir is the workspace root the desk works from; Model "" is the agent's default.
	Dir, Model string
	// Env is added to the agent's environment: the desk's token and the daemon's URL.
	Env []string
}

// Agent runs a turn and calls emit for each line as it streams.
type Agent interface {
	Turn(ctx context.Context, s Spec, emit func(Line)) error
}

// Brief is the desk's standing instruction, given once per conversation. It matches
// shepherd chat's DeskBrief, with what a desk the daemon runs must know in addition.
const Brief = `You are the front desk of uBixShepherd: the one conversation between the person and a swarm of AI coding agents (Claude Code, Copilot, Cursor, OpenCode) working in lanes across the repos of this workspace. You are a coordinator, not a coder.

- Delegate the work. To change a repo: open a lane (lane_open, with a scope that fits the job), then start an agent in it (lane_run) with a clear brief. Choose the agent that fits; say which and why in a few words. Follow up on a lane with run_continue rather than starting over.
- You cannot edit files or run commands yourself, and should not try. You may read files to plan.
- Decisions agents hold for the person (decision_list) are theirs: bring them up with the options and the recommendation, and record an answer (decision_answer) only with the person's own words, in a turn they started. Shepherd refuses decision_answer in a turn it started.
- Requests between lanes that Shepherd could not route (request_list, needs_routing) are yours to route with request_route, opening a lane first if needed; close one that has gone stale with request_close, saying why.
- Messages starting with [Shepherd] are events from the swarm, not the person. Tell the person briefly what matters, act where it is yours to (routing, follow-ups on work they asked for), and do not start new work they have not asked for.
- Be brief. The person reads a thread with many agents in it: lead with what happened and what needs them.`

// Claude is a desk on Claude Code, headless, resumed turn by turn.
type Claude struct {
	// Bin is the claude executable; Shepherd is the shepherd binary that serves the
	// operator tools.
	Bin, Shepherd string
}

// Args builds one turn's command line. The message is not on it: Turn writes it to
// claude's standard input, so it cannot be read with ps. Neither is the token: the
// operator tools' server inherits it from the environment, and --scoped makes that
// server refuse to start without one rather than fall back to daemon.json.
func (c Claude) Args(s Spec) []string {
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"shepherd": map[string]any{"command": c.Shepherd, "args": []string{"mcp", "--scoped"}},
	}})
	a := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if s.New {
		a = append(a, "--session-id", s.Session, "--append-system-prompt", Brief)
	} else {
		a = append(a, "--resume", s.Session)
	}
	// Shepherd's operator tools and reading; no edits, no shell.
	a = append(a, "--strict-mcp-config", "--mcp-config", string(mcp),
		"--allowedTools", "mcp__shepherd", "Read", "Grep", "Glob",
		"--disallowedTools", "Edit", "Write", "Bash", "NotebookEdit")
	if s.Model != "" {
		a = append(a, "--model", s.Model)
	}
	return a
}

// Turn runs one turn of the conversation.
func (c Claude) Turn(ctx context.Context, s Spec, emit func(Line)) error {
	bin := c.Bin
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("claude"); err != nil {
			return fmt.Errorf("claude is not on the daemon's PATH")
		}
	}
	cmd := exec.CommandContext(ctx, bin, c.Args(s)...)
	cmd.Dir = s.Dir
	cmd.Stdin = strings.NewReader(s.Message)
	env := withoutEnv(os.Environ(), dispatch.EnvToken, dispatch.EnvURL, dispatch.EnvRun, "SHEPHERD_CLIENT")
	cmd.Env = append(append(env, "SHEPHERD_CLIENT="+store.OriginDesk), s.Env...)
	inGroup(cmd)
	// Wait closes the output once the agent has exited and WaitDelay has passed, even if
	// something it started still holds it, so an interrupted turn ends promptly.
	cmd.WaitDelay = 2 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waited <- err
	}()
	got := Parse(pr, emit)
	pr.Close()
	if err := <-waited; err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%v: %s", err, lastLine(msg))
		}
		if !got {
			return err
		}
	}
	return nil
}

// Parse reads Claude Code's stream-json and emits lines: the reply as it streams and
// then whole, a short line per tool call, the session's cost so far, and an error when
// the turn fails. It reports whether any reply came through.
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
						emit(Line{Kind: store.DeskAssistant, Text: t})
						got = true
					}
				case "tool_use":
					// ToolSearch is Claude Code loading its own tool list: noise here.
					if c.Name != "ToolSearch" {
						emit(Line{Kind: store.DeskTool, Text: toolLine(c.Name, c.Input)})
					}
					got = true
				}
			}
		case "result":
			if m.Cost > 0 {
				emit(Line{Kind: store.DeskCost, Text: strconv.FormatFloat(m.Cost, 'f', -1, 64)})
			}
			if m.IsError || (m.Subtype != "" && m.Subtype != "success") {
				text := m.Result
				if text == "" {
					text = m.Subtype
				}
				emit(Line{Kind: store.DeskError, Text: "the desk's turn failed: " + text})
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

// withoutEnv is env without the named variables.
func withoutEnv(env []string, names ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		keep := true
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}
