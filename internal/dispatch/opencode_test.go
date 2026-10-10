package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// The adapter is hermetic under test: no config.yaml is read and no opencode is run.
func init() {
	openCodeSettings = func() config.OpenCode { return config.OpenCode{Bin: "/fake/opencode"} }
	openCodeProbe = func(string) (string, error) { return openCodeTested, nil }
}

// fakeOpenCode is a stand-in for `opencode run --format json`: it reads the prompt from
// standard input, reports what it was given, and plays the scenario in $MODE.
const fakeOpenCode = `#!/bin/sh
prompt=$(cat)
echo "ARGS: $(printf '%s ' "$@" | tr '\n' ' ')"
echo "STDIN: $(printf '%s' "$prompt" | tr '\n' ' ')"
echo "CONFIG: $OPENCODE_CONFIG_CONTENT"
S=ses_fake0001
ev() { printf '{"type":"%s","sessionID":"%s","part":%s}\n' "$1" "$S" "$2"; }
ev step_start '{"type":"step-start"}'
case "$MODE" in
  noedit)
    ev text '{"type":"text","text":"I will fix it now.\n<tool_call>"}'
    ev step_finish '{"type":"step-finish","reason":"stop","tokens":{"input":100,"output":5},"cost":0}'
    exit 0 ;;
  wait)
    sleep 1
    exit 0 ;;
  hang)
    ev text '{"type":"text","text":"working"}'
    sleep 30
    exit 0 ;;
  apierror)
    printf '{"type":"error","sessionID":"%s","error":{"name":"UnknownError","data":{"message":"Unexpected server error"}}}\n' "$S"
    exit 1 ;;
esac
mkdir -p src && echo "work" >> src/work.txt
ev tool_use '{"type":"tool","tool":"write","state":{"status":"completed","input":{"filePath":"src/work.txt"},"output":"ok"}}'
ev step_finish '{"type":"step-finish","reason":"tool-calls","tokens":{"input":90,"output":20},"cost":0}'
git add src/work.txt && git commit -q -m "agent work"
ev tool_use '{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"git commit -m x"},"output":""}}'
ev text '{"type":"text","text":"Done."}'
ev step_finish '{"type":"step-finish","reason":"stop","tokens":{"input":30,"output":4},"cost":0}'
exit 0
`

func openCodeFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	f := newFixture(t, mode)
	script := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(script, []byte(fakeOpenCode), 0o755); err != nil {
		t.Fatal(err)
	}
	f.runner.lookPath = func(string) (string, error) { return script, nil }
	return f
}

func resetVersions(t *testing.T) {
	t.Helper()
	versionMu.Lock()
	versionSeen = map[string]string{}
	versionMu.Unlock()
	t.Cleanup(func() {
		versionMu.Lock()
		versionSeen = map[string]string{}
		versionMu.Unlock()
	})
}

func logOf(t *testing.T, run store.Run) string {
	t.Helper()
	b, err := os.ReadFile(run.Log)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenCodeRunEndToEnd(t *testing.T) {
	old := openCodeSettings
	openCodeSettings = func() config.OpenCode {
		return config.OpenCode{Bin: "/fake/opencode", Endpoint: "http://localhost:11434/v1"}
	}
	t.Cleanup(func() { openCodeSettings = old })
	f := openCodeFixture(t, "ok")
	const task = "rename the zebra-quokka helper"
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Model: "bench/qwen3-coder:30b", Prompt: task})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	if done.State != store.RunSucceeded || done.Commits != 1 {
		t.Fatalf("run = %s, %d commit(s), error %q", done.State, done.Commits, done.Error)
	}
	if done.Session != "ses_fake0001" {
		t.Errorf("session = %q: it comes from the JSON events", done.Session)
	}
	if done.CostUSD != 0 {
		t.Errorf("cost = %v: a local model is not billed", done.CostUSD)
	}
	log := logOf(t, done)
	if a := argsOf(t, done); strings.Contains(a, "zebra-quokka") || !strings.Contains(a, "run --format json --pure -m bench/qwen3-coder:30b") {
		t.Errorf("args: %s", a)
	}
	if in := stdinOf(t, done); !strings.Contains(in, task) || !strings.Contains(in, "Work only inside") {
		t.Errorf("the prompt is not on standard input: %s", in)
	}
	for _, want := range []string{"→ write ", "120 tokens in, 24 out", "no billed cost", "opencode " + openCodeTested, "Done."} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "WARNING") {
		t.Errorf("a run that committed is warned about:\n%s", log)
	}
	// The config reached the agent in its environment, with the host and the model,
	// and nothing was written to the worktree.
	cfg := configOf(t, log)
	if !strings.Contains(cfg, `"baseURL":"http://localhost:11434/v1"`) || !strings.Contains(cfg, `"qwen3-coder:30b"`) {
		t.Errorf("config: %s", cfg)
	}
	if st := gitStatus(t, f); st != "" {
		t.Errorf("the worktree is not clean: %q", st)
	}
}

