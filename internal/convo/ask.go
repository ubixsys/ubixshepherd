package convo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// AskTimeout bounds one question to a conversation.
const AskTimeout = 4 * time.Minute

// Answer is what a conversation said back.
type Answer struct {
	Text string  `json:"text"`
	USD  float64 `json:"usd,omitempty"`
}

// AskArgs is the command line for a question: the session resumed, headless, able to
// read but not to edit or run anything. Asking a conversation is asking, not tasking.
// The question is not on it: Ask writes it to standard input, where -p with no prompt
// argument reads it, so it cannot be read with ps.
func AskArgs(id string) []string {
	return []string{"-p", "--resume", id, "--output-format", "stream-json", "--verbose",
		"--allowedTools", "Read", "Grep", "Glob",
		"--disallowedTools", "Edit", "Write", "Bash", "NotebookEdit"}
}

// Ask continues a conversation with a question, in the directory it resumes in, and
// returns its reply. It refuses a conversation that looks open in a terminal: two
// writers on one session would corrupt it.
func Ask(ctx context.Context, bin string, c store.Conversation, question string) (Answer, error) {
	if c.File != "" && InUse(c.File) {
		return Answer{}, fmt.Errorf("conversation %s changed in the last %s, so it may be open in a terminal; ask it there, or try again later", short(c.ID), InUseWindow)
	}
	if _, err := os.Stat(c.Dir); err != nil {
		return Answer{}, fmt.Errorf("conversation %s resumes in %s, which is gone", short(c.ID), c.Dir)
	}
	ctx, cancel := context.WithTimeout(ctx, AskTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, AskArgs(c.ID)...)
	cmd.Dir = c.Dir
	cmd.Stdin = strings.NewReader(question)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Answer{}, err
	}
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return Answer{}, err
	}
	var a Answer
	var texts []string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var m struct {
			Type    string  `json:"type"`
			Result  string  `json:"result"`
			Cost    float64 `json:"total_cost_usd"`
			IsError bool    `json:"is_error"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "assistant":
			for _, part := range m.Message.Content {
				if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
					texts = append(texts, strings.TrimSpace(part.Text))
				}
			}
		case "result":
			a.USD = m.Cost
			if m.Result != "" {
				texts = []string{m.Result} // the final answer, when Claude gives one
			}
		}
	}
	werr := cmd.Wait()
	a.Text = redact.String(strings.TrimSpace(strings.Join(texts, "\n\n")))
	if ctx.Err() != nil {
		return a, fmt.Errorf("no answer within %s", AskTimeout)
	}
	if werr != nil && a.Text == "" {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = werr.Error()
		}
		return a, fmt.Errorf("asking conversation %s failed: %s", short(c.ID), redact.String(msg))
	}
	return a, nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
