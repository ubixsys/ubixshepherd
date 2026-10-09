package fold

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// Review verdicts.
const (
	// Finished: everything on the branch is in the base, or in a merged request.
	Finished = "finished"
	// Live: uncommitted work, an open request, or recent activity.
	Live = "live"
	// Unclear: work that is in neither the base nor a merged request, with no recent
	// activity. The person decides.
	Unclear = "unclear"
)

// RecentWindow is how recent a commit or a conversation must be for a branch with
// unlanded work to count as live.
const RecentWindow = 14 * 24 * time.Hour

// Review is one worktree's verdict, with the evidence for it.
type Review struct {
	Branch   string   `json:"branch"`
	Worktree string   `json:"worktree"`
	Lane     string   `json:"lane,omitempty"`
	Verdict  string   `json:"verdict"`
	Why      string   `json:"why"`
	Evidence []string `json:"evidence,omitempty"`
	// Unlanded are the subjects of commits whose change is in neither the base nor a
	// merged request.
	Unlanded []string `json:"unlanded,omitempty"`
}

// ReviewRepo judges each of a repo's worktrees (lanes or not): finished, live, or
// unclear. It changes nothing.
func (f *Fold) ReviewRepo(ctx context.Context, repoID int64) ([]Review, error) {
	return f.review(ctx, repoID, "", true)
}

// review judges the repo's worktrees, or only the one at only. fetch refreshes the
// remote first; a retire skips it, since fetching cannot make finished work unfinished.
func (f *Fold) review(ctx context.Context, repoID int64, only string, fetch bool) ([]Review, error) {
	repo, err := f.Store.Repo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	wts, err := git.Worktrees(ctx, repo.Path)
	if err != nil {
		return nil, err
	}
	if fetch {
		git.Run(ctx, repo.Path, "fetch", "--quiet", "origin")
	}
	prof := f.Conf().Profile(repo.Name)
	target := prof.BaseBranch
	if git.RefExists(ctx, repo.Path, "refs/remotes/origin/"+target) {
		target = "origin/" + target
	}
	lanes, _ := f.Store.Lanes(ctx, repo.ID)
	laneAt := map[string]store.Lane{}
	for _, l := range lanes {
		laneAt[l.Worktree] = l
	}
	var fg forge.Forge
	if f.ForgeFor != nil {
		fg, _ = f.ForgeFor(repo.Remote)
	}
	convs, _ := f.Store.Conversations(ctx, repo.ID)
	logText := coordLog(repo, prof.CoordFile)

	var out []Review
	for _, wt := range wts[1:] {
		if wt.Bare {
			continue
		}
		canon, _ := paths.Canonical(wt.Path)
		if only != "" && canon != only {
			continue
		}
		r := Review{Branch: wt.Branch, Worktree: canon}
		if l, ok := laneAt[canon]; ok {
			r.Lane = l.Name
		}
		f.judge(ctx, repo, target, fg, convs, logText, wt, &r)
		out = append(out, r)
	}
	return out, nil
}

