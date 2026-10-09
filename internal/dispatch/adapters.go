// Package dispatch starts agents in lanes and records what came of each run.
//
// An adapter builds one agent CLI's headless command line. The task is never on it: the
// runner writes the prompt to the agent's standard input, since anyone on the machine can
// read a command line (ps), and a pkill -f or pgrep -f pattern could match an agent
// through the words of its task. By default an
// agent runs with the person's own powers (Claude Code's auto mode and their settings)
// inside the lane's worktree, and never pushes. Where a CLI can deny `git push` itself,
// the adapter says so; the runner also breaks pushing for every agent, so the rule
// holds even for a CLI that cannot express it. A repo with autonomy.push: agent lifts
// both, and the pre-push hook still checks the lane's branch and scope.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
)

// Adapter describes one agent CLI.
type Adapter struct {
	Name string
	// Bin is the executable looked up on PATH.
	Bin string
	// Args builds the headless command line for a run, without the prompt: every CLI
	// here reads it from standard input when it has no prompt argument.
	Args func(o Opts) []string
	// NewSession returns the id a new conversation will have, or "" when the CLI only
	// reveals it in its output (see SessionIn).
	NewSession func(ctx context.Context, bin, worktree string) (string, error)
	// SessionIn finds a session id in a line of output, for CLIs that print it.
	SessionIn func(line string) string
	// Attach is the interactive command line that resumes a session with a person at
	// the keyboard.
	Attach func(session, worktree string) []string
	// WorkerReady reports whether the agent can be given Shepherd's worker tools. For
	// CLIs configured by flag it is always true; Cursor needs a one-time setup.
	WorkerReady func() bool
	// Note is added to the brief: what this CLI's permissions need the agent to know.
	Note string
	// Read turns a line of the agent's output into the run log's line and any cost.
	Read func(line string) Output
	// SessionCost says the cost the CLI reports is the whole session's so far, not
	// this invocation's: the runner keeps the latest figure and records what the run
	// added beyond the earlier runs of the session. It applies to whatever Read
	// returns, dollars or credits, so an adapter that starts reading a cost sets it
	// when its CLI counts the session.
	SessionCost bool
	// Limit reads a line that says the agent's account is out of quota or past a usage
	// limit.
	Limit func(line string) (Limit, bool)
}

// Limit is an agent CLI saying its account is out of quota.
type Limit struct {
	// Text is what it said, for the person.
	Text string
	// Until is when the CLI says the limit resets; zero when it does not say.
	Until time.Time
	// Sure: the CLI said so in a structured event, not in words that a run's own work
	// might contain.
	Sure bool
}

// limitWords reads a usage limit from a line matching re: the words alone, so they
// count only at the end of a failed run.
func limitWords(re *regexp.Regexp) func(string) (Limit, bool) {
	return func(line string) (Limit, bool) {
		if m := re.FindString(line); m != "" {
			return Limit{Text: clip(m, 200)}, true
		}
		return Limit{}, false
	}
}

var (
	claudeLimitWords  = regexp.MustCompile(`(?i)(claude ai )?usage limit reached(\|\d+)?|you.ve hit your (usage )?limit[^"]*|\b(5-hour|weekly|opus) limit reached[^"]*`)
	copilotLimitWords = regexp.MustCompile(`(?i)[^.]*(premium requests? (allowance|limit|quota)|quota (exceeded|exhausted)|you.ve (reached|exceeded) your [a-z ]*(limit|quota|allowance))[^.]*`)
	cursorLimitWords  = regexp.MustCompile(`(?i)(ActionRequiredError: )?you.ve hit your usage limit[^.]*`)
)

