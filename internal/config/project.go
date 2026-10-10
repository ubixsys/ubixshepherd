package config

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// A cap is soft or hard. Both warn at 80% of the amount; at 100% soft only reports, and
// hard holds the runs Shepherd starts itself, never a run the person started.
const (
	CapSoft = "soft"
	CapHard = "hard"
)

// Caps are the values a budget's cap takes.
var Caps = []string{CapSoft, CapHard}

// capMode is a cap with its default filled in.
func capMode(s string) string {
	if s == "" {
		return CapHard
	}
	return s
}

// BudgetCapMode is daemon.budget_cap with its default filled in.
func (d Daemon) BudgetCapMode() string { return capMode(d.BudgetCap) }

// BudgetCapMode is desk.budget_cap with its default filled in.
func (d Desk) BudgetCapMode() string { return capMode(d.BudgetCap) }

// A budget's period.
const (
	PeriodDay   = "day"
	PeriodMonth = "month"
)

// Periods are the values a project budget's period takes.
var Periods = []string{PeriodDay, PeriodMonth}

// DefaultBriefMaxAge is how old a project's approved brief may be before it is stale.
const DefaultBriefMaxAge = 30 * 24 * time.Hour

// How a project's intake rule decides a request from another project.
const (
	// DecideAuto opens a lane and starts an agent.
	DecideAuto = "auto"
	// DecideHold makes the request a decision for the person.
	DecideHold = "hold"
	// DecideRefuse tells the asker why at once.
	DecideRefuse = "refuse"
)

// Decisions are the values an intake rule's decide takes.
var Decisions = []string{DecideAuto, DecideHold, DecideRefuse}

// The typed fields of a cross-project request that intake rules match on.
var (
	// IntakeKinds are the request kinds a rule's kind names.
	IntakeKinds = []string{"bug", "feature", "notice"}
	// IntakeEvidence are the attachments a rule's evidence may require.
	IntakeEvidence = []string{"failing_test", "repro", "log"}
	// IntakeTouches are what a rule's touches may allow a change to reach.
	IntakeTouches = []string{"internal", "tests", "docs", "public_api"}
)

// AnySender is the intake rule's from that matches every project.
const AnySender = "*"

// Project is a named group of repos in one workspace: a budget, caches and intake
// rules. Its brief is not here; it has an author and an approval, so it lives in the store.
// Use Config.Project for the effective values.
type Project struct {
	// Repos are the project's members, each a key of Config.Repos and in no other project.
	Repos []string `yaml:"repos" json:"repos"`
	// BriefMaxAge is how old the approved brief may be before it shows as stale: "30d",
	// or a Go duration ("720h"); "" is DefaultBriefMaxAge.
	BriefMaxAge string `yaml:"brief_max_age,omitempty" json:"brief_max_age,omitempty"`
	// Budget is the project's own spend limit; its zero value takes the daemon's.
	Budget ProjectBudget `yaml:"budget,omitempty" json:"budget"`
	// Caches are local build caches that are not repos, shared by the project's repos.
	Caches []Cache `yaml:"caches,omitempty" json:"caches,omitempty"`
	// Intake is checked in order for a request from another project; first match wins,
	// and no match holds.
	Intake []IntakeRule `yaml:"intake,omitempty" json:"intake,omitempty"`
}

// ProjectBudget is a project's spend limit.
type ProjectBudget struct {
	// Amount is dollars per period; nil is daemon.budget, 0 is no cap.
	Amount *float64 `yaml:"amount,omitempty" json:"amount,omitempty"`
	// Period is day (the default) or month.
	Period string `yaml:"period,omitempty" json:"period,omitempty"`
	// Cap is soft or hard; "" is hard.
	Cap string `yaml:"cap,omitempty" json:"cap,omitempty"`
}

// Cache declares a local build cache: information for agents, never a member.
type Cache struct {
	// Path is where the cache lives on this machine.
	Path string `yaml:"path" json:"path"`
	// MadeBy is "<repo>:<recipe path>", the recipe in a workspace repo that makes it.
	MadeBy string `yaml:"made_by" json:"made_by"`
	// Note is a line for agents.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Recipe splits MadeBy into the repo and the recipe's path in it.
func (c Cache) Recipe() (repo, path string, ok bool) {
	repo, path, ok = strings.Cut(c.MadeBy, ":")
	if !ok || strings.TrimSpace(repo) == "" || strings.TrimSpace(path) == "" {
		return "", "", false
	}
	return repo, path, true
}

// IntakeRule decides requests from other projects by the sender and typed fields only,
// never by their prose. A field left out matches anything.
type IntakeRule struct {
	// From is the sending project, or "*" for any.
	From string `yaml:"from" json:"from"`
	// Kind is one kind or a list: bug, feature or notice.
	Kind StringList `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Evidence are the attachments the request must carry.
	Evidence []string `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	// Touches is what an auto lane's scope may reach.
	Touches []string `yaml:"touches,omitempty" json:"touches,omitempty"`
	// Decide is auto, hold or refuse.
	Decide string `yaml:"decide" json:"decide"`
	// Why is the reason a refusal gives the asker.
	Why string `yaml:"why,omitempty" json:"why,omitempty"`
}

// StringList is a YAML string or a list of strings.
type StringList []string

// UnmarshalYAML accepts "bug" as well as [feature, bug].
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*l = StringList{s}
		return nil
	}
	var ss []string
	if err := n.Decode(&ss); err != nil {
		return err
	}
	*l = ss
	return nil
}

