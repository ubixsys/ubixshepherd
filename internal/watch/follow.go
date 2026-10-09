package watch

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// A follow turns one repo's release into work in another: the framework tags a version,
// and the host that depends on it gets a lane and an agent to move to it. Shepherd
// remembers the last release it acted on per follow, and acts only on releases past
// it. The first look only records where the followed repo is, so turning a follow on
// never sets off work for releases that were already out.

// followKey is the setting holding the last release a follow acted on.
func followKey(follower, upstream int64) string {
	return fmt.Sprintf("follow/%d/%d", follower, upstream)
}

// waitKey is the setting holding the release a follow is waiting on, and why, so the
// thread hears about a wait once rather than every poll.
func waitKey(follower, upstream int64) string {
	return fmt.Sprintf("follow/%d/%d/wait", follower, upstream)
}

// checkFollows looks at the repos a workspace's repos follow, once.
func (w *Watcher) checkFollows(ctx context.Context, repos []store.Repo) {
	byName := map[string]store.Repo{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	latest := map[int64]string{} // upstream repo → its highest release tag, this check
	for _, repo := range repos {
		for _, f := range w.Fold.Conf().Profile(repo.Name).Follows {
			f = f.Effective()
			up, ok := byName[f.Repo]
			if !ok {
				w.once(repo.ID, "follow "+f.Repo, "watch: followed repo is not in the workspace", "repo", repo.Name, "follows", f.Repo)
				continue
			}
			tag, ok := latest[up.ID]
			if !ok {
				var err error
				tag, err = w.highestRelease(ctx, up)
				if err != nil {
					w.Log.Error("watch: release tags", "repo", up.Name, "err", err)
					continue
				}
				latest[up.ID] = tag
			}
			if tag != "" {
				w.follow(ctx, repo, up, f, tag)
			}
		}
	}
}

// once logs a problem the first time it is seen for a repo.
func (w *Watcher) once(repoID int64, what, msg string, args ...any) {
	if w.noted == nil {
		w.noted = map[string]bool{}
	}
	k := fmt.Sprintf("%d %s", repoID, what)
	if !w.noted[k] {
		w.noted[k] = true
		w.Log.Info(msg, args...)
	}
}

// highestRelease is the repo's highest release tag on its remote, or "".
func (w *Watcher) highestRelease(ctx context.Context, repo store.Repo) (string, error) {
	tags, err := fold.RemoteTags(ctx, repo.Path)
	if err != nil {
		return "", err
	}
	prefix := w.Fold.Conf().Profile(repo.Name).TagPrefix
	best, bestV := "", fold.Version{}
	for _, t := range tags {
		if v, ok := fold.ParseTag(prefix, t); ok && (best == "" || bestV.Less(v)) {
			best, bestV = t, v
		}
	}
	return best, nil
}

func (w *Watcher) follow(ctx context.Context, repo, up store.Repo, f config.Follow, tag string) {
	key := followKey(repo.ID, up.ID)
	last, err := w.Store.Setting(ctx, key)
	if err != nil {
		return
	}
	prefix := w.Fold.Conf().Profile(up.Name).TagPrefix
	if last == "" {
		w.Store.SetSetting(ctx, key, tag)
		w.feed(ctx, store.FeedRelease, 0, "%s follows %s from %s: a release after it opens a lane in %s", repo.Name, up.Name, tag, repo.Name)
		return
	}
	lastV, lastOK := fold.ParseTag(prefix, last)
	v, _ := fold.ParseTag(prefix, tag)
	if lastOK && !lastV.Less(v) {
		return
	}
	if lastOK && bumpRank(lastV, v) < bumpRank(fold.Version{}, minBump(f.MinBump)) {
		w.Store.SetSetting(ctx, key, tag)
		w.feed(ctx, store.FeedRelease, 0, "%s released %s; %s follows its %s releases and up, so nothing to do", up.Name, tag, repo.Name, f.MinBump)
		return
	}
	if f.After == config.Published && !w.published(ctx, repo, up, tag) {
		return
	}
	w.Store.SetSetting(ctx, key, tag)
	w.Store.SetSetting(ctx, waitKey(repo.ID, up.ID), "")
	w.startFollow(ctx, repo, up, f, last, tag, v)
}

// published reports whether the tag's pipeline passed, telling the thread once about a
// wait or a failure.
func (w *Watcher) published(ctx context.Context, repo, up store.Repo, tag string) bool {
	fg, err := w.forgeFor(up)
	if err != nil {
		w.once(up.ID, "follow forge", "watch: cannot check releases are published; set after: tagged to follow tags alone", "repo", up.Name, "why", err)
		return false
	}
	p, err := fg.RefPipeline(ctx, tag)
	if err != nil {
		w.Log.Error("watch: tag pipeline", "repo", up.Name, "tag", tag, "err", err)
		return false
	}
	if p != nil && p.Status == "success" {
		return true
	}
	state, say := "no pipeline yet", fmt.Sprintf("%s released %s; waiting for its pipeline before %s moves to it", up.Name, tag, repo.Name)
	if p != nil {
		state = p.Status
		switch p.Status {
		case "failed", "canceled":
			say = fmt.Sprintf("%s's pipeline for %s %s (%s); %s waits until a run of it passes", up.Name, tag, p.Status, p.URL, repo.Name)
		default:
			say = fmt.Sprintf("%s released %s; its pipeline is %s, and %s moves to it once it passes", up.Name, tag, p.Status, repo.Name)
		}
	}
	// Say it when the wait changes kind, not on every poll.
	kind := tag + " " + waitKind(state)
	wk := waitKey(repo.ID, up.ID)
	if prev, _ := w.Store.Setting(ctx, wk); prev != kind {
		w.Store.SetSetting(ctx, wk, kind)
		w.feed(ctx, store.FeedRelease, 0, "%s", say)
	}
	return false
}

func waitKind(status string) string {
	switch status {
	case "failed", "canceled":
		return "failed"
	}
	return "waiting"
}

// startFollow opens the follower's lane for a release and hands it to the agent.
func (w *Watcher) startFollow(ctx context.Context, repo, up store.Repo, f config.Follow, last, tag string, v fold.Version) {
	expand := strings.NewReplacer(
		"{repo}", up.Name, "{tag}", tag, "{version}", v.String(), "{previous}", last,
		"{major}", strconv.Itoa(v.Major), "{minor}", strconv.Itoa(v.Minor), "{patch}", strconv.Itoa(v.Patch),
	).Replace
	name := expand(f.Lane)
	opened, err := w.Fold.Open(ctx, fold.OpenRequest{RepoID: repo.ID, Name: name, Scope: f.Scope,
		Origin: store.Origin{Via: store.OriginFollow, Detail: "release " + up.Name + " " + tag}})
	if err != nil {
		w.feed(ctx, store.FeedRelease, 0, "%s released %s, but lane %s could not open in %s: %s", up.Name, tag, name, repo.Name, firstLine(err.Error()))
		return
	}
	lane := opened.Lane
	w.feed(ctx, store.FeedLaneOpened, lane.ID, "%s released %s: lane %s opened in %s (scope %s)", up.Name, tag, lane.Name, repo.Name, strings.Join(lane.Scope, ", "))
	if err := w.Fold.WriteView(ctx, repo.ID); err != nil {
		w.Log.Error("watch: coordination view", "err", err)
	}
	if w.Runner == nil {
		return
	}
	prompt := fmt.Sprintf("[Shepherd] %s released %s (the last release this repo moved to was %s).\n\n%s%s",
		up.Name, tag, last, releaseNotes(ctx, up.Path, last, tag), expand(f.Task))
	run, err := w.Runner.Start(ctx, dispatch.StartRequest{LaneID: lane.ID, Agent: f.Agent, Model: f.Model, Prompt: prompt, NewSession: true, Auto: true})
	if err != nil {
		w.feed(ctx, store.FeedRelease, lane.ID, "lane %s is open, but %s did not start: %s (start it with shepherd lane run %s --agent %s)", lane.Name, f.Agent, firstLine(err.Error()), lane.Name, f.Agent)
		return
	}
	w.feed(ctx, store.FeedRelease, lane.ID, "asked %s to move %s to %s %s (run %d)", f.Agent, repo.Name, up.Name, tag, run.ID)
}

// releaseNotes is what changed between two releases, from the followed repo's own
// history: the tag's message and the commit subjects, for the agent's commit message.
func releaseNotes(ctx context.Context, dir, last, tag string) string {
	git.Run(ctx, dir, "fetch", "--quiet", "origin", "refs/tags/"+tag+":refs/tags/"+tag, "refs/tags/"+last+":refs/tags/"+last)
	var b strings.Builder
	if msg, err := git.Run(ctx, dir, "tag", "-l", "--format=%(contents)", tag); err == nil && strings.TrimSpace(msg) != "" {
		fmt.Fprintf(&b, "The tag's message:\n%s\n\n", clipLines(strings.TrimSpace(msg), 40))
	}
	if log, err := git.Run(ctx, dir, "log", "--no-merges", "--format=- %s", last+".."+tag); err == nil && strings.TrimSpace(log) != "" {
		fmt.Fprintf(&b, "Commits since %s:\n%s\n\n", last, clipLines(log, 40))
	}
	return b.String()
}

func clipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n... and %d more", len(lines)-n)
}

// bumpRank is how big the step from a to b is: 3 major, 2 minor, 1 patch, 0 none.
func bumpRank(a, b fold.Version) int {
	switch {
	case a.Major != b.Major:
		return 3
	case a.Minor != b.Minor:
		return 2
	case a.Patch != b.Patch:
		return 1
	}
	return 0
}

// minBump is the smallest version a bump kind reaches from zero, for bumpRank.
func minBump(kind string) fold.Version {
	v, _ := fold.Version{}.Bump(kind)
	return v
}