// claudeLimit reads Claude Code's rate limit event (stream-json), whose status is
// rejected once the account is out, and failing that its words for it.
func claudeLimit(line string) (Limit, bool) {
	var m struct {
		Type string `json:"type"`
		Info struct {
			Status   string `json:"status"`
			ResetsAt int64  `json:"resetsAt"`
			Kind     string `json:"rateLimitType"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal([]byte(line), &m) == nil && m.Type == "rate_limit_event" {
		if m.Info.Status != "rejected" {
			return Limit{}, false
		}
		l := Limit{Text: "Claude usage limit reached", Sure: true}
		if m.Info.Kind != "" {
			l.Text += " (" + strings.ReplaceAll(m.Info.Kind, "_", " ") + ")"
		}
		if m.Info.ResetsAt > 0 {
			l.Until = time.Unix(m.Info.ResetsAt, 0)
		}
		return l, true
	}
	l, ok := limitWords(claudeLimitWords)(line)
	// The older form carries the reset as a Unix time: "Claude AI usage limit reached|1759999999".
	if _, at, found := strings.Cut(l.Text, "|"); ok && found {
		if s, err := strconv.ParseInt(at, 10, 64); err == nil {
			l.Until = time.Unix(s, 0)
		}
	}
	return l, ok
}

// Opts are what a run's command line is built from. The prompt is not among them: it
// goes on standard input.
type Opts struct {
	Model, Gate, Worktree string
	// Session is the agent's conversation id; Resume says whether it already exists
	// (continue it) or is new (start it under that id, where the CLI allows choosing).
	Session string
	Resume  bool
	// Worker is the shepherd binary that serves the worker tools ("" for none).
	Worker string
	// Mode is the repo's agent.permission_mode; "" is auto.
	Mode string
	// Push lets the agent push its lane's branch; Merge lets it arm merge when the
	// pipeline succeeds on its lane's merge request (glab mr merge).
	Push, Merge bool
}

// mode is the permission mode a run uses.
func (o Opts) mode() string {
	if o.Mode == "" {
		return config.PermAuto
	}
	return o.Mode
}

// allowsAll says the mode lets the agent use any tool, as the person's own sessions do,
// for CLIs that have only allow lists.
func (o Opts) allowsAll() bool {
	return o.mode() == config.PermAuto || o.mode() == config.PermBypass
}

// workerServer is the MCP server entry for the worker tools. The server finds its run
// from SHEPHERD_RUN, which the runner sets for the agent and the agent passes on.
func workerServer(exe string, extra map[string]any) map[string]any {
	srv := map[string]any{"command": exe, "args": []string{"mcp", "--worker"}}
	for k, v := range extra {
		srv[k] = v
	}
	return map[string]any{"mcpServers": map[string]any{"shepherd": srv}}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func always() bool { return true }

var copilotResume = regexp.MustCompile(`copilot --resume=([A-Za-z0-9-]+)`)

var adapters = map[string]Adapter{
	"claude": {
		Name: "claude", Bin: "claude",
		Args: func(o Opts) []string {
			// -p with no prompt argument reads the prompt from standard input.
			// auto, the default, is the person's own auto mode and settings; the allowed
			// list below is what acceptEdits and default need to do the work at all.
			a := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", o.mode()}
			if o.Resume {
				a = append(a, "--resume", o.Session)
			} else if o.Session != "" {
				a = append(a, "--session-id", o.Session)
			}
			if o.Worker != "" {
				// Strict: the agent sees Shepherd's worker tools and no other MCP server,
				// so an operator server registered for the person cannot leak into it.
				a = append(a, "--strict-mcp-config", "--mcp-config", mustJSON(workerServer(o.Worker, nil)))
			}
			a = append(a, "--allowedTools", "Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)",
				"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)")
			if o.Gate != "" {
				a = append(a, "Bash("+o.Gate+":*)")
			}
			if o.Worker != "" {
				a = append(a, "mcp__shepherd")
			}
			if o.Push {
				a = append(a, "Bash(git push:*)")
			}
			if o.Merge {
				a = append(a, "Bash(glab mr view:*)", "Bash(glab mr merge:*)")
			}
			if !o.Push {
				a = append(a, "--disallowedTools", "Bash(git push:*)")
			}
			if o.Model != "" {
				a = append(a, "--model", o.Model)
			}
			return a
		},
		WorkerReady: always,
		// Claude Code takes a session id chosen up front.
		NewSession: func(context.Context, string, string) (string, error) { return newUUID() },
		SessionIn:  func(string) string { return "" },
		Attach:     func(session, _ string) []string { return []string{"--resume", session} },
		Read:       claudeOutput,
		// total_cost_usd on a resumed session is the session's total, not the run's.
		SessionCost: true,
		Limit:       claudeLimit,
	},
	"copilot": {
		Name: "copilot", Bin: "copilot",
		Args: func(o Opts) []string {
			// Copilot has no auto mode: auto and bypassPermissions allow every tool, the
			// nearest to the person's own sessions headless; the other modes keep a list.
			// A deny wins over any allow. No -p: it requires the prompt as its value, and
			// Copilot given a piped prompt instead runs it headless and exits.
			var a []string
			if o.allowsAll() {
				a = append(a, "--allow-all-tools")
			} else {
				a = append(a, "--allow-tool", "write", "--allow-tool", "shell(git:*)")
				if stem := firstWord(o.Gate); stem != "" {
					a = append(a, "--allow-tool", "shell("+stem+")")
				}
				if o.Merge {
					a = append(a, "--allow-tool", "shell(glab mr view)", "--allow-tool", "shell(glab mr merge)")
				}
			}
			if !o.Push {
				a = append(a, "--deny-tool", "shell(git push)")
			}
			if o.Worker != "" {
				a = append(a, "--additional-mcp-config", mustJSON(workerServer(o.Worker, map[string]any{"type": "local", "tools": []string{"*"}})),
					"--allow-tool", "shepherd")
			}
			if o.Resume {
				a = append(a, "--resume="+o.Session)
			}
			if o.Model != "" {
				a = append(a, "--model", o.Model)
			}
			return a
		},
		WorkerReady: always,
		// Copilot names the session itself and prints "copilot --resume=<id>" at the end.
		NewSession: func(context.Context, string, string) (string, error) { return "", nil },
		SessionIn: func(line string) string {
			if m := copilotResume.FindStringSubmatch(line); m != nil {
				return m[1]
			}
			return ""
		},
		Attach: func(session, _ string) []string { return []string{"--resume=" + session} },
		// Copilot approves each part of a chained command, and nobody can approve
		// `exit` or `true` in a headless run.
		Note: "Run each git command on its own (git add, then git commit), not chained with &&, || or ;. Chained commands need an approval nobody can give in this run.",
		Read: copilotOutput,
		// Its "AI Credits" line counts the session: a continued run reports the total.
		SessionCost: true,
		Limit:       limitWords(copilotLimitWords),
	},
	"cursor": {
		Name: "cursor", Bin: "cursor-agent",
		Args: func(o Opts) []string {
			// cursor-agent has no permission modes or per-command deny on the command
			// line: --force lets it run commands in every mode, and the runner's push
			// block holds the line where agents may not push. Its chat is created first
			// (NewSession), so every run resumes one.
			// -p is --print, a switch; with no prompt argument it reads standard input.
			a := []string{"-p", "--output-format", "text", "--force", "--trust", "--workspace", o.Worktree}
			if o.Session != "" {
				a = append(a, "--resume", o.Session)
			}
			if o.Worker != "" {
				// The worker server comes from ~/.cursor/mcp.json (shepherd agents setup
				// cursor); approve it without a prompt.
				a = append(a, "--approve-mcps")
			}
			if o.Model != "" {
				a = append(a, "--model", o.Model)
			}
			return a
		},
		WorkerReady: CursorWorkerReady,
		Read:        plainOutput,
		// It prints "ActionRequiredError: You've hit your usage limit" and exits at once.
		Limit: limitWords(cursorLimitWords),
		NewSession: func(ctx context.Context, bin, worktree string) (string, error) {
			cmd := exec.CommandContext(ctx, bin, "create-chat")
			cmd.Dir = worktree
			out, err := cmd.Output()
			if err != nil {
				return "", fmt.Errorf("cursor-agent create-chat: %w", err)
			}
			lines := strings.Fields(strings.TrimSpace(string(out)))
			if len(lines) == 0 {
				return "", errors.New("cursor-agent create-chat printed no chat id")
			}
			return lines[len(lines)-1], nil
		},
		SessionIn: func(string) string { return "" },
		Attach: func(session, worktree string) []string {
			return []string{"--resume", session, "--workspace", worktree}
		},
	},
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// AdapterFor returns the adapter by name.
func AdapterFor(name string) (Adapter, error) {
	a, ok := adapters[name]
	if !ok {
		return a, fmt.Errorf("unknown agent %q; Shepherd knows %s", name, strings.Join(AgentNames(), ", "))
	}
	return a, nil
}

// AgentNames lists the adapters, sorted.
func AgentNames() []string {
	var out []string
	for n := range adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// Powers are what a repo lets its agents do beyond committing, as the brief states them.
type Powers struct {
	// Push is who pushes the lane's branch: config.Human, config.Shepherd or config.Agent.
	Push string
	// Merge: the agent may arm merge when the pipeline succeeds on its lane's merge
	// request. Only on GitLab.
	Merge bool
	// GitLab: the lane's remote is on GitLab, which opens a merge request from push options.
	GitLab bool
	// Forbid are the patterns commit messages must not match.
	Forbid []string
}

// Brief wraps a task with what every agent needs to know about its lane, so the same
// task means the same thing to every provider.
func Brief(task, lane, repo, branch, base, worktree string, scope []string, gate string, may Powers, tools bool, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working for uBixShepherd in lane %s of repo %s.\n", lane, repo)
	fmt.Fprintf(&b, "Work only inside this directory: %s (branch %s, cut from %s).\n", worktree, branch, base)
	fmt.Fprintf(&b, "Your scope, the only paths you may change: %s. Changes outside it are refused.\n", strings.Join(scope, ", "))
	if gate != "" {
		fmt.Fprintf(&b, "Before committing, run the repo's gate, `%s`, and make it pass.\n", gate)
	}
	switch may.Push {
	case config.Agent:
		b.WriteString("When the work is done, commit it with clear commit messages, then push your lane's branch, and only it: ")
		if may.GitLab {
			fmt.Fprintf(&b, "`git push -u origin %s -o merge_request.create -o merge_request.target=%s -o merge_request.remove_source_branch` pushes it and opens its merge request.", branch, base)
		} else {
			fmt.Fprintf(&b, "`git push -u origin %s`, then open its merge request on the forge.", branch)
		}
		b.WriteString(" The pre-push hook refuses a push of another branch or of changes outside your scope.\n")
		if len(may.Forbid) > 0 {
			fmt.Fprintf(&b, "Before pushing, check that no commit message matches this repo's forbidden patterns (%s), and amend any that does.\n", strings.Join(may.Forbid, ", "))
		}
	case config.Shepherd:
		b.WriteString("When the work is done, commit it with clear commit messages. Do not push: pushing is blocked. When your run ends, Shepherd runs the gate itself, pushes the lane and opens its merge request.\n")
	default:
		b.WriteString("When the work is done, commit it with clear commit messages. Do not push: pushing is blocked, and the person reviews and pushes.\n")
	}
	if may.Merge {
		fmt.Fprintf(&b, "You may merge your own lane's merge request once its pipeline passes: when it is open with your latest commit, arm it with `glab mr merge <iid> --when-pipeline-succeeds --sha <that commit> --remove-source-branch --yes` (`glab mr view %s` shows the iid). GitLab's approvals, pipelines and threads still decide whether it merges. Never approve a merge request, and never merge any other.\n", branch)
	} else {
		b.WriteString("Do not merge or approve merge requests: that is the person's.\n")
	}
	if note != "" {
		b.WriteString(note + "\n")
	}
	if tools {
		b.WriteString(`You have Shepherd's tools. Use them instead of guessing or stopping silently:
- ask_human: anything that is the person's call (money or pricing, published or user-facing text, deleting or overwriting data, anything in production, a change to scope or design with no clear default). Give options and your recommendation, then end your turn: you will be continued with the answer.
- ask_shepherd: you need another lane (an answer from it, a change outside your scope, a review). Then end your turn: you will be continued with the reply.
- report: say progress, done (with what you did and how you checked it) or blocked (and why).
Everything else, keep working without asking.
`)
	} else {
		b.WriteString("If the task cannot be done inside the scope, stop and say why instead of working around it.\n")
	}
	b.WriteString("\n")
	b.WriteString("Task:\n")
	b.WriteString(task)
	b.WriteString("\n")
	return b.String()
}

// CursorConfig is where Cursor reads MCP servers for every workspace.
func CursorConfig() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cursor", "mcp.json")
}

// CursorWorkerReady reports whether ~/.cursor/mcp.json has Shepherd's worker entry.
func CursorWorkerReady() bool {
	b, err := os.ReadFile(CursorConfig())
	if err != nil {
		return false
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	return json.Unmarshal(b, &cfg) == nil && cfg.MCPServers[cursorWorker] != nil
}

// cursorWorker names Shepherd's entry in Cursor's config.
const cursorWorker = "shepherd-worker"

// SetupCursor adds Shepherd's worker server to ~/.cursor/mcp.json, keeping every other
// server and setting as they are. It reports whether it changed the file.
func SetupCursor(exe string) (bool, error) {
	path := CursorConfig()
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return false, fmt.Errorf("%s is not valid JSON, so Shepherd leaves it alone: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	want := map[string]any{"command": exe, "args": []any{"mcp", "--worker"}}
	if cur, ok := servers[cursorWorker]; ok && mustJSON(cur) == mustJSON(want) {
		return false, nil
	}
	servers[cursorWorker] = want
	cfg["mcpServers"] = servers
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(b, '\n'), 0o644)
}
