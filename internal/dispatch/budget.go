package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// Output is what an adapter makes of one line of its agent's output: what to show in
// the run log ("" to drop the line), and any cost the line reports.
type Output struct {
	Show    string
	USD     float64
	Credits float64
}

// claudeOutput reads Claude Code's stream-json: the agent's text, a line per tool call,
// and the dollar cost from the final result.
func claudeOutput(line string) Output {
	var m struct {
		Type         string  `json:"type"`
		Subtype      string  `json:"subtype"`
		IsError      bool    `json:"is_error"`
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		Message      struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &m) != nil {
		return Output{Show: line}
	}
	switch m.Type {
	case "assistant":
		var parts []string
		for _, c := range m.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					parts = append(parts, t)
				}
			case "tool_use":
				if c.Name != "ToolSearch" {
					parts = append(parts, "→ "+c.Name+" "+clip(string(c.Input), 160))
				}
			}
		}
		return Output{Show: strings.Join(parts, "\n")}
	case "result":
		o := Output{USD: m.TotalCostUSD}
		if m.IsError || (m.Subtype != "" && m.Subtype != "success") {
			o.Show = "(the run ended with " + m.Subtype + ")"
		}
		return o
	}
	return Output{} // system, user (tool results), rate limits: not for the log
}

var copilotCredits = regexp.MustCompile(`^\s*AI Credits\s+([0-9.]+)`)

// copilotOutput keeps Copilot's text and reads its credits line.
func copilotOutput(line string) Output {
	o := Output{Show: line}
	if m := copilotCredits.FindStringSubmatch(line); m != nil {
		o.Credits, _ = strconv.ParseFloat(m[1], 64)
	}
	return o
}

func plainOutput(line string) Output { return Output{Show: line} }

// clock is the time budgets are counted against; tests move it.
var clock = time.Now

// Today is the local day spend is counted against.
func Today() string { return clock().Format("2006-01-02") }

// sessionCost turns the session total a run's agent reported (in CostUSD and Credits)
// into what this run added: the total less the session's total when the run it
// continues ended. The total is kept on the run for the next one. A run that reported
// nothing carries the session's total forward; a total below the last one means the
// CLI started counting again, so it is all this run's.
func (r *Runner) sessionCost(ctx context.Context, run *store.Run) {
	var usd, credits float64
	if run.Parent != 0 {
		if p, err := r.Store.Run(ctx, run.Parent); err == nil {
			usd, credits = p.SessionUSD, p.SessionCredits
			if usd == 0 && credits == 0 {
				// Recorded before session totals were kept: its cost was the total.
				usd, credits = p.CostUSD, p.Credits
			}
		}
	}
	if run.CostUSD == 0 && run.Credits == 0 {
		run.SessionUSD, run.SessionCredits = usd, credits
		return
	}
	run.SessionUSD, run.SessionCredits = run.CostUSD, run.Credits
	run.CostUSD, run.Credits = beyond(run.SessionUSD, usd), beyond(run.SessionCredits, credits)
}

// SessionDelta is what a session added since its last total, for agent CLIs that report
// the whole session's cost each time; a total below before means a new session.
func SessionDelta(total, before float64) float64 { return beyond(total, before) }

func beyond(total, before float64) float64 {
	if total < before {
		return total
	}
	return math.Round((total-before)*1e6) / 1e6 // 6.17 - 4.12 is 2.05, not 2.0500000000000003
}

// Spent is today's spend in dollars, Copilot's credits priced at credit_usd.
func (r *Runner) Spent(ctx context.Context) (float64, map[string]store.Spend, error) {
	return Spent(ctx, r.Store, r.Conf())
}

// Spent is today's spend from the store alone, for callers with no Runner.
func Spent(ctx context.Context, st store.Store, cfg config.Config) (float64, map[string]store.Spend, error) {
	by, err := st.SpendOn(ctx, Today())
	if err != nil {
		return 0, nil, err
	}
	total := 0.0
	for _, sp := range by {
		total += sp.USD + sp.Credits*creditUSD(cfg)
	}
	return total, by, nil
}

func creditUSD(cfg config.Config) float64 {
	if cfg.Daemon.CreditUSD == nil {
		return 0
	}
	return *cfg.Daemon.CreditUSD
}

func budget(cfg config.Config) float64 {
	if cfg.Daemon.Budget == nil {
		return 0
	}
	return *cfg.Daemon.Budget
}

// The kinds of budget line.
const (
	LineWorkspace = "workspace"
	LineDesk      = "desk"
	LineProject   = "project"
)

