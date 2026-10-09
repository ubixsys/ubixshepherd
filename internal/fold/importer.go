package fold

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/scope"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// CoordRow is one row of a coordination file's lane table: who, what paths, which
// branches.
type CoordRow struct {
	Agent    string   `json:"agent"`
	Scope    []string `json:"scope"`
	Prefixes []string `json:"prefixes"`
	// Dropped are backticked tokens in the scope cell that are not repo paths (a
	// namespace, a command): the lane's scope may be narrower than the row meant.
	Dropped []string `json:"dropped,omitempty"`
}

var backticked = regexp.MustCompile("`([^`]+)`")

// ParseCoord reads the lane table of a coordination file (AGENTS-COORD.md): the first
// Markdown table whose header names an agent, a scope and a branch prefix. Scope cells
// are prose; the paths in them are the backticked tokens that look like paths.
//
// Shepherd's own generated view (between its markers) is skipped: it is a table of
// lanes too, and reading it back would import Shepherd's lanes into themselves.
func ParseCoord(md string) []CoordRow {
	if i := strings.Index(md, viewBegin); i >= 0 {
		if j := strings.Index(md[i:], viewEnd); j >= 0 {
			md = md[:i] + md[i+j+len(viewEnd):]
		}
	}
	var rows []CoordRow
	cols := map[string]int{}
	inTable := false
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "|") {
			if inTable && t != "" {
				inTable = false // a table ends at the first non-table, non-blank line
				if len(rows) > 0 {
					break
				}
			}
			continue
		}
		cells := splitRow(t)
		if !inTable {
			cols = map[string]int{}
			for i, c := range cells {
				lc := strings.ToLower(c)
				switch {
				case strings.Contains(lc, "agent") || lc == "lane":
					cols["agent"] = i
				case strings.Contains(lc, "scope") || strings.Contains(lc, "paths"):
					cols["scope"] = i
				case strings.Contains(lc, "prefix") || strings.Contains(lc, "branch"):
					cols["prefix"] = i
				}
			}
			if len(cols) == 3 {
				inTable = true
			}
			continue
		}
		if strings.Trim(t, "|-: ") == "" {
			continue // the separator row
		}
		get := func(k string) string {
			if i := cols[k]; i < len(cells) {
				return cells[i]
			}
			return ""
		}
		kept, dropped := scopeTokens(get("scope"))
		row := CoordRow{Agent: get("agent"), Scope: kept, Dropped: dropped, Prefixes: tokens(get("prefix"))}
		if row.Agent != "" && len(row.Prefixes) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}

