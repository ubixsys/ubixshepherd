package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

const dayFormat = "2006-01-02"

// runStats is `shepherd stats`: tokens, context sizes and cost over a range of days,
// by agent and model, by lane, and for the front desk.
func runStats(ctx context.Context, env Env, args []string) error {
	fs := flags("stats", env)
	days := fs.Int("days", 7, "the last N days, today included")
	from := fs.String("from", "", "first day, YYYY-MM-DD (instead of --days)")
	to := fs.String("to", "", "last day, YYYY-MM-DD (default today)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errUsage
	}
	today := time.Now().Format(dayFormat)
	if *to == "" {
		*to = today
	}
	if *from == "" {
		if *days < 1 {
			return fmt.Errorf("--days must be at least 1")
		}
		end, err := time.ParseInLocation(dayFormat, *to, time.Local)
		if err != nil {
			return fmt.Errorf("--to %q is not a day (want YYYY-MM-DD)", *to)
		}
		*from = end.AddDate(0, 0, 1-*days).Format(dayFormat)
	}
	for _, d := range []string{*from, *to} {
		if _, err := time.Parse(dayFormat, d); err != nil {
			return fmt.Errorf("%q is not a day (want YYYY-MM-DD)", d)
		}
	}
	if *from > *to {
		return fmt.Errorf("--from %s is after --to %s", *from, *to)
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	st, err := c.Usage(ctx, *from, *to)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(env, st)
	}
	printStats(env.Stdout, st)
	return nil
}

func printStats(w io.Writer, st store.UsageStats) {
	if st.From == st.To {
		fmt.Fprintf(w, "Usage on %s\n", st.From)
	} else {
		fmt.Fprintf(w, "Usage from %s to %s\n", st.From, st.To)
	}
	if st.Total.Count == 0 {
		fmt.Fprintln(w, "\nNothing recorded in this range. Runs and desk turns made before usage was kept have no tokens.")
		return
	}
	section(w, "Agent runs", st.Runs, "runs")
	table(w, "agent and model", st.ByModel, "RUNS")
	table(w, "lane", st.ByLane, "RUNS")
	section(w, "Front desk", st.Desk, "turns")
	table(w, "model", st.DeskByModel, "TURNS")
	fmt.Fprintln(w, "\nPEAK is the largest single request's prompt (input + cache read + cache write); >200K counts the records whose peak went past a 200k window.")
	fmt.Fprintln(w, "MEASURED counts the records whose CLI reported tokens; the rest are in RUNS and COST only.")
}

func section(w io.Writer, name string, t store.UsageTotals, unit string) {
	fmt.Fprintf(w, "\n%s: %d %s", name, t.Count, unit)
	if t.Count == 0 {
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, " (%d with token counts), %s\n", t.Measured, cost(t))
	if t.Measured == 0 {
		return
	}
	fmt.Fprintf(w, "  peak context %s, %d over 200k, %d compaction(s)", store.Tokens(t.PeakContext), t.Over200k, t.Compactions)
	if t.ContextWindow > 0 {
		fmt.Fprintf(w, ", largest window %s", store.Tokens(int64(t.ContextWindow)))
	}
	fmt.Fprintf(w, "\n  tokens: in %s, cache read %s, cache write %s, out %s\n",
		store.Tokens(t.Input), store.Tokens(t.CacheRead), store.Tokens(t.CacheCreation), store.Tokens(t.Output))
}

func table(w io.Writer, by string, groups []store.UsageGroup, count string) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  %s\t%s\tMEASURED\tPEAK\t>200K\tWINDOW\tCOMPACT\tINPUT\tCACHE-READ\tCACHE-WRITE\tOUTPUT\tCOST\n", strings.ToUpper(by), count)
	for _, g := range groups {
		peak, window := "-", "-"
		if g.Measured > 0 {
			peak = store.Tokens(g.PeakContext)
		}
		if g.ContextWindow > 0 {
			window = store.Tokens(int64(g.ContextWindow))
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", g.Key, g.Count, g.Measured, peak, g.Over200k, window,
			g.Compactions, store.Tokens(g.Input), store.Tokens(g.CacheRead), store.Tokens(g.CacheCreation), store.Tokens(g.Output), cost(g.UsageTotals))
	}
	tw.Flush()
}

func cost(t store.UsageTotals) string {
	s := fmt.Sprintf("$%.2f", t.CostUSD)
	if t.Credits > 0 {
		s += fmt.Sprintf(" + %.2f credits", t.Credits)
	}
	return s
}
