// Package config loads Shepherd's machine configuration: how the daemon listens, and the
// repo profiles that say how each repo works (base branch, branch model, gate, shared
// paths, autonomy).
//
// A missing file is not an error: every field has a default, and the defaults are
// cautious (a human merges, tags and deploys; agents plan first).
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the whole file.
type Config struct {
	Daemon   Daemon             `yaml:"daemon" json:"daemon"`
	Desk     Desk               `yaml:"desk" json:"desk"`
	Defaults Profile            `yaml:"defaults" json:"defaults"`
	Repos    map[string]Profile `yaml:"repos" json:"repos,omitempty"`
}

// Daemon holds the daemon's own settings.
type Daemon struct {
	// Listen is a loopback host:port. Port 0 picks a free port; clients find it in the
	// daemon's runtime file.
	Listen string `yaml:"listen" json:"listen"`
	// MaxRuns is how many agents may run at once on this machine.
	MaxRuns int `yaml:"max_runs" json:"max_runs"`
	// Poll is how often open lanes' merge requests are checked: "60s", "2m"; "off" stops it.
	Poll string `yaml:"poll" json:"poll"`
	// Budget is the daily spend, in dollars, past which Shepherd holds the runs it would
	// start on its own (fixes, routed requests); 0 means no cap. It warns at 80%.
	Budget *float64 `yaml:"budget" json:"budget"`
	// CreditUSD prices one Copilot credit, which Copilot reports instead of dollars.
	CreditUSD *float64 `yaml:"credit_usd" json:"credit_usd"`
	// LogLevel is what the daemon logs: debug, info, warn or error (one of LogLevels).
	LogLevel string `yaml:"log_level" json:"log_level"`
}

// Desk holds the front desk's settings (shepherd chat).
type Desk struct {
	// Model is the desk's model when the chat is given no --model and none was set
	// with /model; "" leaves it to Claude Code's default.
	Model string `yaml:"model" json:"model,omitempty"`
}

// Agents are the agent CLIs Shepherd can start.
var Agents = []string{"claude", "copilot", "cursor"}

// LogLevels are daemon.log_level's choices, quietest last.
var LogLevels = []string{"debug", "info", "warn", "error"}

// Branch models.
const (
	// Trunk: work branches off the base branch and lands on it; releases are tags.
	Trunk = "trunk"
	// Promotion: work lands on the first branch and is promoted through the rest
	// (for example dev, then staging, then main).
	Promotion = "promotion"
)

// Tag modes.
const (
	TagsFree     = "free"
	TagsReserved = "reserved"
)

// Who may take an action.
const (
	Human = "human"
	Agent = "agent"
	// Shepherd: Shepherd itself, deterministically (for push: after the gate passes).
	Shepherd = "shepherd"
)

// Claude Code permission modes for the agents Shepherd starts.
const (
	// PermAuto runs as the person's own sessions do: their Claude Code auto mode and
	// settings decide what needs no prompt.
	PermAuto = "auto"
	// PermAcceptEdits and PermDefault allow edits (or not) and Shepherd's own short list
	// of commands; anything else is refused, since nobody can approve it headless.
	PermAcceptEdits = "acceptEdits"
	PermDefault     = "default"
	// PermBypass skips permission checks altogether.
	PermBypass = "bypassPermissions"
)

// PermissionModes are the values agent.permission_mode takes.
var PermissionModes = []string{PermAuto, PermAcceptEdits, PermDefault, PermBypass}