// BudgetLine is one budget and what has been spent against it in its period.
type BudgetLine struct {
	// Kind is LineWorkspace, LineDesk or LineProject; Name is the project's name.
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	// Key is the configuration key the amount comes from ("daemon.budget").
	Key string `json:"key"`
	// Amount is dollars per period; 0 is no cap.
	Amount float64 `json:"amount"`
	// Cap is config.CapSoft or config.CapHard.
	Cap string `json:"cap"`
	// Period is config.PeriodDay or config.PeriodMonth; Since is its first day.
	Period string `json:"period"`
	Since  string `json:"since"`
	// Spent is dollars so far this period, credits priced at credit_usd.
	Spent float64 `json:"spent"`
}

// Capped says the line has an amount to stay under.
func (l BudgetLine) Capped() bool { return l.Amount > 0 }

// Reached is spend at or past the amount.
func (l BudgetLine) Reached() bool { return l.Capped() && l.Spent >= l.Amount }

// Holds says the line holds the runs Shepherd starts itself: a hard cap, reached.
func (l BudgetLine) Holds() bool { return l.Reached() && l.Cap == config.CapHard }

// label names the period in prose.
func (l BudgetLine) label() string {
	if l.Period == config.PeriodMonth {
		return "month's"
	}
	return "today's"
}

// who names the budget in a message.
func (l BudgetLine) who() string {
	switch l.Kind {
	case LineDesk:
		return "the front desk's"
	case LineProject:
		return "project " + l.Name + "'s"
	}
	return "the workspace's"
}

// lifts says what ends a hold.
func (l BudgetLine) lifts() string {
	until := "tomorrow"
	if l.Period == config.PeriodMonth {
		until = "the first of next month"
	}
	return until + ", a higher " + l.Key + ", or cap: soft"
}

