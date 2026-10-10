package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/version"
)

func runVersion(_ context.Context, env Env, args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	fmt.Fprintln(env.Stdout, "shepherd", version.Version)
	return nil
}

func runStatus(ctx context.Context, env Env, args []string) error {
	fs := flags("status", env)
	asJSON := fs.Bool("json", false, "print JSON")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	here, err := c.Resolve(ctx, env.Cwd)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(env, struct {
			api.Status
			Here api.Resolution `json:"here"`
		}{st, here})
	}
	w := env.Stdout
	fmt.Fprintf(w, "daemon   running, pid %d, version %s, since %s\n", st.PID, st.Version, st.Started.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "store    %s\n", st.Store)
	fmt.Fprintf(w, "config   %s\n", st.Config)
	if sp, err := c.SpendToday(ctx); err == nil {
		line := fmt.Sprintf("spend    $%.2f today", sp.USD)
		if sp.Budget > 0 {
			line += fmt.Sprintf(" of a $%.2f daily budget", sp.Budget)
		}
		fmt.Fprintln(w, line+" (Claude reports dollars; Copilot credits at $"+fmt.Sprintf("%.2f", sp.CreditUSD)+"; Cursor reports nothing; OpenCode, 0 on a local model)")
	}
	if len(st.Workspaces) == 0 {
		fmt.Fprintln(w, "\nNo workspaces yet. Register one with: shepherd init ~/git")
	} else {
		fmt.Fprintln(w, "\nWorkspaces:")
		for _, ws := range st.Workspaces {
			fmt.Fprintf(w, "  %-12s %s  (%s, %s)\n", ws.Name, ws.Path,
				plural(ws.Repos, "repo", "repos"), plural(ws.Lanes, "lane", "lanes"))
		}
	}
	fmt.Fprintln(w)
	printWhere(env, here)
	return nil
}

func runWhere(ctx context.Context, env Env, args []string) error {
	fs := flags("where", env)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	dir := env.Cwd
	switch len(pos) {
	case 0:
	case 1:
		dir = pos[0]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(env.Cwd, dir)
		}
	default:
		return errUsage
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	res, err := c.Resolve(ctx, dir)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(env, res)
	}
	printWhere(env, res)
	return nil
}

func printWhere(env Env, r api.Resolution) {
	w := env.Stdout
	fmt.Fprintf(w, "here     %s\n", r.Path)
	if r.Workspace == nil {
		fmt.Fprintln(w, "         not in a workspace")
		return
	}
	fmt.Fprintf(w, "  workspace  %s (%s)\n", r.Workspace.Name, r.Workspace.Path)
	if r.Repo == nil {
		fmt.Fprintln(w, "  repo       none: covering every repo in the workspace")
		return
	}
	fmt.Fprintf(w, "  repo       %s\n", r.Repo.Name)
	if r.Lane != nil {
		fmt.Fprintf(w, "  lane       %s (%s, %s)\n", r.Lane.Name, r.Lane.Branch, r.Lane.State)
		fmt.Fprintf(w, "  opened by  %s\n", openedBy(r.Lane.Origin))
	}
	if p := r.Profile; p != nil {
		model := p.BranchModel
		if p.BranchModel == config.Promotion {
			model = fmt.Sprintf("%s %v", model, p.Promotion)
		}
		plan := "no"
		if p.Autonomy.PlanFirst != nil && *p.Autonomy.PlanFirst {
			plan = "yes"
		}
		fmt.Fprintf(w, "  profile    base %s, %s, gate %s\n", p.BaseBranch, model, orNone(p.Gate))
		fmt.Fprintf(w, "             merge %s, tag %s, deploy %s, push %s, plan first %s\n",
			p.Autonomy.Merge, p.Autonomy.Tag, p.Autonomy.Deploy, p.Autonomy.Push, plan)
		fmt.Fprintf(w, "             shared paths %s\n", joinOr(p.SharedPaths, "none"))
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func printJSON(env Env, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// canonicalDir resolves dir against cwd and checks it is a directory.
func canonicalDir(env Env, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(env.Cwd, dir)
	}
	dir, err := paths.Canonical(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, nil
}
