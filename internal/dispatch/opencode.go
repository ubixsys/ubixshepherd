package dispatch

// The OpenCode adapter: `opencode run` headless, for small mechanical edits on a local
// model. How it differs from the other adapters:
//
//   - The prompt goes on standard input (`opencode run` with no message argument reads
//     it), never on the command line and never in a file.
//   - OpenCode has no config-path flag, and the person's own config and the repo's
//     opencode.json must not carry Shepherd's permissions. The run's config (provider,
//     permissions, worker tools) goes in OPENCODE_CONFIG_CONTENT, which OpenCode layers
//     over the files it finds. Nothing is written to the worktree or to the person's
//     home. See openCodeEnv for how the runner's environment gets it.
//   - The session id is in every JSON event, so it is read from the output; a run is
//     resumed with -s.
//   - The version is pinned to the major the adapter was tested with, checked each time
//     the adapter is looked up (AdapterFor), so every run, a resumed one too, is refused
//     with a message when the installed opencode is another major.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/paths"
)

const (
	// openCodeMajor is the major version the adapter was written and checked against.
	openCodeMajor = 1
	// openCodeTested is the full version of that check, for messages.
	openCodeTested = "1.18.35"
	// openCodeEnvVar carries a run's config; OpenCode reads it as inline JSON.
	openCodeEnvVar = "OPENCODE_CONFIG_CONTENT"
	// openCodeLocal names the provider a model with none is run under when the config
	// gives an endpoint.
	openCodeLocal = "local"
)

// openCodeSettings is the machine's opencode configuration. It reads config.yaml each
// time, so a change applies to the next run; tests replace it.
var openCodeSettings = func() config.OpenCode {
	home, err := paths.Home()
	if err != nil {
		return config.OpenCode{}
	}
	c, err := config.Load(paths.Layout{Home: home}.Config())
	if err != nil {
		return config.OpenCode{}
	}
	return c.OpenCode
}

// openCodeBin finds the executable: opencode.bin, else PATH, else the standalone
// installer's ~/.opencode/bin. "" when there is none.
func openCodeBin(s config.OpenCode) string {
	if s.Bin != "" {
		return s.Bin
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".opencode", "bin", "opencode")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

var (
	openCodeVersionRE = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)
	versionMu         sync.Mutex
	versionSeen       = map[string]string{} // path, size and mtime -> version
)

// openCodeProbe asks the executable its version; tests replace it.
var openCodeProbe = func(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("opencode --version (%s): %w", bin, err)
	}
	m := openCodeVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("opencode --version (%s) printed %q, which is not a version", bin, strings.TrimSpace(string(out)))
	}
	return m[0], nil
}

// openCodeVersion runs `bin --version`, remembering the answer for as long as the file
// is unchanged.
func openCodeVersion(bin string) (string, error) {
	key := bin
	if st, err := os.Stat(bin); err == nil {
		key = fmt.Sprintf("%s|%d|%d", bin, st.Size(), st.ModTime().UnixNano())
	}
	versionMu.Lock()
	v, ok := versionSeen[key]
	versionMu.Unlock()
	if ok {
		return v, nil
	}
	v, err := openCodeProbe(bin)
	if err != nil {
		return "", err
	}
	versionMu.Lock()
	versionSeen[key] = v
	versionMu.Unlock()
	return v, nil
}

// checkOpenCode refuses an opencode of another major version than the one tested.
func checkOpenCode(bin string) (string, error) {
	v, err := openCodeVersion(bin)
	if err != nil {
		return "", err
	}
	var major int
	fmt.Sscan(v, &major) // the leading digits; the version matched the pattern above
	if major != openCodeMajor {
		return v, fmt.Errorf("opencode %s (%s) is not supported: Shepherd's adapter was tested with %d.x (%s), and a newer major may change the flags and the JSON events it relies on. Install a %d.x opencode, or point opencode.bin at one",
			v, bin, openCodeMajor, openCodeTested, openCodeMajor)
	}
	return v, nil
}

// openCodeFor completes the adapter for this machine: the executable it found, which the
// runner then looks up as given, and the version check. With no opencode installed it is
// left as is, so the runner says opencode is not installed.
func openCodeFor(ad Adapter) (Adapter, error) {
	bin := openCodeBin(openCodeSettings())
	if bin == "" {
		return ad, nil
	}
	if _, err := checkOpenCode(bin); err != nil {
		return ad, err
	}
	ad.Bin = bin
	return ad, nil
}