// BudgetLines is every budget's spend in its period: the workspace ceiling (daily), the
// front desk's own line, and each project's. A repo outside any project is in the
// workspace line only, and the desk is in no project's. Lines come in that order, the
// projects by name.
func BudgetLines(ctx context.Context, st store.Store, cfg config.Config) ([]BudgetLine, error) {
	today := Today()
	cu := creditUSD(cfg)
	price := func(usd, credits float64) float64 { return usd + credits*cu }
	day, err := st.SpendRollup(ctx, today, today)
	if err != nil {
		return nil, err
	}
	var month *store.SpendRollup
	since := clock().Format("2006-01") + "-01"
	names := make([]string, 0, len(cfg.Projects))
	for name := range cfg.Projects {
		names = append(names, name)
	}
	sort.Strings(names)

	usd, credits := day.Total()
	lines := []BudgetLine{{Kind: LineWorkspace, Key: "daemon.budget", Amount: budget(cfg), Cap: cfg.Daemon.BudgetCapMode(),
		Period: config.PeriodDay, Since: today, Spent: price(usd, credits)}}
	desk := BudgetLine{Kind: LineDesk, Key: "desk.budget", Cap: cfg.Desk.BudgetCapMode(),
		Period: config.PeriodDay, Since: today, Spent: price(day.Desk.USD, day.Desk.Credits)}
	if cfg.Desk.Budget != nil {
		desk.Amount = *cfg.Desk.Budget
	}
	lines = append(lines, desk)
	for _, name := range names {
		p := cfg.Project(name)
		l := BudgetLine{Kind: LineProject, Name: name, Key: "projects." + name + ".budget", Cap: p.Budget.Cap, Period: p.Budget.Period, Since: today}
		if p.Budget.Amount != nil {
			l.Amount = *p.Budget.Amount
		}
		roll := day
		if l.Period == config.PeriodMonth {
			if month == nil {
				m, err := st.SpendRollup(ctx, since, today)
				if err != nil {
					return nil, err
				}
				month = &m
			}
			roll, l.Since = *month, since
		}
		for _, rs := range roll.Repos {
			if slices.Contains(p.Repos, rs.Repo) {
				l.Spent += price(rs.USD, rs.Credits)
			}
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// overBudget says why an automatic run must wait, or "". It looks at the workspace
// ceiling only: use overBudgetIn for a run in a repo that may be in a project.
func (r *Runner) overBudget(ctx context.Context, cfg config.Config) string {
	return r.overBudgetIn(ctx, cfg, "")
}

// overBudgetIn says why a run Shepherd would start on its own in repo (a name in
// Config.Repos; "" for none) must wait, or "". The workspace ceiling holds first, then
// the repo's project. Only a hard cap holds, and the desk's line never holds a run: a
// talkative desk cannot starve a project. The caller never asks for a run the person
// started.
func (r *Runner) overBudgetIn(ctx context.Context, cfg config.Config, repo string) string {
	lines, err := BudgetLines(ctx, r.Store, cfg)
	if err != nil {
		return ""
	}
	project, _ := cfg.ProjectOf(repo)
	for _, l := range lines {
		if !l.Holds() || l.Kind == LineDesk || (l.Kind == LineProject && l.Name != project) {
			continue
		}
		if l.Kind == LineWorkspace && l.Period == config.PeriodDay {
			return fmt.Sprintf("today's spend, $%.2f, has reached the daily budget of $%.2f (%s); Shepherd holds the runs it would start on its own until tomorrow or a higher budget", l.Spent, l.Amount, l.Key)
		}
		return fmt.Sprintf("%s spend, $%.2f, has reached %s budget of $%.2f (%s); Shepherd holds the runs it would start on its own until %s", l.label(), l.Spent, l.who(), l.Amount, l.Key, l.lifts())
	}
	return ""
}

// DeskHeld says why the front desk should not take a turn on its own, or "": its own
// line is a hard cap and has been reached. It holds nothing else, and a turn the person
// asks for is never held.
func DeskHeld(ctx context.Context, st store.Store, cfg config.Config) string {
	lines, err := BudgetLines(ctx, st, cfg)
	if err != nil {
		return ""
	}
	for _, l := range lines {
		if l.Kind == LineDesk && l.Holds() {
			return fmt.Sprintf("the front desk's spend, $%.2f, has reached its budget of $%.2f (%s); it takes no turns of its own until tomorrow, a higher %s, or cap: soft", l.Spent, l.Amount, l.Key, l.Key)
		}
	}
	return ""
}

// Spend records money spent and warns, once per period each, at 80% and 100% of every
// budget the spend counts toward: the workspace's, the desk's and the project's.
func (r *Runner) Spend(ctx context.Context, sp store.Spend) error {
	if sp.USD == 0 && sp.Credits == 0 {
		return nil
	}
	sp.Day = Today()
	if err := r.Store.AddSpend(ctx, sp); err != nil {
		return err
	}
	lines, err := BudgetLines(ctx, r.Store, r.Conf())
	if err != nil {
		return err
	}
	for _, l := range lines {
		if !l.Capped() {
			continue
		}
		r.warn(ctx, l)
	}
	return nil
}

// warn says it once per period when l has passed 80% or 100%: the higher mark only.
func (r *Runner) warn(ctx context.Context, l BudgetLine) {
	scope := l.Kind
	if l.Kind == LineProject {
		scope += "." + l.Name
	}
	reached := l.title() + " reached: $%.2f of $%.2f."
	switch {
	case l.Cap == config.CapSoft:
		reached += " It is a soft cap: nothing is held."
	case l.Kind == LineDesk:
		reached += " The desk takes no turns of its own until it lifts; turns you ask for still go."
	default:
		reached += " Shepherd now holds the runs it would start on its own (fixes, routed requests); runs you or the desk start still go."
	}
	used := "80%% of " + l.who() + " " + l.period() + " budget used: $%.2f of $%.2f."
	if l.Kind == LineWorkspace {
		used = "80%% of " + l.label() + " budget used: $%.2f of $%.2f."
	}
	for _, mark := range []struct {
		at   float64
		key  string
		text string
	}{
		{1.0, "budget.reached." + scope + "." + l.Since, reached},
		{0.8, "budget.warned." + scope + "." + l.Since, used},
	} {
		if l.Spent < mark.at*l.Amount {
			continue
		}
		key := mark.key
		if l.Kind == LineWorkspace { // the keys the single budget always used
			key = strings.Replace(key, ".workspace", "", 1)
		}
		if done, _ := r.Store.Setting(ctx, key); done != "" {
			break
		}
		r.Store.SetSetting(ctx, key, "1")
		r.feed(ctx, store.FeedBudget, 0, mark.text, l.Spent, l.Amount)
		break
	}
}

// period is "daily" or "monthly".
func (l BudgetLine) period() string {
	if l.Period == config.PeriodMonth {
		return "monthly"
	}
	return "daily"
}

// title starts the 100% message: "Daily budget", "Project x's monthly budget".
func (l BudgetLine) title() string {
	if l.Kind == LineWorkspace {
		return "Daily budget"
	}
	w := l.who() + " " + l.period() + " budget"
	return strings.ToUpper(w[:1]) + w[1:]
}
