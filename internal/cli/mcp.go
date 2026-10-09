package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/version"
)

// shepherd mcp serves Shepherd's operator tools over MCP on stdio (newline-delimited
// JSON-RPC 2.0). Each tool runs the CLI command of the same name, so the two never
// drift: the MCP server is one more client of the daemon, like the CLI.

// mcpVersions are the protocol revisions this server speaks; it answers with the
// client's when it is one of these, else the newest. The tools use only base features.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `Shepherd keeps parallel work in a workspace of git repos apart. A lane is one stream of work in one repo: its own branch, cut from a fresh fetch of the repo's base branch, its own worktree, and a scope (globs relative to the repo) that its changes stay inside. Open a lane before changing a repo, and work only in the lane's worktree. Scopes are leases: an overlapping scope is refused, naming the lane that holds it. A pre-push hook refuses pushes outside the scope. Close a lane once its MR is merged. Never pass force to lane_close unless the person has confirmed the MR is merged (a squash merge looks unmerged to git).`

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// args turns the call's arguments into CLI arguments.
	args func(map[string]any) ([]string, error) `json:"-"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	propRepo = map[string]any{"type": "string", "description": "Repo, by its path in the workspace as lane_list and shepherd_status show it (for example \"ubixshepherd\")."}
	propPath = map[string]any{"type": "string", "description": "Absolute directory to resolve. Default: where the MCP server runs."}
)

func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "shepherd_status",
			Description: "The daemon, its workspaces with repo and lane counts, and what the current directory resolves to.",
			InputSchema: obj(map[string]any{}),
			args:        func(map[string]any) ([]string, error) { return []string{"status"}, nil },
		},
		{
			Name:        "shepherd_where",
			Description: "Which workspace, repo and lane a directory belongs to, with the repo's profile (base branch, branch model, gate, shared paths, autonomy).",
			InputSchema: obj(map[string]any{"path": propPath}),
			args: func(a map[string]any) ([]string, error) {
				out := []string{"where"}
				if p := str(a, "path"); p != "" {
					out = append(out, p)
				}
				return out, nil
			},
		},
		{
			Name:        "lane_list",
			Description: "Open lanes: repo, name, state, age, scope, and who opened each (the surface, with agent, run, session, pid and directory where known; unknown for lanes from before Shepherd recorded it). Without repo, every repo in the workspace.",
			InputSchema: obj(map[string]any{"repo": propRepo}),
			args: func(a map[string]any) ([]string, error) {
				if r := str(a, "repo"); r != "" {
					return []string{"lane", "list", "--repo", r}, nil
				}
				return []string{"lane", "list", "--all"}, nil
			},
		},
		{
			Name:        "lane_open",
			Description: "Open a lane: fetch, cut the branch from origin/<base>, add the worktree, record the scope. Returns the worktree path to work in. Refused if the scope overlaps an open lane's.",
			InputSchema: obj(map[string]any{
				"repo":   propRepo,
				"name":   map[string]any{"type": "string", "description": "Lane and branch name: lowercase, at most one slash (feat/login, fix-crash)."},
				"scope":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Globs relative to the repo the work stays inside: \"src/auth/**\", \"docs/*.md\", \"README.md\"."},
				"branch": map[string]any{"type": "string", "description": "Branch name if it should differ from the lane name."},
			}, "repo", "name", "scope"),
			args: func(a map[string]any) ([]string, error) {
				scope, err := strs(a, "scope")
				if err != nil {
					return nil, err
				}
				out := []string{"lane", "open", str(a, "name"), "--repo", str(a, "repo")}
				for _, s := range scope {
					out = append(out, "--scope", s)
				}
				if b := str(a, "branch"); b != "" {
					out = append(out, "--branch", b)
				}
				return out, nil
			},
		},
		{
			Name:        "lane_close",
			Description: "Close a lane: remove its worktree and delete its branch if git sees it merged. Refuses uncommitted changes or an unmerged branch unless force is set.",
			InputSchema: obj(map[string]any{
				"repo":  propRepo,
				"name":  map[string]any{"type": "string", "description": "The lane's name."},
				"force": map[string]any{"type": "boolean", "description": "Close anyway, discarding uncommitted changes and keeping an unmerged branch. Only after the person confirms the MR is merged."},
			}, "repo", "name"),
			args: func(a map[string]any) ([]string, error) {
				out := []string{"lane", "close", str(a, "name"), "--repo", str(a, "repo")}
				if f, _ := a["force"].(bool); f {
					out = append(out, "--force")
				}
				return out, nil
			},
		},
		{
			Name: "lane_ship",
			Description: "Push a lane's committed, unpushed work and open or update its merge request, the way a run's work is shipped when it ends. Shepherd checks the scope and the repo's commit rules and runs the repo's gate itself first (this can take minutes), and refuses with the reason if any fails. " +
				"Only for repos whose profile has autonomy.push: shepherd. Never merges: the merge request is the person's to review.",
			InputSchema: obj(map[string]any{
				"repo": propRepo,
				"name": map[string]any{"type": "string", "description": "The lane's name."},
			}, "repo", "name"),
			args: func(a map[string]any) ([]string, error) {
				return []string{"lane", "ship", str(a, "name"), "--repo", str(a, "repo")}, nil
			},
		},
		{
			Name: "lane_run",
			Description: "Start an agent (claude, copilot or cursor) headless in a lane's worktree on a task, and return at once with the run id. The lane keeps its conversation: by default this continues the lane's last session with the same agent. " +
				"The agent may edit and commit inside the lane's scope and run the repo's gate; it can never push. One agent per lane. Check on it with run_status.",
			InputSchema: obj(map[string]any{
				"repo":        propRepo,
				"lane":        map[string]any{"type": "string", "description": "The lane's name; open it first with lane_open."},
				"agent":       map[string]any{"type": "string", "enum": []string{"claude", "copilot", "cursor"}},
				"task":        map[string]any{"type": "string", "description": "What the agent should do, as you would brief a colleague. Shepherd adds the lane, scope and rules."},
				"model":       map[string]any{"type": "string", "description": "Model, if not the repo's default for this agent (agent.model in its profile) or the agent's own."},
				"new_session": map[string]any{"type": "boolean", "description": "Start a new conversation. By default the lane keeps its conversation: the run continues the lane's last session with this agent."},
			}, "repo", "lane", "agent", "task"),
			args: func(a map[string]any) ([]string, error) {
				out := []string{"lane", "run", str(a, "lane"), str(a, "task"), "--repo", str(a, "repo"), "--agent", str(a, "agent"), "--detach"}
				if m := str(a, "model"); m != "" {
					out = append(out, "--model", m)
				}
				if n, _ := a["new_session"].(bool); n {
					out = append(out, "--new")
				}
				return out, nil
			},
		},
		{
			Name:        "run_continue",
			Description: "Continue a finished run's conversation with a follow-up (same agent, same session, same lane): a correction, the next step, a pipeline failure to fix. Returns the new run's id at once.",
			InputSchema: obj(map[string]any{
				"id":      map[string]any{"type": "integer", "description": "The run whose conversation to continue."},
				"message": map[string]any{"type": "string", "description": "What to tell the agent next."},
			}, "id", "message"),
			args: func(a map[string]any) ([]string, error) {
				id, ok := a["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("id must be a number")
				}
				return []string{"run", "continue", fmt.Sprint(int64(id)), str(a, "message"), "--detach"}, nil
			},
		},
		{
			Name:        "run_list",
			Description: "Recent agent runs across the workspace: id, agent, lane, state, commits, age, task.",
			InputSchema: obj(map[string]any{}),
			args:        func(map[string]any) ([]string, error) { return []string{"run", "list", "--all"}, nil },
		},
		{
			Name:        "run_status",
			Description: "One run: state, time, exit code, commits made, any files changed outside the lane's scope, and its log (long logs keep their start and end).",
			InputSchema: obj(map[string]any{"id": map[string]any{"type": "integer"}}, "id"),
			args: func(a map[string]any) ([]string, error) {
				id, ok := a["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("id must be a number")
				}
				return []string{"run", "show", fmt.Sprint(int64(id)), "--with-log"}, nil
			},
		},
		{
			Name:        "run_stop",
			Description: "Stop a running agent. Its commits so far stay in the lane.",
			InputSchema: obj(map[string]any{"id": map[string]any{"type": "integer"}}, "id"),
			args: func(a map[string]any) ([]string, error) {
				id, ok := a["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("id must be a number")
				}
				return []string{"run", "stop", fmt.Sprint(int64(id))}, nil
			},
		},
		{
			Name: "lane_review",
			Description: "Judge each of a repo's worktrees, lanes or not: finished (everything in the base or a merged request), live (uncommitted work, an open request, recent activity) or unclear (unlanded work, nothing recent), with the evidence. Changes nothing. " +
				"Use it to tell the person which old worktrees are safe to retire; retiring is theirs to ask for.",
			InputSchema: obj(map[string]any{"repo": propRepo}, "repo"),
			args: func(a map[string]any) ([]string, error) {
				return []string{"lane", "review", "--repo", str(a, "repo")}, nil
			},
		},
		{
			Name:        "session_list",
			Description: "Conversations the person had with agents outside Shepherd (adopted Claude Code sessions): id, repo, title, dates, branches. Ask one with session_ask.",
			InputSchema: obj(map[string]any{}),
			args:        func(map[string]any) ([]string, error) { return []string{"session", "list"}, nil },
		},
		{
			Name: "session_ask",
			Description: "Ask an adopted conversation a question; it answers with its own history and can read files but not change anything. Use it to recover what was decided or done in work the person did by hand. " +
				"Refused if the conversation looks open in a terminal right now.",
			InputSchema: obj(map[string]any{
				"id":       map[string]any{"type": "string", "description": "The conversation's id, or its first 8 characters."},
				"question": map[string]any{"type": "string"},
			}, "id", "question"),
			args: func(a map[string]any) ([]string, error) {
				return []string{"session", "ask", str(a, "id"), str(a, "question")}, nil
			},
		},
		{
			Name:        "tag_reserve",
			Description: "Reserve the next release version in a repo (major, minor or patch) for a lane, atomically against the remote's tags and other reservations. In repos whose tags are reserved, a release tag must be reserved before it is pushed.",
			InputSchema: obj(map[string]any{
				"repo": propRepo,
				"bump": map[string]any{"type": "string", "enum": []string{"major", "minor", "patch"}},
				"lane": map[string]any{"type": "string", "description": "The lane it is for; leave out for a release cut outside any lane."},
			}, "repo", "bump"),
			args: func(a map[string]any) ([]string, error) {
				out := []string{"tag", "reserve", str(a, "bump"), "--repo", str(a, "repo")}
				if l := str(a, "lane"); l != "" {
					out = append(out, "--lane", l)
				} else {
					out = append(out, "--no-lane")
				}
				return out, nil
			},
		},
		{
			Name:        "tag_list",
			Description: "A repo's live tag reservations: version, state (reserved, pushed, verified) and lane.",
			InputSchema: obj(map[string]any{"repo": propRepo}, "repo"),
			args: func(a map[string]any) ([]string, error) {
				return []string{"tag", "list", "--repo", str(a, "repo")}, nil
			},
		},
		{
			Name:        "decision_list",
			Description: "Questions agents are holding for the person, with their options and recommendations. Bring these to the person; do not answer them yourself.",
			InputSchema: obj(map[string]any{}),
			args:        func(map[string]any) ([]string, error) { return []string{"decision", "list"}, nil },
		},
		{
			Name:        "decision_answer",
			Description: "Record the person's answer to a decision; Shepherd continues the asking agent's conversation with it. Only with the person's own answer, never your guess at it.",
			InputSchema: obj(map[string]any{
				"id":     map[string]any{"type": "integer"},
				"answer": map[string]any{"type": "string", "description": "The person's answer, in their words, or an option number."},
			}, "id", "answer"),
			args: func(a map[string]any) ([]string, error) {
				id, ok := a["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("id must be a number")
				}
				return []string{"decision", "answer", fmt.Sprint(int64(id)), str(a, "answer")}, nil
			},
		},
		{
			Name:        "request_list",
			Description: "Requests between lanes: agents asking each other, through Shepherd, for answers, hand-offs and reviews. needs_routing ones wait for you to say which lane (and agent).",
			InputSchema: obj(map[string]any{}),
			args:        func(map[string]any) ([]string, error) { return []string{"request", "list"}, nil },
		},
		{
			Name:        "request_route",
			Description: "Route a request Shepherd could not route by rule: name the target lane (open it first with lane_open if needed) and optionally the agent. Shepherd starts or continues that agent and carries the reply back to the asker.",
			InputSchema: obj(map[string]any{
				"id":    map[string]any{"type": "integer"},
				"lane":  map[string]any{"type": "string"},
				"agent": map[string]any{"type": "string", "enum": []string{"claude", "copilot", "cursor"}},
			}, "id", "lane"),
			args: func(a map[string]any) ([]string, error) {
				id, ok := a["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("id must be a number")
				}
				out := []string{"request", "route", fmt.Sprint(int64(id)), "--lane", str(a, "lane")}
				if ag := str(a, "agent"); ag != "" {
					out = append(out, "--agent", ag)
				}
				return out, nil
			},
		},
		{
			Name:        "request_close",
			Description: "Close a request between lanes without a reply: one gone stale or no longer needed. A target agent already working on it is left to finish, but its reply is not carried back. Say why; the reason is kept on the request.",
			InputSchema: obj(map[string]any{
				"id":  map[string]any{"type": "integer"},
				"why": map[string]any{"type": "string", "description": "Why it is closed."},
			}, "id"),
			args: func(a map[string]any) ([]string, error) {
				id, ok := a["id"].(float64)
				if !ok {
					return nil, fmt.Errorf("id must be a number")
				}
				out := []string{"request", "close", fmt.Sprint(int64(id))}
				if w := str(a, "why"); w != "" {
					out = append(out, "--why", w)
				}
				return out, nil
			},
		},
		{
			Name:        "fold_gc",
			Description: "List worktrees across the workspace that look finished (merged, branch gone, missing) and lanes whose worktree is gone. Changes nothing.",
			InputSchema: obj(map[string]any{}),
			args:        func(map[string]any) ([]string, error) { return []string{"fold", "gc"}, nil },
		},
	}
}

func str(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

func strs(a map[string]any, k string) ([]string, error) {
	raw, ok := a[k].([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", k)
	}
	var out []string
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", k)
		}
		out = append(out, s)
	}
	return out, nil
}

const workerInstructions = `You are an agent Shepherd started in a lane. These tools are how you reach Shepherd and the person. ask_human: anything that is the person's call (money or pricing, published or user-facing text, deleting or overwriting data, production, a scope or design change with no clear default); give options and your recommendation, then end your turn, and you will be continued with the answer. ask_shepherd: you need another lane (an answer from it, a change outside your scope, a review); then end your turn, and you will be continued with the reply. report: progress, done (what you did and how you checked it) or blocked (why). Everything else, keep working without asking.`

func workerTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "report",
			Description: "Tell Shepherd how the work stands: progress, done (what you did and how you checked it) or blocked (why). Shepherd checks a claim of done against the lane's commits and gate.",
			InputSchema: obj(map[string]any{
				"status":  map[string]any{"type": "string", "enum": []string{"progress", "done", "blocked"}},
				"summary": map[string]any{"type": "string"},
			}, "status", "summary"),
			args: func(a map[string]any) ([]string, error) {
				return []string{"worker", "report", "--status", str(a, "status"), str(a, "summary")}, nil
			},
		},
		{
			Name: "ask_human",
			Description: "Hold a decision for the person: money or pricing, published or user-facing text, deleting or overwriting data, production, or a scope or design change with no clear default. " +
				"Give the options and your recommendation. Then end your turn: Shepherd continues this conversation with the answer.",
			InputSchema: obj(map[string]any{
				"question":       map[string]any{"type": "string"},
				"options":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"recommendation": map[string]any{"type": "string", "description": "Which option you recommend, and why, in a sentence."},
				"why":            map[string]any{"type": "string", "description": "Why this is the person's call rather than yours."},
			}, "question", "recommendation"),
			args: func(a map[string]any) ([]string, error) {
				out := []string{"worker", "ask-human", str(a, "question"), "--recommendation", str(a, "recommendation"), "--why", str(a, "why")}
				if raw, ok := a["options"]; ok && raw != nil {
					opts, err := strs(a, "options")
					if err != nil {
						return nil, err
					}
					for _, o := range opts {
						out = append(out, "--option", o)
					}
				}
				return out, nil
			},
		},
		{
			Name:        "tag_reserve",
			Description: "Reserve the next release version for your lane (major, minor or patch) before you tag. Tag exactly the version you are given, on a commit that includes your merged work.",
			InputSchema: obj(map[string]any{"bump": map[string]any{"type": "string", "enum": []string{"major", "minor", "patch"}}}, "bump"),
			args: func(a map[string]any) ([]string, error) {
				return []string{"worker", "tag-reserve", str(a, "bump")}, nil
			},
		},
		{
			Name: "ask_shepherd",
			Description: "Ask for something from another lane: a question to the agent working there, a hand-off of work outside your scope, or a review. " +
				"Kind person is for the person instead, and becomes a decision for them, as ask_human does. " +
				"Then end your turn: Shepherd continues this conversation with the reply.",
			InputSchema: obj(map[string]any{
				"kind":    map[string]any{"type": "string", "enum": []string{"question", "handoff", "review", "person"}},
				"message": map[string]any{"type": "string", "description": "What you need, as you would ask a colleague."},
				"lane":    map[string]any{"type": "string", "description": "The lane it is for, if you know it."},
			}, "kind", "message"),
			args: func(a map[string]any) ([]string, error) {
				out := []string{"worker", "ask-shepherd", str(a, "message"), "--kind", str(a, "kind")}
				if l := str(a, "lane"); l != "" {
					out = append(out, "--lane", l)
				}
				return out, nil
			},
		},
	}
}