// openCodeArgs is the headless command line, without the prompt.
func openCodeArgs(o Opts) []string {
	// --pure: no external plugins. No --auto: the permission block in the run's config
	// says what is allowed, and anything it does not allow is refused rather than
	// approved by default.
	a := []string{"run", "--format", "json", "--pure"}
	if o.Resume {
		a = append(a, "-s", o.Session)
	}
	s := openCodeSettings()
	if m := openCodeModel(o.Model, s.Endpoint != ""); m != "" {
		a = append(a, "-m", m)
	}
	openCodeEnv(o, s)
	return a
}

// openCodeModel is the -m value: provider/model. With an endpoint configured the
// provider is Shepherd's, and a bare model name runs under "local".
func openCodeModel(model string, haveEndpoint bool) string {
	if model == "" {
		return ""
	}
	if haveEndpoint && !strings.Contains(model, "/") {
		return openCodeLocal + "/" + model
	}
	return model
}

// envMu orders openCodeEnv's change to the process environment.
var envMu sync.Mutex

// openCodeEnv puts the run's config in OPENCODE_CONFIG_CONTENT in this process's
// environment, because Adapter.Args is all the runner gives an adapter and the child
// inherits the environment. That is safe only because Runner.Start builds the command
// line and reads os.Environ() for the child under the same lock (r.mu): the value
// cannot change between the two, and each opencode run sets its own. The cost is that
// the variable stays in the daemon's environment, where only another opencode run
// reads it. A per-run Env hook on Adapter would replace this.
func openCodeEnv(o Opts, s config.OpenCode) {
	envMu.Lock()
	defer envMu.Unlock()
	os.Setenv(openCodeEnvVar, openCodeConfig(o, s))
}

// kv is one JSON object member; objects are built in order because OpenCode's
// permission rules are evaluated last match wins.
type kv struct {
	k string
	v any
}

type obj []kv

func (o obj) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(e.k)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// openCodeConfig is the run's opencode.json. The permissions are the evaluated set for
// mechanical edits: files may be edited, shell commands are denied except the repo's
// build and test tools, reading git state and committing, and everything that reaches
// beyond the worktree is denied. Pushing is denied unless the repo lets agents push.
func openCodeConfig(o Opts, s config.OpenCode) string {
	bash := obj{{"*", "deny"}}
	allow := []string{"go test*", "go build*", "go vet*", "gofmt*", "git diff*", "git status*",
		"git log*", "git show*", "git add*", "git commit*", "ls*", "cat *"}
	if g := strings.TrimSpace(o.Gate); g != "" {
		allow = append(allow, g+"*")
	}
	if o.Merge {
		allow = append(allow, "glab mr view*", "glab mr merge*")
	}
	for _, p := range allow {
		bash = append(bash, kv{p, "allow"})
	}
	if o.Push {
		bash = append(bash, kv{"git push*", "allow"})
	} else {
		bash = append(bash, kv{"git push*", "deny"})
	}
	perm := obj{
		{"bash", bash},
		{"edit", "allow"},
		{"question", "deny"},
		{"webfetch", "deny"},
		{"websearch", "deny"},
		{"external_directory", "deny"},
		{"doom_loop", "deny"},
	}
	cfg := obj{
		// A run never upgrades or shares itself.
		{"autoupdate", false},
		{"share", "disabled"},
		{"permission", perm},
	}
	if s.Endpoint != "" {
		provider, model := openCodeLocal, o.Model
		if p, m, ok := strings.Cut(o.Model, "/"); ok {
			provider, model = p, m
		}
		prov := obj{
			{"npm", "@ai-sdk/openai-compatible"},
			{"name", provider},
			{"options", obj{
				{"baseURL", s.Endpoint},
				// A model host that stops answering ends the request instead of
				// leaving the run waiting.
				{"timeout", 900000},
				{"chunkTimeout", 120000},
			}},
		}
		if model != "" {
			prov = append(prov, kv{"models", obj{{model, obj{
				{"name", model},
				{"limit", obj{{"context", 32768}, {"output", 8192}}},
			}}}})
		}
		cfg = append(cfg, kv{"provider", obj{{provider, prov}}})
	}
	if o.Worker != "" {
		cfg = append(cfg, kv{"mcp", obj{{"shepherd", obj{
			{"type", "local"},
			{"command", []string{o.Worker, "mcp", "--worker"}},
			{"enabled", true},
		}}}})
		perm = append(perm, kv{"shepherd_*", "allow"})
		cfg[2].v = perm
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// openCodeEvent is one line of `opencode run --format json`.
type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Error     struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	Part struct {
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		Tool   string  `json:"tool"`
		Reason string  `json:"reason"`
		Cost   float64 `json:"cost"`
		Tokens struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		} `json:"tokens"`
		State struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Error  string          `json:"error"`
		} `json:"state"`
	} `json:"part"`
}

