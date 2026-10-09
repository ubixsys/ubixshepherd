package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// Request kinds.
const (
	KindQuestion = "question"
	KindHandoff  = "handoff"
	KindReview   = "review"
	// KindPerson is for the person, not a lane: it becomes a decision for them.
	KindPerson = "person"
)

// forPerson says a request is addressed to the person: its kind, or its lane naming them.
func forPerson(q store.Request) bool {
	switch strings.ToLower(strings.TrimSpace(q.Lane)) {
	case "person", "human", "the person":
		return true
	}
	return q.Kind == KindPerson
}

// MaxDepth caps a chain of requests: an agent answering one may ask in turn, but not
// without end.
const MaxDepth = 3

// reviewerOrder is who reviews, first installed agent that is not the author.
var reviewerOrder = []string{"claude", "copilot", "cursor"}

// RequestHelp records an agent's ask_shepherd and routes it when it can. The asking
// agent ends its turn; the reply continues its conversation.
func (r *Runner) RequestHelp(ctx context.Context, q store.Request) (store.Request, error) {
	switch q.Kind {
	case KindQuestion, KindHandoff, KindReview, KindPerson:
	default:
		return q, refuse("request kind %q: want question, handoff, review or person", q.Kind)
	}
	if strings.TrimSpace(q.Message) == "" {
		return q, refuse("say what you need")
	}
	if _, err := r.Store.Run(ctx, q.FromRun); err != nil {
		return q, err
	}
	if forPerson(q) {
		return r.toPerson(ctx, q)
	}
	depth, err := r.depthOf(ctx, q.FromRun)
	if err != nil {
		return q, err
	}
	if depth+1 > MaxDepth {
		return q, refuse("this would be request %d in one chain (at most %d); use ask_human instead", depth+1, MaxDepth)
	}
	q.Depth, q.State = depth+1, store.RequestPending
	q, err = r.Store.CreateRequest(ctx, q)
	if err != nil {
		return q, err
	}
	r.Log.Info("request recorded", "request", q.ID, "kind", q.Kind, "from_run", q.FromRun, "lane", q.Lane)
	target := "Shepherd"
	if q.Lane != "" {
		target = "lane " + q.Lane
	}
	r.feed(ctx, store.FeedRequest, q.ID, "request %d: %s asks %s (%s): %s", q.ID, r.who(ctx, q.FromRun), target, q.Kind, clip(q.Message, 160))
	go r.Route(context.Background())
	return q, nil
}

// toPerson turns a request for the person into a decision for them, the way ask_human
// would have, and records the request closed with a pointer to it. The answer comes
// back into the asker's session as any decision's does.
func (r *Runner) toPerson(ctx context.Context, q store.Request) (store.Request, error) {
	d, err := r.Ask(ctx, store.Decision{RunID: q.FromRun, Question: q.Message,
		Why: "asked through ask_shepherd (" + q.Kind + "), addressed to the person"})
	if err != nil {
		return q, err
	}
	q.Lane, q.State, q.Note = "", store.RequestClosed, fmt.Sprintf("for the person: held as decision %d", d.ID)
	q, err = r.Store.CreateRequest(ctx, q)
	if err != nil {
		return q, err
	}
	r.Log.Info("request for the person", "request", q.ID, "decision", d.ID)
	return q, nil
}

// CloseRequest closes a request that is not finished, without a reply: one gone stale,
// or no longer needed. A target run already going is left to finish; its reply is not
// carried back. A question no longer holds its asker's lane from shipping.
func (r *Runner) CloseRequest(ctx context.Context, id int64, why string) (store.Request, error) {
	q, err := r.Store.Request(ctx, id)
	if err != nil {
		return q, err
	}
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	switch q.State {
	case store.RequestReplied, store.RequestFailed, store.RequestClosed:
		return q, refuse("request %d is %s already", q.ID, q.State)
	}
	why = strings.TrimSpace(why)
	if why == "" {
		why = "no reason given"
	}
	q.State, q.Note = store.RequestClosed, "closed: "+why
	if err := r.Store.UpdateRequest(ctx, q); err != nil {
		return q, err
	}
	r.Log.Info("request closed", "request", q.ID, "why", why)
	r.feed(ctx, store.FeedRequest, q.ID, "request %d closed: %s", q.ID, clip(why, 160))
	r.settled(ctx, q)
	return q, nil
}

