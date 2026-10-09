// Package watch keeps lanes in step with their forge: it polls the merge request for
// each open lane's branch and acts on what changed. A merged request is proof, and the
// lane closes; a failed pipeline goes back to the lane's agent to fix; everything else
// is a line in the person's thread. It also watches the repos others follow, and a
// release of one opens a lane in each follower (follow.go). Polling, because a Shepherd on a laptop cannot
// receive webhooks.
package watch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// MaxFixTries is how many times Shepherd hands a failed pipeline back to the lane's agent
// on its own before leaving it to the person.
const MaxFixTries = 2

// Watcher polls forges for open lanes.
type Watcher struct {
	Store    store.Store
	Fold     *fold.Fold
	Runner   *dispatch.Runner
	Log      *slog.Logger
	Interval time.Duration
	// ForgeFor returns a repo's forge; tests replace it.
	ForgeFor func(remote string) (forge.Forge, error)
	// Clock is the time, for the backoff from an unreachable host; tests replace it.
	Clock func() time.Time

	mu      sync.Mutex
	skipped map[int64]bool        // repos with no readable forge, logged once
	noted   map[string]bool       // follow problems, logged once
	down    map[string]*hostState // forge hosts that did not answer, by host (backoff.go)
}

// Run checks every Interval until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		w.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check looks at every open lane, and every followed repo's releases, once.
func (w *Watcher) Check(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.skipped == nil {
		w.skipped = map[int64]bool{}
	}
	wss, err := w.Store.Workspaces(ctx)
	if err != nil {
		w.Log.Error("watch: workspaces", "err", err)
		return
	}
	for _, ws := range wss {
		repos, err := w.Store.Repos(ctx, ws.ID)
		if err != nil {
			continue
		}
		for _, repo := range repos {
			lanes, err := w.Store.Lanes(ctx, repo.ID)
			if err != nil || len(lanes) == 0 {
				continue
			}
			f, err := w.forgeFor(repo)
			if err != nil {
				if !w.skipped[repo.ID] {
					w.skipped[repo.ID] = true
					w.Log.Info("watch: not watching repo", "repo", repo.Name, "why", err)
				}
				continue
			}
			host := hostOf(repo)
			for _, lane := range lanes {
				if lane.State != store.LaneOpen {
					continue
				}
				if w.skipHost(host) || !w.result(host, w.checkLane(ctx, f, lane)) {
					break
				}
			}
		}
		w.checkFollows(ctx, repos)
	}
}

func (w *Watcher) forgeFor(repo store.Repo) (forge.Forge, error) {
	if w.ForgeFor != nil {
		return w.ForgeFor(repo.Remote)
	}
	return forge.For(repo.Remote)
}

func (w *Watcher) feed(ctx context.Context, kind string, ref int64, format string, a ...any) {
	if err := w.Store.AddFeed(ctx, kind, redact.String(fmt.Sprintf(format, a...)), ref); err != nil {
		w.Log.Error("watch: feed", "err", err)
	}
}

// checkLane reads a lane's merge request and acts on what changed. It returns the
// forge's error, for the backoff; one from a host that answered is logged here.
func (w *Watcher) checkLane(ctx context.Context, f forge.Forge, lane store.Lane) error {
	mr, err := f.MRForBranch(ctx, lane.Branch)
	if err != nil {
		if !forge.Unreachable(err) {
			w.Log.Error("watch: merge request", "lane", lane.Name, "err", err)
		}
		return err
	}
	if mr == nil {
		return nil
	}
	prev, err := w.Store.LaneForge(ctx, lane.ID)
	if err != nil {
		return nil
	}
	next := prev
	next.MR, next.MRState, next.MRURL = mr.IID, mr.State, mr.URL
	if mr.IID != prev.MR {
		next.FixTries, next.Pipeline, next.PipelineStatus = 0, 0, ""
		w.feed(ctx, store.FeedMR, lane.ID, "!%d %s for lane %s: %s", mr.IID, mr.State, lane.Name, mr.URL)
	}

	if p := mr.Pipeline; p != nil && mr.State == "opened" && (p.ID != prev.Pipeline || p.Status != prev.PipelineStatus) {
		next.Pipeline, next.PipelineStatus = p.ID, p.Status
		switch p.Status {
		case "success":
			w.feed(ctx, store.FeedPipeline, lane.ID, "pipeline %d passed for !%d (lane %s)", p.ID, mr.IID, lane.Name)
		case "failed":
			next.FixTries = w.pipelineFailed(ctx, f, lane, mr, p, next.FixTries)
		case "canceled":
			w.feed(ctx, store.FeedPipeline, lane.ID, "pipeline %d was canceled for !%d (lane %s)", p.ID, mr.IID, lane.Name)
		}
	}

	// Act only on a merge seen happening. A request already merged the first time
	// Shepherd looks (an imported lane, a branch reused after its merge) proves nothing
	// about the work in the lane now, so it is reported, never acted on.
	firstSight := prev.MR == 0 || mr.IID != prev.MR
	switch {
	case mr.State == "merged" && firstSight:
		next.MergeSHA = mr.MergeSHA
		w.feed(ctx, store.FeedMR, lane.ID, "!%d for lane %s was already merged when Shepherd first looked; the lane stays open (close it with shepherd lane close when its work is done)", mr.IID, lane.Name)
	case mr.State == "merged" && prev.MRState != "merged":
		next.MergeSHA = mr.MergeSHA
		w.merged(ctx, lane, mr)
	}
	if mr.State == "closed" && prev.MRState != "closed" && mr.IID == prev.MR {
		w.feed(ctx, store.FeedMR, lane.ID, "!%d was closed without merging; lane %s stays open", mr.IID, lane.Name)
	}
	if next != prev {
		if err := w.Store.PutLaneForge(ctx, next); err != nil {
			w.Log.Error("watch: save", "lane", lane.Name, "err", err)
		}
	}
	return nil
}