// openCodeRun is what is known of a session's current run while its events stream.
type openCodeRun struct {
	changed bool // a completed edit, write or commit
	printed bool // the model wrote a tool call as text
	in, out int
}

var (
	runsMu   sync.Mutex
	openRuns = map[string]*openCodeRun{} // by session id
)

// editTools are OpenCode's tools that change files.
var editTools = map[string]bool{"edit": true, "write": true, "patch": true, "multiedit": true, "apply_patch": true}

// printedToolCall matches the markup a model writes when it prints a tool call as text
// instead of making it.
var printedToolCall = regexp.MustCompile(`<tool_call>|<function=|"name"\s*:\s*"(edit|write|bash|read)"\s*,\s*"arguments"`)

// openCodeOutput turns one line of the run's output into the run log's line and cost.
// Cost is 0 on a local model: OpenCode prices a model it has no rates for at 0, and
// the runner records that rather than inventing one. A final step with no edit or
// commit in the run is called out in the log, since the process exits 0 either way.
func openCodeOutput(line string) Output {
	var e openCodeEvent
	if json.Unmarshal([]byte(line), &e) != nil || e.Type == "" {
		return Output{Show: line}
	}
	switch e.Type {
	case "text":
		run := openRunFor(e.SessionID)
		if printedToolCall.MatchString(e.Part.Text) {
			run.printed = true
		}
		return Output{Show: strings.TrimSpace(e.Part.Text)}
	case "tool_use":
		run := openRunFor(e.SessionID)
		st := e.Part.State
		if st.Status == "completed" && openCodeChanged(e.Part.Tool, st.Input) {
			run.changed = true
		}
		in := clip(string(st.Input), 160)
		if st.Status == "error" {
			return Output{Show: fmt.Sprintf("→ %s %s: %s", e.Part.Tool, in, clip(st.Error, 200))}
		}
		return Output{Show: fmt.Sprintf("→ %s %s", e.Part.Tool, in)}
	case "step_finish":
		run := openRunFor(e.SessionID)
		run.in += e.Part.Tokens.Input
		run.out += e.Part.Tokens.Output
		o := Output{USD: e.Part.Cost}
		if e.Part.Reason == "tool-calls" {
			return o
		}
		o.Show = openCodeEnd(e.SessionID, run, e.Part.Reason)
		return o
	case "error":
		msg := e.Error.Data.Message
		if msg == "" {
			msg = e.Error.Name
		}
		return Output{Show: "ERROR " + e.Error.Name + ": " + clip(msg, 300)}
	}
	return Output{}
}

// openCodeEnd closes a session's run in the log and forgets its state.
func openCodeEnd(session string, run *openCodeRun, reason string) string {
	runsMu.Lock()
	delete(openRuns, session)
	runsMu.Unlock()
	ver := ""
	if bin := openCodeBin(openCodeSettings()); bin != "" {
		if v, err := openCodeVersion(bin); err == nil {
			ver = ", opencode " + v
		}
	}
	s := fmt.Sprintf("# opencode ended (%s): %d tokens in, %d out, no billed cost (a local model has no rates)%s", reason, run.in, run.out, ver)
	switch {
	case run.printed && !run.changed:
		s += "\n# shepherd: WARNING the model printed a tool call as text and changed nothing; exit 0 here is not a success"
	case !run.changed:
		s += "\n# shepherd: WARNING opencode ended its turn without editing a file or committing; exit 0 here is not a success"
	}
	return s
}

func openRunFor(session string) *openCodeRun {
	runsMu.Lock()
	defer runsMu.Unlock()
	r := openRuns[session]
	if r == nil {
		r = &openCodeRun{}
		openRuns[session] = r
	}
	return r
}

// openCodeChanged says a completed tool call changed the lane: a file tool, a commit,
// or gofmt -w.
func openCodeChanged(tool string, input json.RawMessage) bool {
	if editTools[tool] {
		return true
	}
	if tool != "bash" {
		return false
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	c := in.Command
	return strings.Contains(c, "git commit") || (strings.Contains(c, "gofmt") && strings.Contains(c, " -w"))
}

// openCodeSession reads the session id every JSON event carries.
func openCodeSession(line string) string {
	if !strings.HasPrefix(line, "{") {
		return ""
	}
	var e struct {
		SessionID string `json:"sessionID"`
	}
	if json.Unmarshal([]byte(line), &e) != nil {
		return ""
	}
	return e.SessionID
}