// depthOf is how deep in a chain of requests a run is: 0 for a run nobody asked for.
// A run started for a request, or continuing one that was, carries that depth.
func (r *Runner) depthOf(ctx context.Context, runID int64) (int, error) {
	reqs, err := r.Store.Requests(ctx)
	if err != nil {
		return 0, err
	}
	byTarget := map[int64]int{}
	for _, q := range reqs {
		if q.TargetRun != 0 {
			byTarget[q.TargetRun] = q.Depth
		}
	}
	for id, hops := runID, 0; id != 0 && hops < 100; hops++ {
		if d, ok := byTarget[id]; ok {
			return d, nil
		}
		run, err := r.Store.Run(ctx, id)
		if err != nil {
			return 0, err
		}
		id = run.Parent
	}
	return 0, nil
}

// RouteRequest is the front desk's or the person's say on a request Shepherd could not
// route by rule: which lane, and optionally which agent.
func (r *Runner) RouteRequest(ctx context.Context, id int64, lane, agent string) (store.Request, error) {
	q, err := r.Store.Request(ctx, id)
	if err != nil {
		return q, err
	}
	if q.State != store.RequestNeedsRouting && q.State != store.RequestPending {
		return q, refuse("request %d is %s", q.ID, q.State)
	}
	if agent != "" {
		if _, err := AdapterFor(agent); err != nil {
			return q, refuse("%v", err)
		}
	}
	q.Lane, q.Agent, q.State, q.Note = lane, agent, store.RequestPending, ""
	if err := r.Store.UpdateRequest(ctx, q); err != nil {
		return q, err
	}
	// Routing is the person's say, through the desk or by hand: its run goes like one
	// they started, past the daily budget's hold on Shepherd's own runs.
	r.said.Store(q.ID, true)
	r.Route(ctx)
	// A request still pending here says why in its note (queued behind a run, held).
	return r.Store.Request(ctx, id)
}

// Route moves every open request on as far as it can go now: to its target lane, from
// the target's reply back into the asker's conversation. It runs whenever a run ends.
func (r *Runner) Route(ctx context.Context) {
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	reqs, err := r.Store.Requests(ctx, store.RequestPending, store.RequestRouted, store.RequestReplyReady)
	if err != nil {
		r.Log.Error("load requests", "err", err)
		return
	}
	for _, q := range reqs {
		if q.State == store.RequestPending {
			q = r.dispatchRequest(ctx, q)
		}
		if q.State == store.RequestRouted {
			q = r.collectReply(ctx, q)
		}
		if q.State == store.RequestReplyReady {
			r.returnReply(ctx, q)
		}
	}
}

func (r *Runner) save(ctx context.Context, q store.Request) store.Request {
	if err := r.Store.UpdateRequest(ctx, q); err != nil {
		r.Log.Error("save request", "request", q.ID, "err", err)
	}
	return q
}

func (r *Runner) needsRouting(ctx context.Context, q store.Request, why string) store.Request {
	q.State, q.Note = store.RequestNeedsRouting, why
	r.Log.Info("request needs routing", "request", q.ID, "why", why)
	r.feed(ctx, store.FeedRequestStuck, q.ID, "request %d needs routing: %s", q.ID, why)
	return r.save(ctx, q)
}

// holdRequest leaves a request where it is and says why in its note, so a request is
// never stuck without a reason. The feed hears of a new reason, unless quiet (the asker
// has not ended its turn yet, which is every request's first moment).
func (r *Runner) holdRequest(ctx context.Context, q store.Request, quiet bool, format string, a ...any) store.Request {
	why := fmt.Sprintf(format, a...)
	if q.Note == why {
		return q
	}
	q.Note = why
	r.Log.Info("request waits", "request", q.ID, "why", why)
	if !quiet {
		r.feed(ctx, store.FeedRequest, q.ID, "request %d waits: %s", q.ID, why)
	}
	return r.save(ctx, q)
}