// Project returns the effective project: its entry in Projects with the budget taken
// from daemon.budget, the period day, the cap hard and the brief age 30d when unset.
func (c Config) Project(name string) Project {
	p := c.Projects[name]
	if p.Budget.Amount == nil && c.Daemon.Budget != nil {
		p.Budget.Amount = ptr(*c.Daemon.Budget)
	}
	if p.Budget.Period == "" {
		p.Budget.Period = PeriodDay
	}
	p.Budget.Cap = capMode(p.Budget.Cap)
	if p.BriefMaxAge == "" {
		p.BriefMaxAge = "30d"
	}
	return p
}

// ProjectOf names the project a repo is in; false when it is in none.
func (c Config) ProjectOf(repo string) (string, bool) {
	for _, name := range c.projectNames() {
		if slices.Contains(c.Projects[name].Repos, repo) {
			return name, true
		}
	}
	return "", false
}

// MaxAge is BriefMaxAge as a duration, DefaultBriefMaxAge when unset or unparseable
// (Validate rejects the latter).
func (p Project) MaxAge() time.Duration {
	if d, err := parseAge(p.BriefMaxAge); err == nil && p.BriefMaxAge != "" {
		return d
	}
	return DefaultBriefMaxAge
}

// parseAge reads "30d" or a Go duration; it must be above zero.
func parseAge(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, err
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("not above zero")
	}
	return d, nil
}

func (c Config) projectNames() []string {
	names := make([]string, 0, len(c.Projects))
	for n := range c.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (c Config) validateProjects() []error {
	var errs []error
	owner := map[string]string{}
	for _, name := range c.projectNames() {
		at := "projects." + name
		p := c.Projects[name]
		if strings.TrimSpace(name) != name || name == "" || name == AnySender || strings.ContainsAny(name, " \t\n") {
			errs = append(errs, fmt.Errorf("projects: %q is not a project name", name))
		}
		for _, r := range p.Repos {
			if _, ok := c.Repos[r]; !ok {
				errs = append(errs, fmt.Errorf("%s.repos: %q is not in repos", at, r))
				continue
			}
			if first, dup := owner[r]; dup {
				if first == name {
					errs = append(errs, fmt.Errorf("%s.repos: %q is listed twice", at, r))
				} else {
					errs = append(errs, fmt.Errorf("%s.repos: %q is already in project %q; a repo is in at most one project", at, r, first))
				}
				continue
			}
			owner[r] = name
		}
		if p.BriefMaxAge != "" {
			if _, err := parseAge(p.BriefMaxAge); err != nil {
				errs = append(errs, fmt.Errorf("%s.brief_max_age: %q; a duration above zero such as 30d or 720h", at, p.BriefMaxAge))
			}
		}
		if a := p.Budget.Amount; a != nil && *a < 0 {
			errs = append(errs, fmt.Errorf("%s.budget.amount: %v cannot be negative", at, *a))
		}
		if p.Budget.Period != "" && !slices.Contains(Periods, p.Budget.Period) {
			errs = append(errs, fmt.Errorf("%s.budget.period: %q is not %s", at, p.Budget.Period, strings.Join(Periods, " or ")))
		}
		if !slices.Contains(Caps, capMode(p.Budget.Cap)) {
			errs = append(errs, fmt.Errorf("%s.budget.cap: %q is not %s", at, p.Budget.Cap, strings.Join(Caps, " or ")))
		}
		for i, ch := range p.Caches {
			at := fmt.Sprintf("%s.caches[%d]", at, i)
			if strings.TrimSpace(ch.Path) == "" {
				errs = append(errs, fmt.Errorf("%s.path: empty", at))
			}
			repo, _, ok := ch.Recipe()
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("%s.made_by: %q is not <repo>:<recipe path>", at, ch.MadeBy))
			default:
				if _, in := c.Repos[repo]; !in {
					errs = append(errs, fmt.Errorf("%s.made_by: %q is not in repos", at, repo))
				}
			}
		}
		for i, r := range p.Intake {
			errs = append(errs, c.validateIntake(fmt.Sprintf("%s.intake[%d]", at, i), r)...)
		}
	}
	return errs
}

func (c Config) validateIntake(at string, r IntakeRule) []error {
	var errs []error
	if _, known := c.Projects[r.From]; r.From != AnySender && !known {
		errs = append(errs, fmt.Errorf("%s.from: %q is not a project or %q", at, r.From, AnySender))
	}
	if !slices.Contains(Decisions, r.Decide) {
		errs = append(errs, fmt.Errorf("%s.decide: %q is not %s", at, r.Decide, strings.Join(Decisions, ", ")))
	}
	for field, set := range map[string]struct{ got, want []string }{
		"kind":     {r.Kind, IntakeKinds},
		"evidence": {r.Evidence, IntakeEvidence},
		"touches":  {r.Touches, IntakeTouches},
	} {
		for _, v := range set.got {
			if !slices.Contains(set.want, v) {
				errs = append(errs, fmt.Errorf("%s.%s: %q is not %s", at, field, v, strings.Join(set.want, ", ")))
			}
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}
