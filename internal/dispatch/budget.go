package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
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

// Today is the local day spend is counted against.
func Today() string { return time.Now().Format("2006-01-02") }

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

// overBudget says why an automatic run must wait, or "".
func (r *Runner) overBudget(ctx context.Context, cfg config.Config) string {
	b := budget(cfg)
	if b <= 0 {
		return ""
	}
	spent, _, err := Spent(ctx, r.Store, cfg)
	if err != nil || spent < b {
		return ""
	}
	return fmt.Sprintf("today's spend, $%.2f, has reached the daily budget of $%.2f (daemon.budget); Shepherd holds the runs it would start on its own until tomorrow or a higher budget", spent, b)
}

// Spend records money spent and warns, once a day each, at 80% and 100% of the budget.
func (r *Runner) Spend(ctx context.Context, sp store.Spend) error {
	if sp.USD == 0 && sp.Credits == 0 {
		return nil
	}
	sp.Day = Today()
	if err := r.Store.AddSpend(ctx, sp); err != nil {
		return err
	}
	cfg := r.Conf()
	b := budget(cfg)
	if b <= 0 {
		return nil
	}
	spent, _, err := Spent(ctx, r.Store, cfg)
	if err != nil {
		return err
	}
	for _, mark := range []struct {
		at   float64
		key  string
		text string
	}{
		{1.0, "budget.reached." + sp.Day, "Daily budget reached: $%.2f of $%.2f. Shepherd now holds the runs it would start on its own (fixes, routed requests); runs you or the desk start still go."},
		{0.8, "budget.warned." + sp.Day, "80%% of today's budget used: $%.2f of $%.2f."},
	} {
		if spent < mark.at*b {
			continue
		}
		if done, _ := r.Store.Setting(ctx, mark.key); done != "" {
			break
		}
		r.Store.SetSetting(ctx, mark.key, "1")
		r.feed(ctx, store.FeedBudget, 0, mark.text, spent, b)
		break
	}
	return nil
}