// dispatchRequest starts the target agent on a pending request, once the asker has
// ended its turn and the target lane is free. Until then the request says what it waits
// for; every run's end tries again.
func (r *Runner) dispatchRequest(ctx context.Context, q store.Request) store.Request {
	from, err := r.Store.Run(ctx, q.FromRun)
	if err != nil {
		return r.holdRequest(ctx, q, false, "its asking run %d cannot be read: %v", q.FromRun, err)
	}
	if from.State == store.RunRunning {
		return r.holdRequest(ctx, q, true, "run %d, which asked, has not ended its turn", from.ID)
	}
	fromLane, err := r.Store.Lane(ctx, from.LaneID)
	if err != nil {
		return r.holdRequest(ctx, q, false, "the asking run's lane cannot be read: %v", err)
	}
	laneName := q.Lane
	if laneName == "" && q.Kind == KindReview {
		laneName = fromLane.Name
	}
	if laneName == "" {
		return r.needsRouting(ctx, q, "no lane named; say which lane it is for")
	}
	target, why, err := r.findLane(ctx, fromLane, laneName)
	if err != nil {
		return r.holdRequest(ctx, q, false, "looking up lane %s failed: %v", laneName, err)
	}
	if why != "" {
		return r.needsRouting(ctx, q, why)
	}
	if busy, _ := r.Store.Runs(ctx, target.ID, store.RunRunning, 1); len(busy) > 0 {
		return r.holdRequest(ctx, q, false, "queued: run %d (%s) is going in lane %s; it starts when that run ends", busy[0].ID, busy[0].Agent, target.Name)
	}

	req := StartRequest{LaneID: target.ID, Agent: q.Agent}
	switch {
	case q.Kind == KindReview && req.Agent == "":
		req.Agent = r.reviewer(from.Agent)
		if req.Agent == "" {
			return r.needsRouting(ctx, q, "no agent other than "+from.Agent+" is installed to review")
		}
		req.NewSession = true
	case q.Kind == KindReview:
		req.NewSession = true
	case req.Agent == "":
		prev, _ := r.Store.Runs(ctx, target.ID, "", 1)
		if len(prev) == 0 {
			// No agent of Shepherd's has worked here; a conversation the person had by
			// hand on this branch may know the answer.
			if q.Kind == KindQuestion {
				if c, ok := r.conversationFor(ctx, target); ok {
					return r.askConversation(ctx, q, c, from.Agent, fromLane.Name, target)
				}
			}
			return r.needsRouting(ctx, q, "lane "+target.Name+" has no agent yet; say which one")
		}
		req.Agent = prev[0].Agent
	}
	req.Prompt = requestPrompt(q, from.Agent, fromLane.Name, target)
	_, said := r.said.Load(q.ID)
	req.Auto = !said
	run, err := r.Start(ctx, req)
	if errors.Is(err, ErrHeld) {
		return r.holdRequest(ctx, q, false, "held: %s", err) // tried again when a run ends
	}
	if err != nil {
		q.State, q.Note = store.RequestFailed, err.Error()
		r.Log.Error("route request", "request", q.ID, "err", err)
		r.feed(ctx, store.FeedRequestFailed, q.ID, "request %d failed: %s", q.ID, clip(err.Error(), 160))
		q = r.save(ctx, q)
		r.settled(ctx, q)
		return q
	}
	q.State, q.Lane, q.Agent, q.TargetRun, q.Note = store.RequestRouted, target.Name, req.Agent, run.ID, ""
	r.Log.Info("request routed", "request", q.ID, "lane", target.Name, "agent", req.Agent, "run", run.ID)
	r.feed(ctx, store.FeedRequestRouted, q.ID, "request %d routed to %s in lane %s (run %d)", q.ID, req.Agent, target.Name, run.ID)
	return r.save(ctx, q)
}

// settled is called when a request ends without a reply going back to its asker
// (failed, closed). A question held its asker's lane from shipping; with no answer
// coming, the lane ships now if its last run was the asker.
func (r *Runner) settled(ctx context.Context, q store.Request) {
	r.said.Delete(q.ID)
	if q.Kind != KindQuestion {
		return
	}
	run, err := r.Store.Run(ctx, q.FromRun)
	if err != nil || run.State == store.RunRunning {
		return
	}
	if last, err := r.Store.Runs(ctx, run.LaneID, "", 1); err != nil || len(last) == 0 || last[0].ID != run.ID {
		return // a later run in the lane ships it when it ends
	}
	lane, err := r.Store.Lane(ctx, run.LaneID)
	if err != nil {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.ship(context.Background(), run, lane)
	}()
}