func TestOpenCodeNoEditIsNotQuietSuccess(t *testing.T) {
	f := openCodeFixture(t, "noedit")
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "fix it"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	log := logOf(t, done)
	if !strings.Contains(log, "WARNING the model printed a tool call as text and changed nothing") {
		t.Errorf("log:\n%s", log)
	}
	// Exit 0 with nothing changed is a failure the person sees, with the reason.
	if done.State != store.RunFailed || *done.ExitCode != 0 || !strings.Contains(done.Error, "changed nothing") {
		t.Errorf("run = %s (exit %d), error %q", done.State, *done.ExitCode, done.Error)
	}
	if !strings.Contains(log, "# shepherd: opencode exited 0 but changed nothing") {
		t.Errorf("log:\n%s", log)
	}
	if k := feedKind(t, f, "changed nothing"); k != store.FeedRunFailed {
		t.Errorf("feed kind = %q", k)
	}
}

func TestOpenCodeErrorEventFailsTheRun(t *testing.T) {
	f := openCodeFixture(t, "apierror")
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "fix it"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	if done.State != store.RunFailed || done.Session != "ses_fake0001" {
		t.Errorf("run = %s, session %q", done.State, done.Session)
	}
	if log := logOf(t, done); !strings.Contains(log, "ERROR UnknownError: Unexpected server error") {
		t.Errorf("log:\n%s", log)
	}
}

func TestOpenCodeResumesWithSession(t *testing.T) {
	f := openCodeFixture(t, "ok")
	first, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, first.ID)
	second, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "two"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, second.ID)
	if a := argsOf(t, done); !strings.Contains(a, "-s ses_fake0001") {
		t.Errorf("a second run does not resume the session: %s", a)
	}
}

func TestOpenCodeVersionPin(t *testing.T) {
	resetVersions(t)
	old := openCodeProbe
	t.Cleanup(func() { openCodeProbe = old })
	openCodeProbe = func(string) (string, error) { return "2.0.1", nil }
	if _, err := AdapterFor("opencode"); err == nil || !strings.Contains(err.Error(), "2.0.1") || !strings.Contains(err.Error(), "1.x") {
		t.Errorf("a newer major is accepted or unexplained: %v", err)
	}
	resetVersions(t)
	openCodeProbe = func(string) (string, error) { return "1.99.0", nil }
	ad, err := AdapterFor("opencode")
	if err != nil || ad.Bin != "/fake/opencode" {
		t.Errorf("a 1.x opencode: %v, bin %q", err, ad.Bin)
	}
}