// merged closes the lane on the forge's proof, or says why it cannot.
func (w *Watcher) merged(ctx context.Context, lane store.Lane, mr *forge.MR) {
	if mr.MergeSHA == "" {
		w.feed(ctx, store.FeedMR, lane.ID, "!%d is merged but the forge gives no merge commit yet; lane %s stays open until it does", mr.IID, lane.Name)
		return
	}
	for _, p := range w.Fold.VerifyTags(ctx, lane, mr.MergeSHA) {
		w.feed(ctx, store.FeedMR, lane.ID, "!%d merged, but %s", mr.IID, p)
	}
	res, err := w.Fold.CloseMerged(ctx, lane.ID, mr.MergeSHA, mr.SHA)
	if err != nil {
		w.feed(ctx, store.FeedMR, lane.ID, "!%d merged at %s, but lane %s cannot close: %s", mr.IID, short(mr.MergeSHA), lane.Name, firstLine(err.Error()))
		return
	}
	w.Log.Info("watch: lane closed on merge", "lane", lane.Name, "mr", mr.IID, "merge", mr.MergeSHA)
	if err := w.Fold.WriteView(ctx, lane.RepoID); err != nil {
		w.Log.Error("watch: coordination view", "err", err)
	}
	note := ""
	if res.BranchDeleted {
		note = ", local branch deleted"
	}
	w.feed(ctx, store.FeedLaneClosed, lane.ID, "!%d merged at %s; lane %s closed%s", mr.IID, short(mr.MergeSHA), lane.Name, note)
}

// pipelineFailed hands a failed pipeline to the lane's agent while tries remain, and to
// the person after. It returns the tries used.
func (w *Watcher) pipelineFailed(ctx context.Context, f forge.Forge, lane store.Lane, mr *forge.MR, p *forge.Pipeline, tries int) int {
	jobs, err := f.FailedJobs(ctx, p.ID)
	if err != nil {
		w.Log.Error("watch: failed jobs", "lane", lane.Name, "err", err)
	}
	var names []string
	var logs strings.Builder
	for _, j := range jobs {
		names = append(names, j.Name)
		tail, err := f.JobLog(ctx, j.ID, 60)
		if err != nil {
			tail = "(could not read the log: " + err.Error() + ")"
		}
		fmt.Fprintf(&logs, "== job %s (%s) ==\n%s\n\n", j.Name, j.URL, tail)
	}
	what := strings.Join(names, ", ")
	if what == "" {
		what = "no failed job listed"
	}
	w.feed(ctx, store.FeedPipeline, lane.ID, "pipeline %d failed for !%d (lane %s): %s", p.ID, mr.IID, lane.Name, what)

	runs, _ := w.Store.Runs(ctx, lane.ID, "", 20)
	var last *store.Run
	for i := range runs {
		if runs[i].State == store.RunRunning {
			w.feed(ctx, store.FeedPipeline, lane.ID, "lane %s has an agent running; the failure waits for you", lane.Name)
			return tries
		}
		if last == nil && runs[i].Session != "" {
			last = &runs[i]
		}
	}
	switch {
	case w.Runner == nil || last == nil:
		w.feed(ctx, store.FeedPipeline, lane.ID, "lane %s has no agent conversation to hand the failure to; it is yours", lane.Name)
		return tries
	case tries >= MaxFixTries:
		w.feed(ctx, store.FeedPipeline, lane.ID, "pipeline failed again after %d fixes by %s; lane %s is yours now", tries, last.Agent, lane.Name)
		return tries
	}
	prompt := fmt.Sprintf("[Shepherd] The pipeline for your merge request !%d failed (pipeline %d, %s).\n\n%s"+
		"Find the cause and fix it within your scope, run the gate, and commit. Do not push: the person pushes the fix.",
		mr.IID, p.ID, p.URL, logs.String())
	// Count the try before starting it, so a fix that ends fast is checked against it.
	if lf, err := w.Store.LaneForge(ctx, lane.ID); err == nil {
		lf.FixTries = tries + 1
		w.Store.PutLaneForge(ctx, lf)
	}
	run, err := w.Runner.Start(ctx, dispatch.StartRequest{Continue: last.ID, Prompt: prompt, Auto: true})
	if err != nil {
		if lf, err := w.Store.LaneForge(ctx, lane.ID); err == nil {
			lf.FixTries = tries
			w.Store.PutLaneForge(ctx, lf)
		}
		w.feed(ctx, store.FeedPipeline, lane.ID, "could not hand the failure to %s in lane %s: %s", last.Agent, lane.Name, firstLine(err.Error()))
		return tries
	}
	tries++
	w.feed(ctx, store.FeedPipeline, lane.ID, "asked %s to fix it (run %d, try %d of %d); push its fix when it is done", last.Agent, run.ID, tries, MaxFixTries)
	return tries
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