// findLane finds an open lane by name in the asker's workspace, preferring the asker's
// repo. why is set when the name does not settle it.
func (r *Runner) findLane(ctx context.Context, from store.Lane, name string) (store.Lane, string, error) {
	fromRepo, err := r.Store.Repo(ctx, from.RepoID)
	if err != nil {
		return store.Lane{}, "", err
	}
	repos, err := r.Store.Repos(ctx, fromRepo.WorkspaceID)
	if err != nil {
		return store.Lane{}, "", err
	}
	// bare holds the asker's repo's open lanes named name after a prefix: "m2-gate"
	// for "feat/m2-gate".
	var found, bare []store.Lane
	for _, rp := range repos {
		lanes, err := r.Store.Lanes(ctx, rp.ID)
		if err != nil {
			return store.Lane{}, "", err
		}
		for _, l := range lanes {
			if l.State != store.LaneOpen {
				continue
			}
			if l.Name == name {
				if rp.ID == fromRepo.ID {
					return l, "", nil
				}
				found = append(found, l)
			} else if rp.ID == fromRepo.ID && strings.HasSuffix(l.Name, "/"+name) {
				bare = append(bare, l)
			}
		}
	}
	switch {
	case len(found) == 0 && len(bare) == 1:
		return bare[0], "", nil
	case len(found) == 0 && len(bare) > 1:
		var names []string
		for _, l := range bare {
			names = append(names, l.Name)
		}
		return store.Lane{}, fmt.Sprintf("%s could be any of %s; name it in full", name, strings.Join(names, ", ")), nil
	}
	switch len(found) {
	case 0:
		return store.Lane{}, "no open lane " + name, nil
	case 1:
		return found[0], "", nil
	}
	return store.Lane{}, fmt.Sprintf("%d open lanes are named %s; say which repo", len(found), name), nil
}

// reviewer is the first installed agent that is not the author: a review by a second
// provider catches what the first one's habits hide.
func (r *Runner) reviewer(author string) string {
	look := r.lookPath
	if look == nil {
		look = exec.LookPath
	}
	for _, name := range reviewerOrder {
		if name == author {
			continue
		}
		if _, err := look(adapters[name].Bin); err == nil {
			return name
		}
	}
	return ""
}

func requestPrompt(q store.Request, fromAgent, fromLane string, target store.Lane) string {
	head := fmt.Sprintf("[%s from %s in lane %s, through Shepherd (request %d)]\n%s\n\n", strings.ToUpper(q.Kind[:1])+q.Kind[1:], fromAgent, fromLane, q.ID, q.Message)
	switch q.Kind {
	case KindQuestion:
		return head + "Answer it. Change files only if the question asks you to. Then call report with status done and your answer as the summary."
	case KindHandoff:
		return head + "Do it within your lane's scope and commit. Then call report with status done and what you did, or blocked and why."
	}
	return head + fmt.Sprintf("Review this lane's work: git log --oneline %s..HEAD, and its diff (git diff %s...HEAD). Do not change any files. "+
		"Then call report with status done and your findings as the summary: numbered, most important first, each with the file and line, or \"no findings\".", target.Base, target.Base)
}

// collectReply takes the target run's report, or the end of its output, once it ends.
func (r *Runner) collectReply(ctx context.Context, q store.Request) store.Request {
	if q.TargetRun == 0 {
		return q // a conversation is answering; it sets the reply itself
	}
	run, err := r.Store.Run(ctx, q.TargetRun)
	if err != nil || run.State == store.RunRunning {
		return q
	}
	reply := ""
	if events, err := r.Store.Events(ctx, run.ID); err == nil {
		for _, e := range events {
			if e.Kind == EventReport && (e.Status == "done" || e.Status == "blocked") {
				reply = e.Text
				if e.Status == "blocked" {
					reply = "Blocked: " + reply
				}
			}
		}
	}
	if reply == "" {
		reply = logTail(run.Log, 3000)
		if reply == "" {
			reply = fmt.Sprintf("The %s run ended (%s) without a reply.", run.Agent, run.State)
		}
	}
	q.State, q.Reply = store.RequestReplyReady, reply
	return r.save(ctx, q)
}

