package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const twoRepos = "repos:\n  a: {}\n  b: {}\n  c: {}\n"

func TestProjectDefaults(t *testing.T) {
	c, err := Parse([]byte("daemon:\n  budget: 7.5\n" + twoRepos + "projects:\n  p:\n    repos: [a, b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Project("p")
	if p.Budget.Amount == nil || *p.Budget.Amount != 7.5 || p.Budget.Period != PeriodDay || p.Budget.Cap != CapHard ||
		p.MaxAge() != DefaultBriefMaxAge || p.BriefMaxAge != "30d" || len(p.Caches) != 0 || len(p.Intake) != 0 {
		t.Errorf("defaults = %+v", p)
	}
	if c.Projects["p"].Budget.Amount != nil || c.Projects["p"].Budget.Cap != "" {
		t.Errorf("Project changed the stored entry: %+v", c.Projects["p"].Budget)
	}
	if name, ok := c.ProjectOf("b"); !ok || name != "p" {
		t.Errorf("ProjectOf(b) = %q %v", name, ok)
	}
	if name, ok := c.ProjectOf("c"); ok {
		t.Errorf("repo c is in %q, want none", name)
	}
	if d := Default(); d.Daemon.BudgetCapMode() != CapHard || d.Desk.BudgetCapMode() != CapHard || d.Desk.Budget != nil {
		t.Errorf("workspace and desk defaults = %+v %+v", d.Daemon, d.Desk)
	}
}

func TestProjectSet(t *testing.T) {
	c, err := Parse([]byte(`
daemon:
  budget: 20
  budget_cap: soft
desk:
  budget: 3
  budget_cap: hard
repos:
  a: {}
  b: {}
  tools: {}
projects:
  one:
    repos: [a]
    brief_max_age: 12h
    budget: {amount: 0, period: month, cap: soft}
    caches:
      - path: ~/build/cache
        made_by: tools:build/make.sh
        note: rebuild it
    intake:
      - from: "*"
        kind: [bug, feature]
        evidence: [failing_test, log]
        touches: [tests]
        decide: auto
      - from: two
        kind: notice
        decide: refuse
        why: no
  two:
    repos: [b]
    budget: {amount: 4.5}
`))
	if err != nil {
		t.Fatal(err)
	}
	one := c.Project("one")
	if one.MaxAge() != 12*time.Hour || *one.Budget.Amount != 0 || one.Budget.Period != PeriodMonth || one.Budget.Cap != CapSoft {
		t.Errorf("one = %+v", one)
	}
	if r := one.Intake[0]; len(r.Kind) != 2 || r.Kind[1] != "feature" || r.Decide != DecideAuto || len(r.Evidence) != 2 {
		t.Errorf("rule 0 = %+v", r)
	}
	if r := one.Intake[1]; len(r.Kind) != 1 || r.Kind[0] != "notice" || r.From != "two" || r.Why != "no" {
		t.Errorf("rule 1 = %+v", r)
	}
	if repo, path, ok := one.Caches[0].Recipe(); !ok || repo != "tools" || path != "build/make.sh" {
		t.Errorf("recipe = %q %q %v", repo, path, ok)
	}
	if two := c.Project("two"); *two.Budget.Amount != 4.5 || two.Budget.Period != PeriodDay || two.Budget.Cap != CapHard {
		t.Errorf("two = %+v", two)
	}
	if c.Daemon.BudgetCapMode() != CapSoft || c.Desk.BudgetCapMode() != CapHard || *c.Desk.Budget != 3 {
		t.Errorf("workspace and desk = %+v %+v", c.Daemon, c.Desk)
	}
}

func TestProjectRejects(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"unknown member":        {"projects:\n  p:\n    repos: [zzz]\n", `projects.p.repos: "zzz" is not in repos`},
		"repo in two projects":  {"projects:\n  p:\n    repos: [a]\n  q:\n    repos: [a, b]\n", `projects.q.repos: "a" is already in project "p"`},
		"repo listed twice":     {"projects:\n  p:\n    repos: [a, a]\n", `"a" is listed twice`},
		"bad cap":               {"projects:\n  p:\n    budget: {cap: firm}\n", "projects.p.budget.cap"},
		"bad period":            {"projects:\n  p:\n    budget: {period: week}\n", "projects.p.budget.period"},
		"negative amount":       {"projects:\n  p:\n    budget: {amount: -1}\n", "projects.p.budget.amount"},
		"bad age":               {"projects:\n  p:\n    brief_max_age: soon\n", "projects.p.brief_max_age"},
		"zero age":              {"projects:\n  p:\n    brief_max_age: 0d\n", "projects.p.brief_max_age"},
		"negative age":          {"projects:\n  p:\n    brief_max_age: -3d\n", "projects.p.brief_max_age"},
		"made_by no colon":      {"projects:\n  p:\n    caches: [{path: /x, made_by: a}]\n", "projects.p.caches[0].made_by"},
		"made_by empty path":    {"projects:\n  p:\n    caches: [{path: /x, made_by: \"a:\"}]\n", "projects.p.caches[0].made_by"},
		"made_by empty repo":    {"projects:\n  p:\n    caches: [{path: /x, made_by: \":tools/x.sh\"}]\n", "projects.p.caches[0].made_by"},
		"made_by unknown repo":  {"projects:\n  p:\n    caches: [{path: /x, made_by: \"zzz:x.sh\"}]\n", `made_by: "zzz" is not in repos`},
		"cache no path":         {"projects:\n  p:\n    caches: [{made_by: \"a:x.sh\"}]\n", "projects.p.caches[0].path"},
		"decide":                {"projects:\n  p:\n    intake: [{from: \"*\", decide: maybe}]\n", "projects.p.intake[0].decide"},
		"decide missing":        {"projects:\n  p:\n    intake: [{from: \"*\"}]\n", "projects.p.intake[0].decide"},
		"unknown sender":        {"projects:\n  p:\n    intake: [{from: nobody, decide: hold}]\n", "projects.p.intake[0].from"},
		"no sender":             {"projects:\n  p:\n    intake: [{decide: hold}]\n", "projects.p.intake[0].from"},
		"bad kind":              {"projects:\n  p:\n    intake: [{from: \"*\", kind: chore, decide: hold}]\n", "projects.p.intake[0].kind"},
		"bad evidence":          {"projects:\n  p:\n    intake: [{from: \"*\", evidence: [vibes], decide: hold}]\n", "projects.p.intake[0].evidence"},
		"bad touches":           {"projects:\n  p:\n    intake: [{from: \"*\", touches: [everything], decide: auto}]\n", "projects.p.intake[0].touches"},
		"unknown project key":   {"projects:\n  p:\n    members: [a]\n", "members"},
		"unknown budget key":    {"projects:\n  p:\n    budget: {limit: 3}\n", "limit"},
		"project name star":     {"projects:\n  \"*\": {}\n", "not a project name"},
		"project name spaced":   {"projects:\n  \"my project\": {}\n", "not a project name"},
		"workspace bad cap":     {"daemon:\n  budget_cap: firm\n", "daemon.budget_cap"},
		"desk bad cap":          {"desk:\n  budget_cap: firm\n", "desk.budget_cap"},
		"desk negative budget":  {"desk:\n  budget: -2\n", "desk.budget"},
		"desk budget not a num": {"desk:\n  budget: lots\n", "cannot unmarshal"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(twoRepos + tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestProjectAges(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		c, err := Parse([]byte(twoRepos + "projects:\n  p:\n    brief_max_age: " + in + "\n"))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := c.Project("p").MaxAge(); got != want {
			t.Errorf("brief_max_age %s = %v, want %v", in, got, want)
		}
	}
}

func TestProjectsReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(twoRepos + "projects:\n  p:\n    repos: [a]\n    budget: {amount: 5}\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *c.Project("p").Budget.Amount != 5 {
		t.Fatalf("first load = %+v", c.Project("p"))
	}
	write(twoRepos + "projects:\n  p:\n    repos: [a, b]\n    budget: {amount: 9, cap: soft}\n")
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Project("p"); *p.Budget.Amount != 9 || p.Budget.Cap != CapSoft || len(p.Repos) != 2 {
		t.Errorf("reload = %+v", p)
	}
	// A bad edit is an error, so the daemon keeps the config it has.
	write(twoRepos + "projects:\n  p:\n    repos: [a]\n  q:\n    repos: [a]\n")
	if _, err := Load(path); err == nil {
		t.Error("a repo in two projects loaded")
	}
}
