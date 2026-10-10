package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissingFileIsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Daemon.Listen != "127.0.0.1:0" {
		t.Errorf("listen = %q", c.Daemon.Listen)
	}
	p := c.Profile("anything")
	if p.BaseBranch != "main" || p.BranchModel != Trunk {
		t.Errorf("default profile = %+v", p)
	}
	if p.Autonomy.Merge != Human || p.Autonomy.Tag != Human || p.Autonomy.Deploy != Human {
		t.Errorf("default autonomy is not cautious: %+v", p.Autonomy)
	}
	if p.Autonomy.PlanFirst == nil || !*p.Autonomy.PlanFirst {
		t.Error("default plan_first should be true")
	}
}

func TestRepoProfileInherits(t *testing.T) {
	c, err := Parse([]byte(`
defaults:
  gate: make check
repos:
  framework:
    shared_paths: [README.md, ".gitlab-ci.yml"]
    autonomy:
      tag: agent
  product:
    base_branch: dev
    branch_model: promotion
    promotion: [dev, staging, main]
    autonomy:
      plan_first: false
`))
	if err != nil {
		t.Fatal(err)
	}
	fw := c.Profile("framework")
	if fw.Gate != "make check" || fw.BaseBranch != "main" || fw.Autonomy.Tag != Agent || fw.Autonomy.Merge != Human {
		t.Errorf("framework = %+v", fw)
	}
	if len(fw.SharedPaths) != 2 {
		t.Errorf("framework shared paths = %v", fw.SharedPaths)
	}
	pr := c.Profile("product")
	if pr.BaseBranch != "dev" || pr.BranchModel != Promotion || *pr.Autonomy.PlanFirst {
		t.Errorf("product = %+v", pr)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "defaults:\n  base_brnch: dev\n",
		"bad model":         "defaults:\n  branch_model: gitflow\n",
		"promotion too few": "repos:\n  x:\n    branch_model: promotion\n    promotion: [dev]\n",
		"bad autonomy":      "repos:\n  x:\n    autonomy:\n      merge: sometimes\n",
		"public listen":     "daemon:\n  listen: 0.0.0.0:7400\n",
		"bad listen":        "daemon:\n  listen: nope\n",
		"bad push":          "repos:\n  x:\n    autonomy:\n      push: robot\n",
		"shepherd no gate":  "repos:\n  x:\n    autonomy:\n      push: shepherd\n",
		"bad permission":    "defaults:\n  agent:\n    permission_mode: yolo\n",
		"bad log level":     "daemon:\n  log_level: loud\n",
		"log level case":    "daemon:\n  log_level: DEBUG\n",
		"desk model spaces": "desk:\n  model: \"my model\"\n",
		"model agent":       "defaults:\n  agent:\n    model: {gemini: pro}\n",
		"model empty":       "repos:\n  x:\n    agent:\n      model: {claude: \"\"}\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseNamesTheField(t *testing.T) {
	_, err := Parse([]byte("repos:\n  x:\n    autonomy:\n      deploy: robot\n"))
	if err == nil || !strings.Contains(err.Error(), "repos.x.autonomy.deploy") {
		t.Errorf("error does not name the field: %v", err)
	}
}

func TestAgentPowers(t *testing.T) {
	if p := Default().Profile("x"); p.Agent.PermissionMode != PermAuto || p.Autonomy.Push != Human {
		t.Errorf("default agent powers = %+v %+v", p.Agent, p.Autonomy)
	}
	// An agent pushing needs no gate: Shepherd runs none before the agent's push.
	c, err := Parse([]byte(`
defaults:
  agent:
    permission_mode: acceptEdits
repos:
  app:
    agent:
      permission_mode: bypassPermissions
    autonomy:
      push: agent
      merge: agent
`))
	if err != nil {
		t.Fatal(err)
	}
	if app := c.Profile("app"); app.Agent.PermissionMode != PermBypass || app.Autonomy.Push != Agent || app.Autonomy.Merge != Agent {
		t.Errorf("app = %+v", app)
	}
	if other := c.Profile("other"); other.Agent.PermissionMode != PermAcceptEdits || other.Autonomy.Push != Human {
		t.Errorf("other = %+v", other)
	}
}

func TestEmptyFileIsDefault(t *testing.T) {
	if _, err := Parse(nil); err != nil {
		t.Errorf("empty file: %v", err)
	}
}

func TestLogLevel(t *testing.T) {
	if Default().Daemon.LogLevel != "info" {
		t.Errorf("default log level = %q", Default().Daemon.LogLevel)
	}
	for _, l := range LogLevels {
		c, err := Parse([]byte("daemon:\n  log_level: " + l + "\n"))
		if err != nil || c.Daemon.LogLevel != l {
			t.Errorf("log_level %s: %q %v", l, c.Daemon.LogLevel, err)
		}
	}
}

func TestModels(t *testing.T) {
	c, err := Parse([]byte("desk:\n  model: opus\ndefaults:\n  agent:\n    model: {claude: sonnet, cursor: auto}\n" +
		"repos:\n  x:\n    agent:\n      model: {claude: opus}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Desk.Model != "opus" {
		t.Errorf("desk.model = %q", c.Desk.Model)
	}
	if m := c.Profile("x").Agent.Model; m["claude"] != "opus" || m["cursor"] != "auto" {
		t.Errorf("repo models = %v", m)
	}
	if m := c.Profile("y").Agent.Model; m["claude"] != "sonnet" {
		t.Errorf("default models = %v", m)
	}
	if c.Defaults.Agent.Model["claude"] != "sonnet" {
		t.Error("a repo's models changed the defaults'")
	}
}

func TestDeskWake(t *testing.T) {
	if m := Default().Desk.WakeMode(); m != WakeAttached {
		t.Errorf("default wake = %q", m)
	}
	for _, w := range WakeModes {
		if _, err := Parse([]byte("desk:\n  wake: " + w + "\n")); err != nil {
			t.Errorf("wake %s: %v", w, err)
		}
	}
	if _, err := Parse([]byte("desk:\n  wake: sometimes\n")); err == nil || !strings.Contains(err.Error(), "desk.wake") {
		t.Errorf("bad wake: %v", err)
	}
}

func TestOpenCode(t *testing.T) {
	c, err := Parse([]byte("opencode:\n  bin: /opt/oc/bin/opencode\n  endpoint: http://localhost:11434/v1\n" +
		"defaults:\n  agent:\n    model: {opencode: \"local/qwen3-coder:30b\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenCode.Bin != "/opt/oc/bin/opencode" || c.OpenCode.Endpoint != "http://localhost:11434/v1" {
		t.Errorf("opencode = %+v", c.OpenCode)
	}
	if m := c.Profile("x").Agent.Model["opencode"]; m != "local/qwen3-coder:30b" {
		t.Errorf("model = %q", m)
	}
	for name, in := range map[string]string{
		"no scheme":   "opencode:\n  endpoint: localhost:11434\n",
		"credentials": "opencode:\n  endpoint: http://user:pw@localhost:11434/v1\n",
		"not http":    "opencode:\n  endpoint: ftp://localhost/v1\n",
		"blank bin":   "opencode:\n  bin: \" oc\"\n",
		"unknown key": "opencode:\n  port: 1\n",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOpenCodeIdleTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{"": DefaultOpenCodeIdle, "off": 0, "30m": 30 * time.Minute} {
		c, err := Parse([]byte("opencode:\n  idle_timeout: \"" + in + "\"\n"))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := c.OpenCode.Idle(); got != want {
			t.Errorf("idle_timeout %q = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"10s", "soon", "-5m"} {
		if _, err := Parse([]byte("opencode:\n  idle_timeout: " + in + "\n")); err == nil {
			t.Errorf("idle_timeout %q accepted", in)
		}
	}
}

func TestDeskRotation(t *testing.T) {
	d := Default().Desk
	if d.RotateAt() != DefaultRotateTokens || d.RotateUSD() != 0 || d.SummaryCap() != DefaultSummaryChars ||
		d.SummaryTurnCount() != DefaultSummaryTurns || d.ToolOutputCap() != DefaultToolOutputChars || d.SummaryModel != "" {
		t.Errorf("defaults = %+v", d)
	}
	c, err := Parse([]byte("desk:\n  rotate_tokens: 0\n  rotate_cost: 2.5\n  summary_model: haiku\n  summary_chars: 3000\n  summary_turns: 0\n  tool_output_chars: 800\n"))
	if err != nil {
		t.Fatal(err)
	}
	d = c.Desk
	if d.RotateAt() != 0 || d.RotateUSD() != 2.5 || d.SummaryModel != "haiku" || d.SummaryCap() != 3000 ||
		d.SummaryTurnCount() != 0 || d.ToolOutputCap() != 800 {
		t.Errorf("set = %+v", d)
	}
	for in, want := range map[string]string{
		"rotate_tokens: 500":     "desk.rotate_tokens: 500; at least 50000",
		"rotate_tokens: -1":      "desk.rotate_tokens",
		"rotate_cost: 0":         "desk.rotate_cost",
		"rotate_cost: -3":        "desk.rotate_cost",
		"summary_model: \"a b\"": "desk.summary_model",
		"summary_chars: 10":      "desk.summary_chars: 10; at least 1000",
		"summary_turns: 99":      "desk.summary_turns: 99; 0 to 50",
		"summary_turns: -1":      "desk.summary_turns",
		"tool_output_chars: 100": "desk.tool_output_chars: 100; at least 500",
		"rotate_tokens: lots":    "cannot unmarshal",
		"rotate_after_turns: 3":  "rotate_after_turns",
	} {
		if _, err := Parse([]byte("desk:\n  " + in + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}