// returnReply continues the asker's conversation with the reply, once its lane is free.
func (r *Runner) returnReply(ctx context.Context, q store.Request) {
	from, err := r.Store.Run(ctx, q.FromRun)
	if err != nil {
		return
	}
	if busy, _ := r.Store.Runs(ctx, from.LaneID, store.RunRunning, 1); len(busy) > 0 {
		r.holdRequest(ctx, q, false, "the reply is queued: run %d is going in the asker's lane; it goes back when that run ends", busy[0].ID)
		return
	}
	prompt := fmt.Sprintf("[Reply to your %s (request %d), from %s in lane %s]\n%s\n\nContinue your work with that.", q.Kind, q.ID, q.Agent, q.Lane, q.Reply)
	_, said := r.said.Load(q.ID)
	next, err := r.Start(ctx, StartRequest{Continue: q.FromRun, Prompt: prompt, Auto: !said})
	if errors.Is(err, ErrHeld) {
		r.holdRequest(ctx, q, false, "the reply is held: %s", err)
		return
	}
	if err != nil {
		q.State, q.Note = store.RequestFailed, "could not return the reply: "+err.Error()
		r.feed(ctx, store.FeedRequestFailed, q.ID, "request %d failed: %s", q.ID, clip(q.Note, 160))
		r.settled(ctx, r.save(ctx, q))
		return
	}
	q.Note = ""
	r.said.Delete(q.ID)
	q.State, q.ReplyRun = store.RequestReplied, next.ID
	r.save(ctx, q)
	r.Log.Info("reply returned", "request", q.ID, "run", next.ID)
	r.feed(ctx, store.FeedRequestReplied, q.ID, "request %d: %s replied; the asker carries on as run %d: %s", q.ID, q.Agent, next.ID, clip(q.Reply, 160))
}

// conversationFor is the adopted conversation that worked on a lane's branch most
// recently, if one is not open in a terminal.
func (r *Runner) conversationFor(ctx context.Context, lane store.Lane) (store.Conversation, bool) {
	cs, err := r.Store.Conversations(ctx, lane.RepoID)
	if err != nil {
		return store.Conversation{}, false
	}
	for _, c := range cs { // most recent first
		for _, b := range c.Branches {
			if b == lane.Branch && !convo.InUse(c.File) {
				return c, true
			}
		}
	}
	return store.Conversation{}, false
}

// askConversation answers a question from an adopted conversation, read-only, in the
// background; the reply then goes back to the asker like any other.
func (r *Runner) askConversation(ctx context.Context, q store.Request, c store.Conversation, fromAgent, fromLane string, target store.Lane) store.Request {
	q.State, q.Lane, q.Agent, q.Note = store.RequestRouted, target.Name, "conversation "+c.ID[:8], ""
	q = r.save(ctx, q)
	r.feed(ctx, store.FeedRequestRouted, q.ID, "request %d routed to conversation %s (%s), which worked on lane %s", q.ID, c.ID[:8], clip(c.Title, 60), target.Name)
	prompt := fmt.Sprintf("[A question through uBixShepherd from %s, working in lane %s]\n%s\n\nAnswer from what you know of branch %s. Do not change anything.", fromAgent, fromLane, q.Message, target.Branch)
	ask := r.AskConversation
	if ask == nil {
		ask = defaultAsk
	}
	r.wg.Add(1)
	go func(q store.Request) { // its own copy: the caller returns q while this runs
		defer r.wg.Done()
		ctx := context.Background()
		a, err := ask(ctx, c, prompt)
		r.Spend(ctx, store.Spend{Source: "session", USD: a.USD})
		reply := a.Text
		if err != nil {
			reply = "The conversation could not answer: " + err.Error()
		}
		q.State, q.Reply = store.RequestReplyReady, reply
		r.save(ctx, q)
		r.Route(ctx)
	}(q)
	return q
}

func defaultAsk(ctx context.Context, c store.Conversation, question string) (convo.Answer, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return convo.Answer{}, errors.New("claude is not on the daemon's PATH")
	}
	return convo.Ask(ctx, bin, c, question)
}

// logTail is the last part of a run's log, without Shepherd's own lines.
func logTail(path string, max int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "# shepherd") && !strings.HasPrefix(line, "# task:") {
			keep = append(keep, line)
		}
	}
	s := strings.TrimSpace(strings.Join(keep, "\n"))
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return s
}
