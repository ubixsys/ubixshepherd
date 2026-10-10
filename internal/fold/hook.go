package fold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/scope"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// PushRef is one line git gives a pre-push hook on stdin.
type PushRef struct {
	LocalRef  string `json:"local_ref"`
	LocalSHA  string `json:"local_sha"`
	RemoteRef string `json:"remote_ref"`
	RemoteSHA string `json:"remote_sha"`
}

// ParsePushRefs reads a pre-push hook's stdin.
func ParsePushRefs(in string) ([]PushRef, error) {
	var refs []PushRef
	for _, line := range strings.Split(strings.TrimSpace(in), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 {
			return nil, fmt.Errorf("pre-push input %q: want 4 fields", line)
		}
		refs = append(refs, PushRef{f[0], f[1], f[2], f[3]})
	}
	return refs, nil
}

func zero(sha string) bool { return strings.Trim(sha, "0") == "" }

// Verdict answers a pre-push check.
type Verdict struct {
	OK bool `json:"ok"`
	// Lane is empty when the push is not from a lane, which Shepherd leaves alone.
	Lane     string   `json:"lane,omitempty"`
	Problems []string `json:"problems,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// PushAgent is the autonomy.push value that lets an agent push its own lane branch. It is
// compared as a string so this works whichever config package version is built.
const PushAgent = "agent"

// CheckPush decides whether a push may go ahead, without knowing where it goes. While an
// agent runs in the lane that means refusing, since the remote of an agent's push cannot
// be verified. See CheckPushTo.
func (f *Fold) CheckPush(ctx context.Context, lane *store.Lane, repo *store.Repo, dir string, refs []PushRef) (Verdict, error) {
	return f.CheckPushTo(ctx, lane, repo, dir, "", refs)
}

// CheckPushTo decides whether a push to remote (the name or URL git hands the hook, "" if
// unknown) may go ahead. From a lane: only the lane's branch, only changes inside its
// scope, no commit message the repo forbids, and no push while an agent runs in it,
// unless the repo's profile says autonomy.push: agent, which lets the agent push the
// lane's own branch to the repo's origin. From any checkout of a repo whose tags are
// reserved: release tags must be reserved, and contain their lane's merge. Everything
// else is left alone.
func (f *Fold) CheckPushTo(ctx context.Context, lane *store.Lane, repo *store.Repo, dir, remote string, refs []PushRef) (Verdict, error) {
	v := Verdict{OK: true}
	var prof config.Profile
	if repo != nil {
		prof = f.Conf().Profile(repo.Name)
	}
	if lane != nil {
		v.Lane = lane.Name
		if running, err := f.Store.Runs(ctx, lane.ID, store.RunRunning, 1); err == nil && len(running) > 0 {
			switch {
			case repo == nil || prof.Autonomy.Push != PushAgent:
				v.Problems = append(v.Problems, fmt.Sprintf("agent run %d (%s) is going in lane %s; agents Shepherd starts never push. Review the lane's commits when it ends, then push yourself",
					running[0].ID, running[0].Agent, lane.Name))
			case !isOrigin(ctx, dir, remote):
				v.Problems = append(v.Problems, fmt.Sprintf("agent run %d (%s) in lane %s may push only to the repo's origin; this pushes to %q",
					running[0].ID, running[0].Agent, lane.Name, remote))
			}
			if len(v.Problems) > 0 {
				v.OK = false
				return v, nil
			}
		}
	}
	reserved := repo != nil && prof.Tags == config.TagsReserved
	for _, r := range refs {
		if zero(r.LocalSHA) {
			continue // deleting a remote ref
		}
		if strings.HasPrefix(r.RemoteRef, "refs/tags/") {
			if !reserved {
				continue
			}
			problem, err := f.checkTag(ctx, *repo, lane, dir, strings.TrimPrefix(r.RemoteRef, "refs/tags/"), r.LocalSHA)
			if err != nil {
				return v, err
			}
			if problem != "" {
				v.Problems = append(v.Problems, problem)
			}
			continue
		}
		if lane == nil {
			continue
		}
		if r.RemoteRef != "refs/heads/"+lane.Branch {
			v.Problems = append(v.Problems, fmt.Sprintf("lane %s pushes only its branch %s; this pushes to %s",
				lane.Name, lane.Branch, strings.TrimPrefix(r.RemoteRef, "refs/heads/")))
			continue
		}
		from, note, err := pushBase(ctx, dir, lane, r)
		if err != nil {
			return v, err
		}
		if note != "" {
			v.Notes = append(v.Notes, note)
		}
		out, err := git.Run(ctx, dir, "diff", "--name-only", "--no-renames", from, r.LocalSHA)
		if err != nil {
			return v, err
		}
		var outside []string
		for _, file := range strings.Split(out, "\n") {
			if file != "" && !scope.Any(lane.Scope, file) {
				outside = append(outside, file)
			}
		}
		if len(outside) > 0 {
			v.Problems = append(v.Problems, fmt.Sprintf("changes outside lane %s's scope (%s):\n    %s",
				lane.Name, strings.Join(lane.Scope, ", "), strings.Join(firstLinesN(outside, 20), "\n    ")))
		}
		if repo != nil {
			bad, err := forbiddenMessage(ctx, dir, newCommitsFrom(ctx, dir, from, r), r.LocalSHA, prof.Forbid)
			if err != nil {
				return v, err
			}
			if bad != "" {
				v.Problems = append(v.Problems, bad)
			}
		}
	}
	v.OK = len(v.Problems) == 0
	return v, nil
}

// isOrigin reports whether remote, as git names it to a hook (a remote name or a URL),
// is the checkout's origin. An empty remote is not verified, so it is not origin.
func isOrigin(ctx context.Context, dir, remote string) bool {
	if remote == "" {
		return false
	}
	if remote == "origin" {
		return true
	}
	for _, args := range [][]string{{"remote", "get-url", "origin"}, {"remote", "get-url", "--push", "origin"}} {
		if u, err := git.Run(ctx, dir, args...); err == nil && u == remote {
			return true
		}
	}
	return false
}

// forbiddenMessage checks the messages of the commits from..to against the repo's forbid
// patterns and describes the first match, or returns "".
func forbiddenMessage(ctx context.Context, dir, from, to string, patterns []string) (string, error) {
	if len(patterns) == 0 {
		return "", nil
	}
	out, err := git.Run(ctx, dir, "log", "--format=%h%x00%B%x01", from+".."+to)
	if err != nil {
		return "", err
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		for _, c := range strings.Split(out, "\x01") {
			sha, msg, _ := strings.Cut(strings.TrimSpace(c), "\x00")
			if m := re.FindString(msg); m != "" {
				return fmt.Sprintf("commit %s's message contains %q, which this repo does not allow (pattern %s)", sha, m, p), nil
			}
		}
	}
	return "", nil
}

// newCommitsFrom is where the commits this push adds start, for judging messages: the
// remote's tip when the push fast-forwards it, so commits already there are not judged
// again, and otherwise (new branch, rebase) the lane's fork point from base.
func newCommitsFrom(ctx context.Context, dir, forkPoint string, r PushRef) string {
	if !zero(r.RemoteSHA) && git.Ok(ctx, dir, "merge-base", "--is-ancestor", r.RemoteSHA, r.LocalSHA) {
		return r.RemoteSHA
	}
	return forkPoint
}

// pushBase is where the lane's own changes start: the merge-base of the pushed tip and
// the repo's base branch on origin. It is never the remote's old tip for the branch: after
// a rebase and force push the old tip is on the other side of every commit the base
// gained, so a diff from it would count the base's work as the lane's.
//
// The base is fetched first, so a rebase onto a commit the local origin/<base> has not
// seen yet still finds the new fork point; a stale ref would put the merge-base back
// before it. If the fetch fails the local ref is used and the note says the check may
// over-report. With no base ref at all it falls back to the remote's old tip, then to the
// root, which over-reports rather than waving anything through.
func pushBase(ctx context.Context, dir string, lane *store.Lane, r PushRef) (from, note string, err error) {
	base := lane.Base
	if git.HasRemote(ctx, dir, "origin") {
		if _, ferr := git.Run(ctx, dir, "fetch", "--quiet", "origin", base); ferr != nil {
			note = fmt.Sprintf("could not fetch origin/%s (%v); judging scope against the last known copy, which may be stale", base, ferr)
		}
		if git.RefExists(ctx, dir, "refs/remotes/origin/"+base) {
			base = "origin/" + base
		}
	}
	if git.RefExists(ctx, dir, base) {
		if mb, merr := git.Run(ctx, dir, "merge-base", base, r.LocalSHA); merr == nil && mb != "" {
			return mb, note, nil
		}
	}
	if !zero(r.RemoteSHA) && git.Ok(ctx, dir, "cat-file", "-e", r.RemoteSHA+"^{commit}") {
		return r.RemoteSHA, note, nil
	}
	return "", "", fmt.Errorf("no base branch %s to judge lane %s's push against", lane.Base, lane.Name)
}

func firstLinesN(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("... and %d more", len(s)-n))
	}
	return s
}

// hookMarker identifies a hook Shepherd wrote, so it may replace or remove it.
const hookMarker = "Installed by uBixShepherd"

// HookScript is the pre-push hook. It prefers the binary that installed it and falls
// back to shepherd on PATH, so a moved binary degrades to PATH instead of breaking.
func HookScript(exe string) []byte {
	return []byte(`#!/bin/sh
# ` + hookMarker + ` (shepherd hook install). Refuses a lane's push that leaves its
# branch or its scope. Skip once with git push --no-verify; remove with shepherd hook uninstall.
SHEPHERD=` + shQuote(exe) + `
[ -x "$SHEPHERD" ] || SHEPHERD=shepherd
exec "$SHEPHERD" hook pre-push "$@"
`)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// HookLine is what to add to an existing pre-push hook to chain Shepherd's check.
const HookLine = `shepherd hook pre-push "$@" || exit 1`

// HookState describes a repo's pre-push hook.
type HookState struct {
	Path string `json:"path"`
	// Ours: Shepherd's hook is installed. Foreign: another pre-push hook is there.
	// Chained: that other hook already runs Shepherd's check.
	Ours    bool `json:"ours"`
	Foreign bool `json:"foreign"`
	Chained bool `json:"chained,omitempty"`
	// Tracked: the hooks directory is outside .git (core.hooksPath), usually a tracked
	// directory in the work tree, so a hook written there is a change to commit.
	Tracked bool `json:"tracked"`
}

// Hook reports the state of a repo's pre-push hook.
func Hook(ctx context.Context, repo string) (HookState, error) {
	rel, err := git.Run(ctx, repo, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return HookState{}, err
	}
	dir := rel
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repo, rel)
	}
	common, err := git.Run(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return HookState{}, err
	}
	cdir, _ := paths.Canonical(dir)
	ccommon, _ := paths.Canonical(common)
	st := HookState{Path: filepath.Join(cdir, "pre-push"), Tracked: !paths.Within(ccommon, cdir)}
	b, err := os.ReadFile(st.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return st, err
	case bytes.Contains(b, []byte(hookMarker)):
		st.Ours = true
	default:
		st.Foreign = true
		st.Chained = bytes.Contains(b, []byte("shepherd hook pre-push"))
	}
	return st, nil
}

// InstallHook writes Shepherd's pre-push hook. It never replaces another hook; with
// another one there it returns an error saying what to add to it.
func InstallHook(ctx context.Context, repo, exe string) (HookState, error) {
	st, err := Hook(ctx, repo)
	if err != nil {
		return st, err
	}
	if st.Foreign {
		return st, refuse("%s already has a pre-push hook that is not Shepherd's. Add this line to it:\n    %s", st.Path, HookLine)
	}
	if err := os.MkdirAll(filepath.Dir(st.Path), 0o755); err != nil {
		return st, err
	}
	if err := os.WriteFile(st.Path, HookScript(exe), 0o755); err != nil {
		return st, err
	}
	st.Ours = true
	return st, nil
}

// UninstallHook removes Shepherd's hook, and nothing else.
func UninstallHook(ctx context.Context, repo string) (HookState, error) {
	st, err := Hook(ctx, repo)
	if err != nil || !st.Ours {
		return st, err
	}
	if err := os.Remove(st.Path); err != nil {
		return st, err
	}
	st.Ours = false
	return st, nil
}

// ensureHook installs the hook when that is safe without asking: no pre-push hook yet,
// in a hooks directory git keeps out of the work tree. Otherwise it says what to do.
// It looks from the lane's worktree: with a relative core.hooksPath (a tracked
// .githooks), each worktree runs its own checked-out copy of the hook.
func (f *Fold) ensureHook(ctx context.Context, repo, worktree string) string {
	if f.Exe == "" {
		return ""
	}
	st, err := Hook(ctx, worktree)
	switch {
	case err != nil:
		return "could not check the pre-push hook: " + err.Error()
	case st.Ours, st.Chained:
		return ""
	case st.Foreign:
		return fmt.Sprintf("scope is not enforced on push: %s is another hook. Add to it: %s", st.Path, HookLine)
	case st.Tracked:
		return fmt.Sprintf("scope is not enforced on push: this repo's hooks live outside .git (%s), so Shepherd does not write there unasked. Run shepherd hook install, and commit the hook if that directory is tracked", filepath.Dir(st.Path))
	}
	if _, err := InstallHook(ctx, worktree, f.Exe); err != nil {
		return "could not install the pre-push hook: " + err.Error()
	}
	return "installed the pre-push hook at " + st.Path
}