func TestOpenCodeBinaryLookup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	if got := openCodeBin(config.OpenCode{}); got != "" {
		t.Errorf("nothing installed, found %q", got)
	}
	std := filepath.Join(home, ".opencode", "bin", "opencode")
	os.MkdirAll(filepath.Dir(std), 0o755)
	os.WriteFile(std, []byte("#!/bin/sh\n"), 0o755)
	if got := openCodeBin(config.OpenCode{}); got != std {
		t.Errorf("standalone install: %q", got)
	}
	onPath := t.TempDir()
	os.WriteFile(filepath.Join(onPath, "opencode"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", onPath)
	if got := openCodeBin(config.OpenCode{}); got != filepath.Join(onPath, "opencode") {
		t.Errorf("PATH comes before the installer's directory: %q", got)
	}
	if got := openCodeBin(config.OpenCode{Bin: "/x/oc"}); got != "/x/oc" {
		t.Errorf("configured: %q", got)
	}
}

func TestOpenCodeConfigPermissions(t *testing.T) {
	var cfg struct {
		Permission map[string]json.RawMessage `json:"permission"`
		Provider   map[string]struct {
			NPM     string `json:"npm"`
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
			Models map[string]json.RawMessage `json:"models"`
		} `json:"provider"`
		MCP map[string]struct {
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	s := openCodeConfig(Opts{Gate: "make check", Model: "bench/qwen3-coder:30b", Worker: "/bin/shepherd"},
		config.OpenCode{Endpoint: "http://localhost:11434/v1"})
	if err := json.Unmarshal([]byte(s), &cfg); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	for _, k := range []string{"question", "webfetch", "websearch", "external_directory"} {
		if string(cfg.Permission[k]) != `"deny"` {
			t.Errorf("%s = %s, want deny", k, cfg.Permission[k])
		}
	}
	if string(cfg.Permission["edit"]) != `"allow"` {
		t.Errorf("edit = %s", cfg.Permission["edit"])
	}
	// "*": deny comes first and git push last: the rules are last match wins.
	bash := string(cfg.Permission["bash"])
	if !strings.HasPrefix(bash, `{"*":"deny","go test*":"allow"`) || !strings.HasSuffix(bash, `"git push*":"deny"}`) {
		t.Errorf("bash rules: %s", bash)
	}
	for _, want := range []string{`"make check*":"allow"`, `"git commit*":"allow"`, `"cat *":"allow"`} {
		if !strings.Contains(bash, want) {
			t.Errorf("bash rules lack %s: %s", want, bash)
		}
	}
	p := cfg.Provider["bench"]
	if p.NPM != "@ai-sdk/openai-compatible" || p.Options.BaseURL != "http://localhost:11434/v1" || p.Models["qwen3-coder:30b"] == nil {
		t.Errorf("provider: %+v", cfg.Provider)
	}
	if m := cfg.MCP["shepherd"]; len(m.Command) != 3 || m.Command[0] != "/bin/shepherd" {
		t.Errorf("worker tools: %+v", cfg.MCP)
	}
	if string(cfg.Permission["shepherd_*"]) != `"allow"` {
		t.Errorf("worker tools are not allowed: %v", cfg.Permission)
	}
	// An agent that may push, and merge, gets those commands; with no endpoint the
	// person's own provider is left alone.
	s = openCodeConfig(Opts{Push: true, Merge: true}, config.OpenCode{})
	if !strings.Contains(s, `"git push*":"allow"`) || !strings.Contains(s, `"glab mr merge*":"allow"`) || strings.Contains(s, `"provider"`) || strings.Contains(s, `"mcp"`) {
		t.Errorf("config: %s", s)
	}
}

func TestOpenCodeModelNaming(t *testing.T) {
	for _, c := range []struct {
		model string
		host  bool
		want  string
	}{
		{"", true, ""},
		{"qwen3-coder:30b", true, "local/qwen3-coder:30b"},
		{"bench/qwen3-coder:30b", true, "bench/qwen3-coder:30b"},
		{"qwen3-coder:30b", false, "qwen3-coder:30b"},
	} {
		if got := openCodeModel(c.model, c.host); got != c.want {
			t.Errorf("openCodeModel(%q, %v) = %q, want %q", c.model, c.host, got, c.want)
		}
	}
}

func TestOpenCodeOutputEvents(t *testing.T) {
	o := openCodeOutput(`{"type":"step_finish","sessionID":"s1","part":{"reason":"tool-calls","cost":0.5,"tokens":{"input":10,"output":2}}}`)
	if o.USD != 0.5 || o.Show != "" {
		t.Errorf("a step's cost is read and a mid-run step is quiet: %+v", o)
	}
	if o := openCodeOutput("not json at all"); o.Show != "not json at all" {
		t.Errorf("a plain line is kept: %+v", o)
	}
	o = openCodeOutput(`{"type":"tool_use","sessionID":"s2","part":{"tool":"bash","state":{"status":"error","input":{"command":"rm x"},"error":"denied by a rule"}}}`)
	if !strings.Contains(o.Show, "bash") || !strings.Contains(o.Show, "denied by a rule") {
		t.Errorf("a refused tool call: %+v", o)
	}
	if id := openCodeSession(`{"type":"text","sessionID":"ses_abc","part":{}}`); id != "ses_abc" {
		t.Errorf("session = %q", id)
	}
	if id := openCodeSession("plain"); id != "" {
		t.Errorf("session from a plain line = %q", id)
	}
}

func TestOpenCodeAttachResumesSession(t *testing.T) {
	ad, err := AdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ad.Attach("ses_x", "/w"), " "); got != "-s ses_x" {
		t.Errorf("attach = %q", got)
	}
}

// configOf is the CONFIG line the fake agent printed.
func configOf(t *testing.T, log string) string {
	t.Helper()
	for _, l := range strings.Split(log, "\n") {
		if rest, ok := strings.CutPrefix(l, "CONFIG: "); ok {
			return rest
		}
	}
	t.Fatalf("no CONFIG line in:\n%s", log)
	return ""
}

func gitStatus(t *testing.T, f *fixture) string {
	t.Helper()
	lane, err := f.st.Lane(context.Background(), f.lane.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", lane.Worktree, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestOpenCodeNoChangeIsNotFailedWhenTheRunAsked(t *testing.T) {
	f := openCodeFixture(t, "wait")
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "ask first"})
	if err != nil {
		t.Fatal(err)
	}
	// A run that reported through Shepherd's tools did something, even if it changed no file.
	if _, err := f.runner.Record(context.Background(), store.Event{RunID: run.ID, Kind: EventReport, Status: "blocked", Text: "need the schema"}); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(t, run.ID); done.State != store.RunSucceeded {
		t.Errorf("run = %s, error %q", done.State, done.Error)
	}
}

func TestNoChangeStaysSucceededForOtherAgents(t *testing.T) {
	// Another agent's no-change run stays succeeded: the verdict is the adapter's.
	f := newFixture(t, "quick")
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(t, run.ID); done.State != store.RunSucceeded || done.Commits != 0 {
		t.Errorf("claude run = %s, %d commit(s), error %q", done.State, done.Commits, done.Error)
	}
}

func TestOpenCodeIdleRunIsStoppedAndFailed(t *testing.T) {
	old := openCodeIdle
	openCodeIdle = func() time.Duration { return 400 * time.Millisecond }
	t.Cleanup(func() { openCodeIdle = old })
	f := openCodeFixture(t, "hang")
	start := time.Now()
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "fix it"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	if time.Since(start) > 15*time.Second {
		t.Errorf("took %s: not stopped for being idle", time.Since(start))
	}
	if done.State != store.RunFailed || !strings.Contains(done.Error, "printed nothing for 400ms") {
		t.Errorf("run = %s, error %q", done.State, done.Error)
	}
	if k := feedKind(t, f, "printed nothing"); k != store.FeedRunFailed {
		t.Errorf("feed kind = %q", k)
	}
}

func TestOpenCodeSilenceUnderTheLimitIsNotIdle(t *testing.T) {
	// A run that was quiet for less than the limit is not idle.
	old := openCodeIdle
	openCodeIdle = func() time.Duration { return 2500 * time.Millisecond }
	t.Cleanup(func() { openCodeIdle = old })
	f := openCodeFixture(t, "wait") // prints, then sleeps 1s: under the limit
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Record(context.Background(), store.Event{RunID: run.ID, Kind: EventReport, Status: "progress", Text: "p"})
	if done := f.wait(t, run.ID); done.State != store.RunSucceeded {
		t.Errorf("run = %s, error %q", done.State, done.Error)
	}
}

func TestOpenCodeConfigIsInTheRunsEnvironmentOnly(t *testing.T) {
	t.Setenv(openCodeEnvVar, "") // so the check below is of this test's runs
	os.Unsetenv(openCodeEnvVar)
	f := openCodeFixture(t, "ok")
	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "opencode", Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(t, run.ID)
	if !strings.Contains(configOf(t, logOf(t, done)), `"permission"`) {
		t.Errorf("the run did not get its config:\n%s", logOf(t, done))
	}
	if v, ok := os.LookupEnv(openCodeEnvVar); ok {
		t.Errorf("%s is set in the daemon after Start: %.40s", openCodeEnvVar, v)
	}
	// Another agent started afterwards does not see it.
	printer := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(printer, []byte("#!/bin/sh\ncat >/dev/null\necho \"OC=[$OPENCODE_CONFIG_CONTENT]\"\n"), 0o755)
	f.runner.lookPath = func(string) (string, error) { return printer, nil }
	other, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if log := logOf(t, f.wait(t, other.ID)); !strings.Contains(log, "OC=[]") {
		t.Errorf("another agent inherited the config:\n%s", log)
	}
}
