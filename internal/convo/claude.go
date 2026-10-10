package convo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// loadClaude reads one Claude Code session's JSON-lines file, found under projects in
// whichever project directory holds it.
func loadClaude(projects, id string) ([]entry, string) {
	matches, _ := filepath.Glob(filepath.Join(projects, "*", id+".jsonl"))
	if len(matches) == 0 {
		return nil, fmt.Sprintf("Claude Code's session %s is not under %s (moved or removed); see this run's log", short(id), projects)
	}
	file := matches[0]
	st := stamp(file)
	if es, ok := cacheGet(file, st); ok {
		return es, ""
	}
	fh, err := os.Open(file)
	if err != nil {
		return nil, "Claude Code's session file cannot be read (permissions?); see this run's log"
	}
	defer fh.Close()
	es := parseClaude(fh)
	cachePut(file, st, es)
	return es, ""
}

type claudeLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseClaude normalizes Claude Code's records. Claude writes each content block of an
// assistant message as its own line, and a tool's result as a later user line naming
// the call; a line that does not parse (a half-written last one) is skipped.
func parseClaude(r io.Reader) []entry {
	var out []entry
	tools := map[string]int{} // tool_use id -> index in out
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for sc.Scan() {
		var l claudeLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.IsSidechain || l.IsMeta {
			continue
		}
		if l.Type != "user" && l.Type != "assistant" {
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		var blocks []claudeBlock
		var s string
		if json.Unmarshal(l.Message.Content, &s) == nil {
			blocks = []claudeBlock{{Type: "text", Text: s}}
		} else if json.Unmarshal(l.Message.Content, &blocks) != nil {
			continue
		}
		for n, b := range blocks {
			id := fmt.Sprintf("%s#%d", l.UUID, n)
			if l.UUID == "" {
				id = ""
			}
			switch {
			case l.Type == "user" && b.Type == "text":
				if txt := userVisible(b.Text); txt != "" {
					out = append(out, entry{id: id, t: t, kind: KindUser, text: red(txt)})
				}
			case l.Type == "user" && b.Type == "tool_result":
				i, ok := tools[b.ToolUseID]
				if !ok {
					continue
				}
				out[i].output = red(toolResultText(b.Content))
				out[i].status = ToolOK
				if b.IsError {
					out[i].status = ToolError
				}
			case l.Type == "assistant" && b.Type == "text":
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, entry{id: id, t: t, kind: KindAgent, text: red(b.Text)})
				}
			case l.Type == "assistant" && b.Type == "thinking":
				out = append(out, entry{id: id, t: t, kind: KindThinking, text: red(b.Thinking)})
			case l.Type == "assistant" && b.Type == "tool_use":
				e := entry{id: id, t: t, kind: KindTool, tool: b.Name, status: ToolRunning}
				var in map[string]any
				if json.Unmarshal(b.Input, &in) == nil {
					e.summary = red(summarize(b.Name, in))
					if pretty, err := json.MarshalIndent(in, "", "  "); err == nil {
						e.input = red(string(pretty))
					}
				}
				tools[b.ID] = len(out)
				out = append(out, e)
			}
		}
	}
	return out
}

// userVisible is the part of a user message a person (or Shepherd's brief) said: the
// harness's own wrappers, such as slash-command echoes and reminders, are not.
func userVisible(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"<system-reminder>", "<command-name>", "<command-message>", "<local-command", "<user-prompt-submit-hook>", "Caveat: The messages below"} {
		if strings.HasPrefix(s, p) {
			return ""
		}
	}
	return s
}

// toolResultText flattens a tool result: a string, or a list of text parts.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(bytes.TrimSpace(raw))
	}
	var b strings.Builder
	for _, p := range parts {
		switch {
		case p.Text != "":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		case p.Type != "" && p.Type != "text":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[" + p.Type + "]")
		}
	}
	return b.String()
}