func runMCP(ctx context.Context, env Env, args []string) error {
	fs := flags("mcp", env)
	worker := fs.Bool("worker", false, "serve the worker tools, for an agent Shepherd started")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	if *worker {
		return serveMCP(ctx, env, env.Stdin, env.Stdout, workerTools(), workerInstructions)
	}
	return serveMCP(ctx, env, env.Stdin, env.Stdout, mcpTools(), mcpInstructions)
}

func serveMCP(ctx context.Context, env Env, in io.Reader, out io.Writer, toolset []mcpTool, instructions string) error {
	tools := map[string]mcpTool{}
	var list []mcpTool
	for _, t := range toolset {
		tools[t.Name] = t
		list = append(list, t)
	}
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(msg.ID) == 0 {
			continue // a notification (initialized, cancelled): nothing to answer
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: msg.ID}
		switch msg.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(msg.Params, &p)
			v := mcpVersions[0]
			for _, s := range mcpVersions {
				if s == p.ProtocolVersion {
					v = s
				}
			}
			resp.Result = map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "shepherd", "version": version.Version},
				"instructions":    instructions,
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": list}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				resp.Error = &rpcError{-32602, err.Error()}
				break
			}
			t, ok := tools[p.Name]
			if !ok {
				resp.Error = &rpcError{-32602, "unknown tool " + p.Name}
				break
			}
			resp.Result = callTool(ctx, env, t, p.Arguments)
		default:
			resp.Error = &rpcError{-32601, "method not found: " + msg.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// callTool runs the tool's CLI command with output captured. A failing command is a
// tool error the model can read and act on, not a protocol error.
func callTool(ctx context.Context, env Env, t mcpTool, a map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	text, isErr := "", false
	args, err := t.args(a)
	if err != nil {
		text, isErr = err.Error(), true
	} else {
		var stdout, stderr bytes.Buffer
		cenv := env
		cenv.Stdin, cenv.Stdout, cenv.Stderr = strings.NewReader(""), &stdout, &stderr
		cenv.Interactive = false
		cenv.Client = "mcp"
		if p := str(a, "path"); p != "" && filepath.IsAbs(p) {
			cenv.Cwd = p
		}
		code := Run(ctx, cenv, args)
		text = strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		isErr = code != 0
	}
	if text == "" {
		text = "done"
	}
	// Keep a long agent log from flooding the session: its start and its end matter most.
	if max := 12000; len(text) > max {
		text = text[:2000] + "\n\n[... " + fmt.Sprint(len(text)-max) + " bytes cut ...]\n\n" + text[len(text)-(max-2000):]
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}