func splitRow(t string) []string {
	t = strings.TrimSuffix(strings.TrimPrefix(t, "|"), "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func tokens(cell string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// scopeTokens keeps the backticked tokens that are repo paths or globs, with braces
// expanded: `docs/{a,b}.md` is two globs. Namespaces (with backslashes), commands and
// prose are dropped.
func scopeTokens(cell string) (kept, dropped []string) {
	for _, tok := range tokens(cell) {
		if strings.ContainsAny(tok, "\\ ") || strings.HasPrefix(tok, "-") {
			dropped = append(dropped, tok)
			continue
		}
		for _, g := range expandBraces(tok) {
			g = strings.TrimPrefix(g, "./")
			if looksLikePath(g) {
				kept = append(kept, g)
			} else {
				dropped = append(dropped, g)
			}
		}
	}
	return kept, dropped
}

func looksLikePath(s string) bool {
	if s == "" || strings.ContainsAny(s, "()=:;'\"") {
		return false
	}
	if strings.Contains(s, "/") || strings.Contains(s, "*") {
		return true
	}
	// A bare file name: name.ext with a lowercase extension (composer.json, .gitlab-ci.yml).
	ext := path.Ext(s)
	return ext != "" && ext == strings.ToLower(ext) && len(ext) <= 6
}

// expandBraces turns a{b,c}d into abd and acd, one level per pass.
func expandBraces(s string) []string {
	i := strings.Index(s, "{")
	j := strings.Index(s, "}")
	if i < 0 || j < i {
		return []string{s}
	}
	var out []string
	for _, alt := range strings.Split(s[i+1:j], ",") {
		out = append(out, expandBraces(s[:i]+alt+s[j+1:])...)
	}
	return out
}

// adoptedAgent is the Agent of an item adopted with no row to name one.
const adoptedAgent = "(adopted, no row)"

// ImportItem is one worktree's fate in an import.
type ImportItem struct {
	Worktree string   `json:"worktree"`
	Branch   string   `json:"branch"`
	Lane     string   `json:"lane,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Scope    []string `json:"scope,omitempty"`
	// Action is import, skip; Why says why it is skipped, or what to know.
	Action string `json:"action"`
	Why    string `json:"why,omitempty"`
}

// ImportPlan is what an import does, or would do.
type ImportPlan struct {
	Items []ImportItem `json:"items"`
	// Unclaimed are table rows no worktree matches: claims with no work in flight.
	Unclaimed []string `json:"unclaimed,omitempty"`
	// Overlaps are pairs of imported lanes whose scopes overlap: the honour system's
	// collisions, reported rather than refused.
	Overlaps []string `json:"overlaps,omitempty"`
	Applied  bool     `json:"applied"`
}

// inferScope is the directories of the files a branch changed since it left the base:
// each changed file's directory, as dir/**, a file at the root as itself. Twelve or more
// collapse to their parents, so a broad branch gets a broad scope rather than a long one.
func inferScope(ctx context.Context, repo, branch, target string) []string {
	base, err := git.Run(ctx, repo, "merge-base", target, "refs/heads/"+branch)
	if err != nil {
		return nil
	}
	out, err := git.Run(ctx, repo, "diff", "--name-only", "--no-renames", base, "refs/heads/"+branch)
	if err != nil || out == "" {
		return nil
	}
	files := strings.Split(out, "\n")
	for depth := 6; depth >= 1; depth-- {
		seen := map[string]bool{}
		var globs []string
		for _, f := range files {
			dir := path.Dir(f)
			if dir == "." {
				if !seen[f] {
					seen[f] = true
					globs = append(globs, f)
				}
				continue
			}
			parts := strings.Split(dir, "/")
			if len(parts) > depth {
				parts = parts[:depth]
			}
			g := strings.Join(parts, "/") + "/**"
			if !seen[g] {
				seen[g] = true
				globs = append(globs, g)
			}
		}
		if len(globs) < 12 || depth == 1 {
			return globs
		}
	}
	return nil
}

var laneUnsafe = regexp.MustCompile(`[^a-z0-9._/-]+`)

// laneNameFor turns a branch into a lane name: lowercase, at most one slash.
func laneNameFor(branch string) string {
	n := laneUnsafe.ReplaceAllString(strings.ToLower(branch), "-")
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[:i+1] + strings.ReplaceAll(n[i+1:], "/", "-")
	}
	return strings.Trim(n, "-/")
}

// Import reads a repo's coordination file and worktrees into lanes: each worktree whose
// branch matches a row's prefix becomes a lane with that row's scope. Worktrees that look
// finished, match no row, or are already lanes are skipped and said so. Without apply it
// only plans.
func (f *Fold) Import(ctx context.Context, repoID int64, coordFile string, apply bool) (ImportPlan, error) {
	return f.ImportAdopt(ctx, repoID, coordFile, apply, nil)
}

// ImportAdopt is Import, also turning the worktrees of the named branches into lanes
// when no row claims them (sessions that never registered), with scopes inferred from
// what each branch changed.
func (f *Fold) ImportAdopt(ctx context.Context, repoID int64, coordFile string, apply bool, adopt []string) (ImportPlan, error) {
	adopting := map[string]bool{}
	for _, b := range adopt {
		adopting[b] = true
	}
	var plan ImportPlan
	repo, err := f.Store.Repo(ctx, repoID)
	if err != nil {
		return plan, err
	}
	if coordFile == "" {
		coordFile = "AGENTS-COORD.md"
	}
	if !filepath.IsAbs(coordFile) {
		coordFile = filepath.Join(repo.Path, coordFile)
	}
	b, err := os.ReadFile(coordFile)
	if err != nil {
		return plan, refuse("cannot read the coordination file: %v", err)
	}
	rows := ParseCoord(string(b))
	if len(rows) == 0 {
		return plan, refuse("%s has no lane table Shepherd can read (a table with agent, scope and branch prefix columns)", coordFile)
	}
	// A dropped token that names a file in the repo is a path after all (Makefile,
	// Dockerfile_Test: no extension to tell by).
	if files, err := listFiles(ctx, repo.Path, "HEAD"); err == nil {
		have := map[string]bool{}
		for _, f := range files {
			have[f] = true
		}
		for i := range rows {
			var still []string
			for _, d := range rows[i].Dropped {
				if have[d] {
					rows[i].Scope = append(rows[i].Scope, d)
				} else {
					still = append(still, d)
				}
			}
			rows[i].Dropped = still
		}
	}
	wts, err := git.Worktrees(ctx, repo.Path)
	if err != nil {
		return plan, err
	}
	existing, err := f.Store.Lanes(ctx, repo.ID)
	if err != nil {
		return plan, err
	}
	isLane := map[string]bool{}
	for _, l := range existing {
		isLane[l.Worktree] = true
	}
	prof := f.Conf().Profile(repo.Name)
	base := prof.BaseBranch
	target := base
	if git.RefExists(ctx, repo.Path, "refs/remotes/origin/"+base) {
		target = "origin/" + base
	}
	var fg forge.Forge
	if f.ForgeFor != nil {
		fg, _ = f.ForgeFor(repo.Remote)
	}
	matched := map[int]bool{}
	var imported []store.Lane
	for _, wt := range wts[1:] {
		canon, _ := paths.Canonical(wt.Path)
		it := ImportItem{Worktree: canon, Branch: wt.Branch, Action: "skip"}
		switch {
		case wt.Bare:
			continue
		case isLane[canon]:
			it.Why = "already a Shepherd lane"
		case wt.Branch == "":
			it.Why = "detached HEAD, no branch"
		case !exists(canon):
			it.Why = "missing on disk"
		case git.Ok(ctx, repo.Path, "merge-base", "--is-ancestor", "refs/heads/"+wt.Branch, target):
			it.Why = "its branch is already in " + target + " (merged, or no new commits): a candidate for fold gc"
		case fg != nil:
			if mr, err := fg.MRForBranch(ctx, wt.Branch); err == nil && mr != nil && mr.State == "merged" && mr.SHA != "" &&
				git.Ok(ctx, repo.Path, "merge-base", "--is-ancestor", "refs/heads/"+wt.Branch, mr.SHA) {
				it.Why = fmt.Sprintf("its merge request !%d is merged and the branch holds nothing beyond it: a candidate for fold gc", mr.IID)
			}
		}
		if it.Why == "" {
			row := -1
			for i, r := range rows {
				for _, p := range r.Prefixes {
					if ok, _ := path.Match(p, wt.Branch); ok || wt.Branch == p {
						row = i
						break
					}
				}
				if row >= 0 {
					break
				}
			}
			if row < 0 && adopting[wt.Branch] {
				it.Lane, it.Agent, it.Action = laneNameFor(wt.Branch), adoptedAgent, "import"
				if it.Scope = inferScope(ctx, repo.Path, wt.Branch, target); len(it.Scope) == 0 {
					it.Scope = []string{"**"}
					it.Why = "adopted; the branch changes nothing yet, so the scope is the whole repo"
				} else {
					it.Why = "adopted; the scope is inferred from what the branch changed: check it"
				}
			} else if row < 0 {
				it.Why = "no row in the table claims a prefix for this branch"
			} else {
				matched[row] = true
				it.Agent, it.Scope, it.Lane = rows[row].Agent, rows[row].Scope, laneNameFor(wt.Branch)
				if len(it.Scope) == 0 {
					if inferred := inferScope(ctx, repo.Path, wt.Branch, target); len(inferred) > 0 {
						it.Scope = inferred
						it.Why = "its row names no paths, so the scope is inferred from what the branch changed: check it"
					} else {
						it.Why = "its row names no paths and the branch changes nothing yet; scope set to the whole repo"
						it.Scope = []string{"**"}
					}
				} else if d := rows[row].Dropped; len(d) > 0 {
					it.Why = "the row also names " + strings.Join(d, ", ") + ", which are not paths: check the scope covers what it meant"
				}
				it.Action = "import"
			}
		}
		if it.Action == "import" && apply {
			origin := store.Origin{Via: store.OriginImport, Detail: "claimed by " + it.Agent}
			if it.Agent == adoptedAgent {
				origin.Detail = "adopted branch"
			}
			l, err := f.Store.CreateLane(ctx, store.Lane{RepoID: repo.ID, Name: it.Lane, Branch: it.Branch, Base: base,
				Worktree: canon, Scope: it.Scope, State: store.LaneOpen, Origin: origin})
			if err != nil {
				it.Action, it.Why = "skip", err.Error()
			} else {
				imported = append(imported, l)
			}
		} else if it.Action == "import" {
			imported = append(imported, store.Lane{Name: it.Lane, Scope: it.Scope})
		}
		plan.Items = append(plan.Items, it)
	}
	for i, r := range rows {
		if !matched[i] {
			plan.Unclaimed = append(plan.Unclaimed, fmt.Sprintf("%s (%s)", r.Agent, strings.Join(r.Prefixes, ", ")))
		}
	}
	files, _ := listFiles(ctx, repo.Path, "HEAD")
	for i := range imported {
		for j := i + 1; j < len(imported); j++ {
			if both := scope.Overlap(imported[i].Scope, imported[j].Scope, files); len(both) > 0 {
				plan.Overlaps = append(plan.Overlaps, fmt.Sprintf("%s and %s on %s", imported[i].Name, imported[j].Name, firstN(both, 3)))
			}
		}
	}
	plan.Applied = apply
	return plan, nil
}