func (f *Fold) judge(ctx context.Context, repo store.Repo, target string, fg forge.Forge, convs []store.Conversation, logText string, wt git.Worktree, r *Review) {
	if _, err := os.Stat(wt.Path); err != nil {
		r.Verdict, r.Why = Finished, "the worktree is gone from disk; git still lists it (git worktree prune clears it)"
		return
	}
	if dirty, _ := git.Dirty(ctx, wt.Path); dirty != "" {
		r.Verdict, r.Why = Live, "it has uncommitted changes"
		r.Evidence = append(r.Evidence, "uncommitted: "+firstN(strings.Split(dirty, "\n"), 5))
		return
	}
	if wt.Branch == "" {
		r.Verdict, r.Why = Unclear, "detached HEAD: no branch to judge"
		return
	}

	// Commits whose change is not in the base, by content (a rebase or squash landing
	// still counts). A root commit is a rewritten history's start, not work.
	var unlanded []string
	if out, err := git.Run(ctx, repo.Path, "cherry", "-v", target, "refs/heads/"+wt.Branch); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "+ ") {
				continue
			}
			fields := strings.SplitN(strings.TrimPrefix(line, "+ "), " ", 2)
			if parents, _ := git.Run(ctx, repo.Path, "rev-list", "--parents", "-n", "1", fields[0]); len(strings.Fields(parents)) == 1 {
				r.Evidence = append(r.Evidence, "ignored a root commit ("+shortSHA(fields[0])+"), from history before a rewrite")
				continue
			}
			subject := ""
			if len(fields) == 2 {
				subject = fields[1]
			}
			unlanded = append(unlanded, shortSHA(fields[0])+" "+subject)
		}
	}
	lastCommit := time.Time{}
	if ts, err := git.Run(ctx, repo.Path, "log", "-1", "--format=%cI", "refs/heads/"+wt.Branch); err == nil {
		lastCommit, _ = time.Parse(time.RFC3339, ts)
		r.Evidence = append(r.Evidence, "last commit "+lastCommit.Local().Format("2006-01-02"))
	}

	var mr *forge.MR
	if fg != nil {
		mr, _ = fg.MRForBranch(ctx, wt.Branch)
		if mr != nil {
			r.Evidence = append(r.Evidence, fmt.Sprintf("merge request !%d is %s", mr.IID, mr.State))
		} else {
			r.Evidence = append(r.Evidence, "no merge request")
		}
	}
	if git.RefExists(ctx, repo.Path, "refs/remotes/origin/"+wt.Branch) {
		r.Evidence = append(r.Evidence, "branch is on the remote")
	} else {
		r.Evidence = append(r.Evidence, "branch was never pushed, or its remote branch is gone")
	}
	var lastTalk time.Time
	for _, c := range convs {
		for _, b := range c.Branches {
			if b == wt.Branch && c.Last.After(lastTalk) {
				lastTalk = c.Last
				r.Evidence = append(r.Evidence, fmt.Sprintf("conversation %s worked on it, last %s", c.ID[:8], c.Last.Local().Format("2006-01-02")))
			}
		}
	}
	if logText != "" {
		if n := strings.Count(logText, wt.Branch); n > 0 {
			r.Evidence = append(r.Evidence, fmt.Sprintf("the coordination log mentions it %d time(s)", n))
		}
	}

	recent := time.Since(lastCommit) < RecentWindow || time.Since(lastTalk) < RecentWindow
	inMR := mr != nil && mr.State == "merged" && mr.SHA != "" &&
		git.Ok(ctx, repo.Path, "merge-base", "--is-ancestor", "refs/heads/"+wt.Branch, mr.SHA)
	switch {
	case len(unlanded) == 0:
		r.Verdict, r.Why = Finished, "every change on it is in "+target
	case inMR:
		r.Verdict, r.Why = Finished, fmt.Sprintf("its merge request !%d is merged and the branch holds nothing beyond it", mr.IID)
	case mr != nil && mr.State == "opened":
		r.Verdict, r.Why = Live, fmt.Sprintf("its merge request !%d is open", mr.IID)
	case recent:
		r.Verdict, r.Why = Live, fmt.Sprintf("%d change(s) not in %s, with activity in the last %d days", len(unlanded), target, int(RecentWindow.Hours()/24))
	default:
		r.Verdict, r.Why = Unclear, fmt.Sprintf("%d change(s) in neither %s nor a merged request, and nothing recent", len(unlanded), target)
	}
	if r.Verdict != Finished {
		r.Unlanded = unlanded
		if len(r.Unlanded) > 5 {
			r.Unlanded = append(r.Unlanded[:5:5], fmt.Sprintf("... and %d more", len(unlanded)-5))
		}
	}
}

// coordLog is the repo's coordination file, for mentions of a branch.
func coordLog(repo store.Repo, file string) string {
	if file == "" {
		file = "AGENTS-COORD.md"
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(repo.Path, file)
	}
	b, _ := os.ReadFile(file)
	return string(b)
}

// Retire removes a finished worktree that is not an open lane. It keeps the branch, and
// refuses anything ReviewRepo does not call finished.
func (f *Fold) Retire(ctx context.Context, repoID int64, worktree string) (Review, error) {
	canon, _ := paths.Canonical(worktree)
	reviews, err := f.review(ctx, repoID, canon, false)
	if err != nil {
		return Review{}, err
	}
	for _, r := range reviews {
		if r.Worktree != canon {
			continue
		}
		switch {
		case r.Lane != "":
			return r, refuse("%s is lane %s; close the lane instead (shepherd lane close)", canon, r.Lane)
		case r.Verdict != Finished:
			return r, refuse("%s is %s (%s); only a finished worktree is retired", canon, r.Verdict, r.Why)
		}
		repo, err := f.Store.Repo(ctx, repoID)
		if err != nil {
			return r, err
		}
		lock := f.repoLock(repo.ID)
		lock.Lock()
		defer lock.Unlock()
		if _, err := os.Stat(canon); err != nil {
			_, err := git.Run(ctx, repo.Path, "worktree", "prune")
			return r, err
		}
		_, err = git.Run(ctx, repo.Path, "worktree", "remove", canon)
		return r, err
	}
	return Review{}, refuse("%s is not one of the repo's worktrees", canon)
}