// Profile says how a repo works. In Repos, an empty field inherits from Defaults.
type Profile struct {
	BaseBranch  string   `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	BranchModel string   `yaml:"branch_model,omitempty" json:"branch_model,omitempty"`
	Promotion   []string `yaml:"promotion,omitempty" json:"promotion,omitempty"`
	// Gate is the command that must pass in a worktree before work counts as green.
	Gate string `yaml:"gate,omitempty" json:"gate,omitempty"`
	// SharedPaths are globs that more than one lane may want; touching one takes a lease.
	SharedPaths []string `yaml:"shared_paths,omitempty" json:"shared_paths,omitempty"`
	// WorktreeRoot is where lane worktrees go. Empty means <workspace>/<repo>-worktrees.
	WorktreeRoot string `yaml:"worktree_root,omitempty" json:"worktree_root,omitempty"`
	// Setup runs in each new lane's worktree, with SHEPHERD_REPO set to the repo's main
	// checkout: what a fresh worktree needs before its gate can run (an .env, vendor/).
	Setup string `yaml:"setup,omitempty" json:"setup,omitempty"`
	// CoordFile is a coordination file (AGENTS-COORD.md) Shepherd keeps a generated view
	// of its lanes in, between markers, during a cutover; "" for none.
	CoordFile string `yaml:"coord_file,omitempty" json:"coord_file,omitempty"`
	// TagPrefix comes before a release version in the repo's tags; "v" by default.
	TagPrefix string `yaml:"tag_prefix,omitempty" json:"tag_prefix,omitempty"`
	// Tags is "free" (the default) or "reserved": every release tag pushed from the repo
	// must be reserved first (shepherd tag reserve), and contain its lane's merge.
	Tags string `yaml:"tags,omitempty" json:"tags,omitempty"`
	// Brief is added to every agent's brief in the repo: its own rules.
	Brief string `yaml:"brief,omitempty" json:"brief,omitempty"`
	// Forbid are regular expressions commit messages must not match before Shepherd
	// pushes ("(?i)co-authored-by"); a match goes back to the agent to amend.
	Forbid   []string `yaml:"forbid,omitempty" json:"forbid,omitempty"`
	Autonomy Autonomy `yaml:"autonomy,omitempty" json:"autonomy"`
	// Agent says how the agents Shepherd starts in the repo run.
	Agent AgentOpts `yaml:"agent,omitempty" json:"agent"`
	// Follows are the repos this one depends on: a release of one opens a lane here and
	// hands it to an agent (a framework's tag, the host's version bump).
	Follows []Follow `yaml:"follows,omitempty" json:"follows,omitempty"`
}

// When a follow acts on a release.
const (
	// Published: the release tag's pipeline passed, so what it publishes is there.
	Published = "published"
	// Tagged: the tag alone, for a repo with no pipeline on its tags.
	Tagged = "tagged"
)

// Follow is one repo a repo follows, and what to do here when it releases. Lane and
// Task may use {repo}, {tag}, {version}, {major}, {minor}, {patch} and {previous}.
type Follow struct {
	// Repo is the followed repo's name in the workspace.
	Repo string `yaml:"repo" json:"repo"`
	// MinBump is the smallest release worth a lane: patch, minor (the default) or major.
	MinBump string `yaml:"min_bump,omitempty" json:"min_bump,omitempty"`
	// After is published (the default) or tagged.
	After string `yaml:"after,omitempty" json:"after,omitempty"`
	// Lane names the lane; "chore/{repo}-{version}" by default.
	Lane  string   `yaml:"lane,omitempty" json:"lane,omitempty"`
	Scope []string `yaml:"scope" json:"scope"`
	// Agent does the work: claude (the default), copilot or cursor.
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Task is what the agent is asked to do.
	Task string `yaml:"task" json:"task"`
}

// Effective fills a follow's defaults.
func (f Follow) Effective() Follow {
	if f.MinBump == "" {
		f.MinBump = "minor"
	}
	if f.After == "" {
		f.After = Published
	}
	if f.Lane == "" {
		f.Lane = "chore/{repo}-{version}"
	}
	if f.Agent == "" {
		f.Agent = "claude"
	}
	return f
}

// AgentOpts says how the agents Shepherd starts run.
type AgentOpts struct {
	// PermissionMode is the Claude Code permission mode of a run: auto (the default),
	// acceptEdits, default or bypassPermissions. Copilot takes auto and bypassPermissions
	// as allowing every tool; Cursor always runs with --force.
	PermissionMode string `yaml:"permission_mode,omitempty" json:"permission_mode,omitempty"`
	// Model is the model each agent runs on when lane run names none, by agent
	// (claude, copilot, cursor); an agent left out uses its CLI's own default. A repo's
	// entries replace the defaults' for the same agent.
	Model map[string]string `yaml:"model,omitempty" json:"model,omitempty"`
}

// badModel says a model name is not one: blank around it, or with spaces inside.
func badModel(m string) bool {
	return m != strings.TrimSpace(m) || strings.ContainsAny(m, " \t\n")
}

// Autonomy records what agents may do unasked in a repo.
type Autonomy struct {
	// Merge: "agent" lets an agent arm merge-when-pipeline-succeeds on its own lane's
	// merge request; the forge's approvals, pipelines and threads still gate it.
	Merge  string `yaml:"merge,omitempty" json:"merge,omitempty"`
	Tag    string `yaml:"tag,omitempty" json:"tag,omitempty"`
	Deploy string `yaml:"deploy,omitempty" json:"deploy,omitempty"`
	// Push: who pushes a lane's branch and opens its merge request. "shepherd" lets
	// Shepherd do it after an agent's run, once the repo's gate passes in the lane;
	// "agent" lets the agent push its own lane's branch during its run.
	Push string `yaml:"push,omitempty" json:"push,omitempty"`
	// PlanFirst means an agent proposes a plan and waits before changing anything.
	PlanFirst *bool `yaml:"plan_first,omitempty" json:"plan_first,omitempty"`
}

// Default is the configuration with no file.
func Default() Config {
	yes := true
	return Config{
		Daemon: Daemon{Listen: "127.0.0.1:0", MaxRuns: 4, Poll: "60s", Budget: ptr(20.0), CreditUSD: ptr(0.04), LogLevel: "info"},
		Defaults: Profile{
			BaseBranch:  "main",
			TagPrefix:   "v",
			Tags:        TagsFree,
			BranchModel: Trunk,
			Autonomy:    Autonomy{Merge: Human, Tag: Human, Deploy: Human, Push: Human, PlanFirst: &yes},
			Agent:       AgentOpts{PermissionMode: PermAuto},
		},
	}
}

func ptr(f float64) *float64 { return &f }

// Load reads path over the defaults. A missing file gives Default().
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	c, err := Parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads YAML over the defaults and validates the result. Unknown keys are errors,
// so a typo cannot silently fall back to a default.
func Parse(b []byte) (Config, error) {
	c := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var file Config
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, err
	}
	if file.Daemon.Listen != "" {
		c.Daemon.Listen = file.Daemon.Listen
	}
	if file.Daemon.MaxRuns != 0 {
		c.Daemon.MaxRuns = file.Daemon.MaxRuns
	}
	if file.Daemon.Poll != "" {
		c.Daemon.Poll = file.Daemon.Poll
	}
	if file.Daemon.Budget != nil {
		c.Daemon.Budget = file.Daemon.Budget
	}
	if file.Daemon.CreditUSD != nil {
		c.Daemon.CreditUSD = file.Daemon.CreditUSD
	}
	if file.Daemon.LogLevel != "" {
		c.Daemon.LogLevel = file.Daemon.LogLevel
	}
	c.Desk = file.Desk
	c.Defaults = merge(c.Defaults, file.Defaults)
	c.Repos = file.Repos
	return c, c.Validate()
}

// Validate checks every value against its closed set.
func (c Config) Validate() error {
	var errs []error
	host, _, err := net.SplitHostPort(c.Daemon.Listen)
	if err != nil {
		errs = append(errs, fmt.Errorf("daemon.listen: %w", err))
	} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		// Authentication is a local token today; logins come with the hosted service.
		errs = append(errs, fmt.Errorf("daemon.listen: %q is not a loopback address", c.Daemon.Listen))
	}
	if c.Daemon.Poll != "off" {
		if d, err := time.ParseDuration(c.Daemon.Poll); err != nil || d < 10*time.Second {
			errs = append(errs, fmt.Errorf("daemon.poll: %q; a duration of at least 10s, or off", c.Daemon.Poll))
		}
	}
	if *c.Daemon.Budget < 0 || *c.Daemon.CreditUSD < 0 {
		errs = append(errs, fmt.Errorf("daemon.budget and daemon.credit_usd cannot be negative"))
	}
	if !slices.Contains(LogLevels, c.Daemon.LogLevel) {
		errs = append(errs, fmt.Errorf("daemon.log_level: %q is not %s", c.Daemon.LogLevel, strings.Join(LogLevels, ", ")))
	}
	if badModel(c.Desk.Model) {
		errs = append(errs, fmt.Errorf("desk.model: %q is not a model name", c.Desk.Model))
	}
	if c.Daemon.MaxRuns < 1 {
		errs = append(errs, fmt.Errorf("daemon.max_runs: %d; at least 1", c.Daemon.MaxRuns))
	}
	errs = append(errs, c.Defaults.validate("defaults")...)
	for name := range c.Repos {
		errs = append(errs, c.Profile(name).validate("repos."+name)...)
	}
	return errors.Join(errs...)
}

// Profile returns the effective profile for a repo: its entry in Repos over Defaults.
func (c Config) Profile(repo string) Profile {
	return merge(c.Defaults, c.Repos[repo])
}

func (p Profile) validate(at string) []error {
	var errs []error
	if p.BaseBranch == "" {
		errs = append(errs, fmt.Errorf("%s.base_branch: empty", at))
	}
	switch p.BranchModel {
	case Trunk:
	case Promotion:
		if len(p.Promotion) < 2 {
			errs = append(errs, fmt.Errorf("%s.promotion: the promotion model needs at least two branches", at))
		}
	default:
		errs = append(errs, fmt.Errorf("%s.branch_model: %q is not %s or %s", at, p.BranchModel, Trunk, Promotion))
	}
	for field, v := range map[string]string{"merge": p.Autonomy.Merge, "tag": p.Autonomy.Tag, "deploy": p.Autonomy.Deploy} {
		if v != Human && v != Agent {
			errs = append(errs, fmt.Errorf("%s.autonomy.%s: %q is not %s or %s", at, field, v, Human, Agent))
		}
	}
	for _, f := range p.Forbid {
		if _, err := regexp.Compile(f); err != nil {
			errs = append(errs, fmt.Errorf("%s.forbid: %q: %v", at, f, err))
		}
	}
	for _, g := range p.SharedPaths {
		if strings.TrimSpace(g) == "" {
			errs = append(errs, fmt.Errorf("%s.shared_paths: empty glob", at))
		}
	}
	if p.Tags != TagsFree && p.Tags != TagsReserved {
		errs = append(errs, fmt.Errorf("%s.tags: %q is not %s or %s", at, p.Tags, TagsFree, TagsReserved))
	}
	for agent, m := range p.Agent.Model {
		if !slices.Contains(Agents, agent) {
			errs = append(errs, fmt.Errorf("%s.agent.model: %q is not claude, copilot or cursor", at, agent))
		}
		if m == "" || badModel(m) {
			errs = append(errs, fmt.Errorf("%s.agent.model.%s: %q is not a model name", at, agent, m))
		}
	}
	if !slices.Contains([]string{Human, Shepherd, Agent}, p.Autonomy.Push) {
		errs = append(errs, fmt.Errorf("%s.autonomy.push: %q is not %s, %s or %s", at, p.Autonomy.Push, Human, Shepherd, Agent))
	}
	if !slices.Contains(PermissionModes, p.Agent.PermissionMode) {
		errs = append(errs, fmt.Errorf("%s.agent.permission_mode: %q is not %s", at, p.Agent.PermissionMode, strings.Join(PermissionModes, ", ")))
	}
	if p.Autonomy.Push == Shepherd && strings.TrimSpace(p.Gate) == "" {
		errs = append(errs, fmt.Errorf("%s.autonomy.push: shepherd pushes only after the gate passes, and no gate is set", at))
	}
	for i, f := range p.Follows {
		f, at := f.Effective(), fmt.Sprintf("%s.follows[%d]", at, i)
		if strings.TrimSpace(f.Repo) == "" {
			errs = append(errs, fmt.Errorf("%s.repo: empty", at))
		}
		if len(f.Scope) == 0 {
			errs = append(errs, fmt.Errorf("%s.scope: a follow's lane needs a scope", at))
		}
		if strings.TrimSpace(f.Task) == "" {
			errs = append(errs, fmt.Errorf("%s.task: empty", at))
		}
		if !slices.Contains([]string{"patch", "minor", "major"}, f.MinBump) {
			errs = append(errs, fmt.Errorf("%s.min_bump: %q is not patch, minor or major", at, f.MinBump))
		}
		if f.After != Published && f.After != Tagged {
			errs = append(errs, fmt.Errorf("%s.after: %q is not %s or %s", at, f.After, Published, Tagged))
		}
		if !slices.Contains(Agents, f.Agent) {
			errs = append(errs, fmt.Errorf("%s.agent: %q is not claude, copilot or cursor", at, f.Agent))
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

// merge returns base with every set field of over applied.
func merge(base, over Profile) Profile {
	out := base
	if over.BaseBranch != "" {
		out.BaseBranch = over.BaseBranch
	}
	if over.BranchModel != "" {
		out.BranchModel = over.BranchModel
	}
	if over.Promotion != nil {
		out.Promotion = over.Promotion
	}
	if over.Gate != "" {
		out.Gate = over.Gate
	}
	if over.SharedPaths != nil {
		out.SharedPaths = over.SharedPaths
	}
	if over.WorktreeRoot != "" {
		out.WorktreeRoot = over.WorktreeRoot
	}
	if over.Setup != "" {
		out.Setup = over.Setup
	}
	if over.CoordFile != "" {
		out.CoordFile = over.CoordFile
	}
	if over.Tags != "" {
		out.Tags = over.Tags
	}
	if over.TagPrefix != "" {
		out.TagPrefix = over.TagPrefix
	}
	if over.Brief != "" {
		out.Brief = over.Brief
	}
	if over.Forbid != nil {
		out.Forbid = over.Forbid
	}
	if over.Autonomy.Merge != "" {
		out.Autonomy.Merge = over.Autonomy.Merge
	}
	if over.Autonomy.Tag != "" {
		out.Autonomy.Tag = over.Autonomy.Tag
	}
	if over.Autonomy.Deploy != "" {
		out.Autonomy.Deploy = over.Autonomy.Deploy
	}
	if over.Autonomy.Push != "" {
		out.Autonomy.Push = over.Autonomy.Push
	}
	if over.Autonomy.PlanFirst != nil {
		out.Autonomy.PlanFirst = over.Autonomy.PlanFirst
	}
	if over.Agent.PermissionMode != "" {
		out.Agent.PermissionMode = over.Agent.PermissionMode
	}
	if len(over.Agent.Model) > 0 {
		m := maps.Clone(base.Agent.Model)
		if m == nil {
			m = map[string]string{}
		}
		maps.Copy(m, over.Agent.Model)
		out.Agent.Model = m
	}
	if over.Follows != nil {
		out.Follows = over.Follows
	}
	return out
}

//go:embed template.yaml
var template []byte

// Template is the commented config file the daemon writes on first start.
func Template() []byte { return template }

// WriteTemplate writes Template to path if nothing is there yet, and reports whether it
// did. An existing file is never touched.
func WriteTemplate(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(template); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}
