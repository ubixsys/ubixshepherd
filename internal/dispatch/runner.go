package dispatch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/forge"
	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/scope"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// ErrRefused is wrapped by errors that are the caller's to fix.
var ErrRefused = errors.New("refused")

// ErrHeld is wrapped by refusals that time lifts: the lane's run going, the machine's run
// limit, the daily budget. Shepherd tries such a run again later.
var ErrHeld = errors.New("held")

type refusal struct {
	msg  string
	held bool
}

func (r refusal) Error() string { return r.msg }
func (r refusal) Is(target error) bool {
	return target == ErrRefused || (r.held && target == ErrHeld)
}

func refuse(format string, a ...any) error { return refusal{msg: fmt.Sprintf(format, a...)} }

func held(format string, a ...any) error { return refusal{msg: fmt.Sprintf(format, a...), held: true} }

// noPush is where git sends the lane repo's pushes while an agent runs (see pushBlock):
// it cannot connect, so `git push` fails whatever flags the agent passes.
const noPush = "shepherd-run-blocks-push://"

// StartRequest asks for an agent to be started in a lane.
//
// By default a lane keeps its conversation: a run continues the lane's last session
// with the same agent. NewSession starts a fresh one; Continue names the run whose
// session to continue (and so the lane and agent).
type StartRequest struct {
	LaneID     int64  `json:"lane_id,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Model      string `json:"model,omitempty"`
	Prompt     string `json:"prompt"`
	NewSession bool   `json:"new_session,omitempty"`
	Continue   int64  `json:"continue,omitempty"`
	// Auto marks a run Shepherd starts on its own (a fix, a routed request); the daily
	// budget holds these.
	Auto bool `json:"-"`
}

// Runner starts agents and watches them until they exit.
type Runner struct {
	Store store.Store
	// Config is the configuration until the first SetConfig; after that SetConfig's
	// wins. A daemon that reloads its configuration calls SetConfig, never assigns this.
	Config config.Config
	// Dir holds the run logs.
	Dir string
	// Exe is the shepherd binary that serves agents their worker tools; "" gives
	// agents none.
	Exe string
	// ForgeFor returns a repo's forge, for opening merge requests; tests replace it.
	ForgeFor func(remote string) (forge.Forge, error)
	// AskConversation answers a question from an adopted conversation; tests replace it.
	AskConversation func(ctx context.Context, c store.Conversation, question string) (convo.Answer, error)
	Log             *slog.Logger
	// lookPath finds an agent's executable; tests replace it.
	lookPath func(string) (string, error)

	mu    sync.Mutex
	procs map[int64]*proc
	// closing: Shutdown has begun. No run starts, and the runs it stops are recorded
	// as interrupted, with nothing set off by their end.
	closing bool
	// outOf holds the agents out of quota, by name, until their limit lifts. It lives
	// with the daemon: a restart tries each agent again.
	outOf   map[string]outOfQuota
	wg      sync.WaitGroup
	routeMu sync.Mutex
	// said holds the ids of requests the person or the front desk routed: their runs
	// are not Shepherd's own, so the daily budget does not hold them.
	said  sync.Map
	cfg   atomic.Pointer[config.Config]
	creds atomic.Pointer[Credentials]
}

// QuotaPause is how long an agent out of quota is held when its CLI does not say when
// the limit resets.
const QuotaPause = 30 * time.Minute

// tailLines is how near the end of a failed run's output a usage limit in words must
// be to count: the CLI's last word, not something the agent was working on.
const tailLines = 20

type outOfQuota struct {
	Limit
	run int64
}

// quotaHeld says why an agent cannot start for now, or "". Call it with r.mu held.
func (r *Runner) quotaHeld(agent string) string {
	q, ok := r.outOf[agent]
	if !ok {
		return ""
	}
	if !time.Now().Before(q.Until) {
		delete(r.outOf, agent)
		return ""
	}
	return fmt.Sprintf("%s is out of quota (run %d: %s); Shepherd holds %s runs until %s", agent, q.run, q.Text, agent, q.Until.Format("Jan 2 15:04"))
}

// outOfQuota records an agent out of quota until its limit lifts, and routes again then,
// for the requests it held.
func (r *Runner) outOfQuota(agent string, run int64, l Limit) time.Time {
	if now := time.Now(); !l.Until.After(now) || l.Until.After(now.Add(8*24*time.Hour)) {
		l.Until = now.Add(QuotaPause)
	}
	r.mu.Lock()
	if r.outOf == nil {
		r.outOf = map[string]outOfQuota{}
	}
	r.outOf[agent] = outOfQuota{Limit: l, run: run}
	r.mu.Unlock()
	time.AfterFunc(time.Until(l.Until)+time.Second, func() {
		r.mu.Lock()
		closing := r.closing
		r.mu.Unlock()
		if !closing {
			r.Route(context.Background())
		}
	})
	return l.Until
}

// SetConfig replaces the configuration, safely while runs go. Each operation (a start,
// a ship, a gate) reads the configuration once, so a reload never splits one.
func (r *Runner) SetConfig(c config.Config) { r.cfg.Store(&c) }

// Conf is the configuration for one operation: the last SetConfig's, else Config.
func (r *Runner) Conf() config.Config {
	if c := r.cfg.Load(); c != nil {
		return *c
	}
	return r.Config
}

type proc struct {
	cmd     *exec.Cmd
	stopped bool
	// token is the run's worker token, hidden in its output; revoke ends it.
	token  string
	revoke func()
}

// Recover marks runs a previous daemon left running as interrupted. Call it once at
// start, before any new run.
func (r *Runner) Recover(ctx context.Context) error {
	runs, err := r.Store.Runs(ctx, 0, store.RunRunning, 1000)
	if err != nil {
		return err
	}
	for _, run := range runs {
		now := time.Now().UTC()
		run.State, run.Ended, run.Error = store.RunInterrupted, &now, "the daemon stopped while this run was going"
		if err := r.Store.UpdateRun(ctx, run); err != nil {
			return err
		}
	}
	return nil
}

// Start starts an agent in a lane and returns at once; the run is watched in the
// background and recorded when it exits.
func (r *Runner) Start(ctx context.Context, req StartRequest) (store.Run, error) {
	cfg := r.Conf()
	var parent store.Run
	if req.Continue != 0 {
		p, err := r.Store.Run(ctx, req.Continue)
		if err != nil {
			return store.Run{}, err
		}
		switch {
		case p.State == store.RunRunning:
			return store.Run{}, refuse("run %d is still going; wait for it, or stop it first", p.ID)
		case p.Session == "":
			return store.Run{}, refuse("run %d has no session to continue (it started before Shepherd kept sessions, or its agent never reported one)", p.ID)
		case req.Agent != "" && req.Agent != p.Agent:
			return store.Run{}, refuse("run %d was %s; a session continues with the same agent", p.ID, p.Agent)
		}
		parent, req.LaneID, req.Agent = p, p.LaneID, p.Agent
		if req.Model == "" {
			req.Model = p.Model
		}
	}
	ad, err := AdapterFor(req.Agent)
	if err != nil {
		return store.Run{}, refuse("%v", err)
	}
	look := r.lookPath
	if look == nil {
		look = exec.LookPath
	}
	bin, err := look(ad.Bin)
	if err != nil {
		return store.Run{}, refuse("%s is not installed on this machine (%s not found on the daemon's PATH)", ad.Name, ad.Bin)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return store.Run{}, refuse("a run needs a task")
	}
	lane, err := r.Store.Lane(ctx, req.LaneID)
	if err != nil {
		return store.Run{}, err
	}
	if lane.State != store.LaneOpen {
		return store.Run{}, refuse("lane %s is %s", lane.Name, lane.State)
	}
	if _, err := os.Stat(lane.Worktree); err != nil {
		return store.Run{}, refuse("lane %s's worktree is gone: %s", lane.Name, lane.Worktree)
	}
	repo, err := r.Store.Repo(ctx, lane.RepoID)
	if err != nil {
		return store.Run{}, err
	}
	// The model: the one asked for, else the continued run's, else the repo's
	// agent.model for this agent, else the agent's own default.
	if req.Model == "" {
		req.Model = cfg.Profile(repo.Name).Agent.Model[ad.Name]
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return store.Run{}, held("the daemon is stopping; start the run when it is back")
	}
	if why := r.quotaHeld(ad.Name); why != "" {
		return store.Run{}, held("%s", why)
	}
	if r.procs == nil {
		r.procs = map[int64]*proc{}
	}
	running, err := r.Store.Runs(ctx, 0, store.RunRunning, 1000)
	if err != nil {
		return store.Run{}, err
	}
	for _, other := range running {
		if other.LaneID == lane.ID {
			return store.Run{}, held("run %d (%s) is already going in lane %s; one agent per lane", other.ID, other.Agent, lane.Name)
		}
	}
	if req.Auto {
		if why := r.overBudget(ctx, cfg); why != "" {
			return store.Run{}, held("%s", why)
		}
	}
	if max := cfg.Daemon.MaxRuns; len(running) >= max {
		return store.Run{}, held("%d agents are already running, the limit on this machine (daemon.max_runs)", max)
	}

	// The lane's conversation: continue its last session with this agent unless asked
	// for a new one.
	if parent.ID == 0 && !req.NewSession {
		prev, err := r.Store.Runs(ctx, lane.ID, "", 50)
		if err != nil {
			return store.Run{}, err
		}
		for _, p := range prev {
			if p.Agent == ad.Name && p.Session != "" {
				parent = p
				break
			}
		}
	}
	session, resume := parent.Session, parent.ID != 0
	if !resume {
		if session, err = ad.NewSession(ctx, bin, lane.Worktree); err != nil {
			return store.Run{}, err
		}
	}

	startSHA, err := git.Run(ctx, lane.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return store.Run{}, err
	}
	run, err := r.Store.CreateRun(ctx, store.Run{
		LaneID: lane.ID, Agent: ad.Name, Model: req.Model, Prompt: req.Prompt,
		Session: session, Parent: parent.ID,
		State: store.RunRunning, StartSHA: startSHA, Log: "pending",
	})
	if err != nil {
		return run, err
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return run, r.fail(ctx, run, err)
	}
	run.Log = filepath.Join(r.Dir, fmt.Sprintf("%d.log", run.ID))
	logf, err := os.OpenFile(run.Log, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return run, r.fail(ctx, run, err)
	}

	prof := cfg.Profile(repo.Name)
	gate := prof.Gate
	may := powers(prof, repo.Remote)
	// A new session gets the full brief; a continuing one already has it.
	prompt := req.Prompt
	worker := ""
	if r.Exe != "" && ad.WorkerReady() {
		worker = r.Exe
	}
	if !resume {
		note := ad.Note
		if b := prof.Brief; b != "" {
			note = strings.TrimSpace(note + "\nThis repo's rules: " + b)
		}
		prompt = Brief(req.Prompt, lane.Name, repo.Name, lane.Branch, lane.Base, lane.Worktree, lane.Scope, gate, may, worker != "", note)
	}
	agentPushes := may.Push == config.Agent
	cmd := exec.Command(bin, ad.Args(Opts{Model: req.Model, Gate: gate, Worktree: lane.Worktree,
		Session: session, Resume: resume, Worker: worker,
		Mode: prof.Agent.PermissionMode, Push: agentPushes, Merge: may.Merge})...)
	cmd.Dir = lane.Worktree
	// The prompt, then end of input: headless, and off the command line (see Adapter).
	cmd.Stdin = strings.NewReader(prompt)
	creds, token, revoke, err := r.grant(ctx, run.ID, lane.Worktree)
	if err != nil {
		logf.Close()
		return run, r.fail(ctx, run, err)
	}
	// The agent's tools reach the daemon with the run's own token, never the operator's:
	// drop one inherited from whoever started the daemon.
	cmd.Env = append(withoutEnv(os.Environ(), EnvToken, EnvURL, EnvRun),
		"GIT_TERMINAL_PROMPT=0", "SHEPHERD_LANE="+lane.Name)
	cmd.Env = append(cmd.Env, creds...)
	if !agentPushes {
		cmd.Env = append(cmd.Env, pushBlock(ctx, lane.Worktree)...)
	}
	cmd.SysProcAttr = groupAttr()
	out, err := cmd.StdoutPipe()
	if err != nil {
		revoke()
		logf.Close()
		return run, r.fail(ctx, run, err)
	}
	cmd.Stderr = cmd.Stdout // one ordered stream

	how := "new session"
	if resume {
		how = fmt.Sprintf("continues run %d", parent.ID)
	}
	fmt.Fprintf(logf, "# shepherd run %d: %s in lane %s (%s), %s, started %s\n# task: %s\n\n",
		run.ID, ad.Name, lane.Name, repo.Name, how, time.Now().Format(time.RFC3339), redact.String(firstLine(req.Prompt)))
	if err := cmd.Start(); err != nil {
		revoke()
		logf.Close()
		return run, r.fail(ctx, run, err)
	}
	run.PID = cmd.Process.Pid
	if err := r.Store.UpdateRun(ctx, run); err != nil {
		r.Log.Error("record run pid", "run", run.ID, "err", err)
	}
	p := &proc{cmd: cmd, token: token, revoke: revoke}
	r.procs[run.ID] = p
	r.wg.Add(1)
	go r.watch(run, ad, lane, p, out, logf)
	r.Log.Info("run started", "run", run.ID, "agent", ad.Name, "lane", lane.Name, "pid", run.PID)
	verb := "started"
	if resume {
		verb = "continues"
	}
	r.feed(ctx, store.FeedRunStarted, run.ID, "run %d: %s %s in lane %s: %s", run.ID, ad.Name, verb, lane.Name, clip(req.Prompt, 120))
	return run, nil
}

// powers reads what a repo's profile lets its agents do. Arming a merge is a GitLab
// command, so it is given only on GitLab.
func powers(prof config.Profile, remote string) Powers {
	gitlab := false
	if f, err := forge.For(remote); err == nil {
		gitlab = f.Name() == "gitlab"
	}
	return Powers{Push: prof.Autonomy.Push, Merge: prof.Autonomy.Merge == config.Agent && gitlab, GitLab: gitlab, Forbid: prof.Forbid}
}

// watch copies the agent's output to the log line by line, redacted, then records the
// outcome when the agent exits.
func (r *Runner) watch(run store.Run, ad Adapter, lane store.Lane, p *proc, out io.Reader, logf *os.File) {
	defer r.wg.Done()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var limit Limit
	lines, limitAt := 0, 0
	for sc.Scan() {
		line := sc.Text()
		lines++
		if ad.Limit != nil {
			if l, ok := ad.Limit(line); ok {
				limit, limitAt = l, lines
			}
		}
		// The latest id an agent reports wins, in case resuming ever moves a session.
		if id := ad.SessionIn(line); id != "" {
			run.Session = id
		}
		out := Output{Show: line}
		if ad.Read != nil {
			out = ad.Read(line)
		}
		switch {
		case !ad.SessionCost:
			run.CostUSD += out.USD
			run.Credits += out.Credits
		case out.USD > 0 || out.Credits > 0:
			run.CostUSD, run.Credits = out.USD, out.Credits // the session's latest total
		}
		if out.Show != "" {
			fmt.Fprintln(logf, scrub(out.Show, p.token))
		}
	}
	err := p.cmd.Wait()
	// The agent is gone: so is what it could call the daemon with.
	p.revoke()

	ctx := context.Background()
	now := time.Now().UTC()
	run.Ended = &now
	code := p.cmd.ProcessState.ExitCode()
	run.ExitCode = &code
	r.mu.Lock()
	stopped, closing := p.stopped, r.closing
	delete(r.procs, run.ID)
	r.mu.Unlock()
	switch {
	case stopped:
		run.State = store.RunStopped
	case err == nil && code == 0:
		run.State = store.RunSucceeded
	case closing:
		// Cut short by Shutdown: not the agent's failure.
		run.State, run.Error = store.RunInterrupted, "the daemon stopped while this run was going"
	default:
		run.State = store.RunFailed
		if err != nil {
			run.Error = redact.String(err.Error())
		}
	}
	// Out of quota: the CLI said so as the run failed, so the agent waits out its limit
	// instead of failing every run Shepherd starts in the meantime.
	var quotaUntil time.Time
	if run.State == store.RunFailed && limit.Text != "" && (limit.Sure || lines-limitAt < tailLines) {
		quotaUntil = r.outOfQuota(ad.Name, run.ID, limit)
		run.Error = redact.String("out of quota: " + limit.Text)
	}
	if end, err := git.Run(ctx, lane.Worktree, "rev-parse", "HEAD"); err == nil {
		run.EndSHA = end
		if n, err := git.Run(ctx, lane.Worktree, "rev-list", "--count", run.StartSHA+".."+end); err == nil {
			fmt.Sscan(n, &run.Commits)
		}
		if files, err := git.Run(ctx, lane.Worktree, "diff", "--name-only", "--no-renames", run.StartSHA, end); err == nil {
			for _, f := range strings.Split(files, "\n") {
				if f != "" && !scope.Any(lane.Scope, f) {
					run.Outside = append(run.Outside, f)
				}
			}
		}
	}
	if ad.SessionCost {
		r.sessionCost(ctx, &run)
	}
	r.Spend(ctx, store.Spend{Source: run.Agent, Ref: run.ID, USD: run.CostUSD, Credits: run.Credits})
	fmt.Fprintf(logf, "\n# shepherd: run %d %s (exit %d) with %d commit(s)", run.ID, run.State, code, run.Commits)
	if run.CostUSD > 0 {
		fmt.Fprintf(logf, ", $%.2f", run.CostUSD)
	}
	if run.Credits > 0 {
		fmt.Fprintf(logf, ", %.2f credits", run.Credits)
	}
	if len(run.Outside) > 0 {
		fmt.Fprintf(logf, "; outside the scope: %s", strings.Join(run.Outside, ", "))
	}
	fmt.Fprintln(logf)
	logf.Close()
	if err := r.Store.UpdateRun(ctx, run); err != nil {
		r.Log.Error("record run outcome", "run", run.ID, "err", err)
	}
	r.Log.Info("run ended", "run", run.ID, "state", run.State, "exit", code, "commits", run.Commits, "outside", len(run.Outside))
	ended := fmt.Sprintf("run %d: %s in lane %s %s, %d commit(s)", run.ID, run.Agent, lane.Name, run.State, run.Commits)
	if len(run.Outside) > 0 {
		ended += ", outside its scope: " + strings.Join(run.Outside, ", ")
	}
	if run.Commits > 0 {
		r.feed(ctx, store.FeedCommit, run.ID, "run %d: %d commit(s) in lane %s, latest: %s", run.ID, run.Commits, lane.Name, clip(latestSubject(ctx, lane.Worktree), 100))
	}
	r.feed(ctx, outcomeKind(run.State), run.ID, "%s", ended)
	if !quotaUntil.IsZero() {
		r.feed(ctx, store.FeedRunQuota, run.ID, "run %d: %s is out of quota (%s); Shepherd holds %s runs until %s",
			run.ID, run.Agent, clip(limit.Text, 160), run.Agent, quotaUntil.Format("Jan 2 15:04"))
	}
	if closing {
		// The daemon is stopping: answers and requests wait for its next start, and a
		// push (after a gate run of up to GateTimeout) is the person's or the next run's.
		return
	}
	r.deliverAnswers(ctx, run.ID)
	r.Route(ctx)
	r.ship(ctx, run, lane)
}

// Answer records a person's answer to a decision and carries it back into the asking
// agent's session: at once if the agent has ended its turn, or when its run ends.
func (r *Runner) Answer(ctx context.Context, id int64, answer string) (store.Decision, error) {
	if strings.TrimSpace(answer) == "" {
		return store.Decision{}, refuse("an answer cannot be empty")
	}
	d, err := r.Store.AnswerDecision(ctx, id, answer)
	if errors.Is(err, store.ErrConflict) {
		return d, refuse("%v", err)
	}
	if err != nil {
		return d, err
	}
	run, err := r.Store.Run(ctx, d.RunID)
	if err != nil {
		return d, err
	}
	if run.State == store.RunRunning {
		r.Log.Info("answer held until the run ends", "decision", d.ID, "run", run.ID)
		return d, nil
	}
	return r.deliver(ctx, d)
}

func (r *Runner) deliverAnswers(ctx context.Context, runID int64) {
	answered, err := r.Store.Decisions(ctx, store.DecisionAnswered)
	if err != nil {
		r.Log.Error("load answered decisions", "err", err)
		return
	}
	for _, d := range answered {
		if d.RunID == runID && d.AnswerRun == 0 {
			if _, err := r.deliver(ctx, d); err != nil {
				r.Log.Error("deliver answer", "decision", d.ID, "err", err)
			}
		}
	}
}

// deliver continues the asking run's session with the answer.
func (r *Runner) deliver(ctx context.Context, d store.Decision) (store.Decision, error) {
	prompt := fmt.Sprintf("The person answered your question (decision %d): %q\nAnswer: %s\nContinue the work with that.", d.ID, d.Question, d.Answer)
	next, err := r.Start(ctx, StartRequest{Continue: d.RunID, Prompt: prompt})
	if err != nil {
		return d, fmt.Errorf("answer recorded, but continuing run %d failed: %w", d.RunID, err)
	}
	if err := r.Store.SetDecisionRun(ctx, d.ID, next.ID); err != nil {
		return d, err
	}
	d.AnswerRun = next.ID
	r.Log.Info("answer delivered", "decision", d.ID, "run", next.ID)
	r.feed(ctx, store.FeedDecisionAnswer, d.ID, "decision %d answered; the agent carries on as run %d", d.ID, next.ID)
	return d, nil
}

// Ask records a question an agent holds for a person.
func (r *Runner) Ask(ctx context.Context, d store.Decision) (store.Decision, error) {
	if strings.TrimSpace(d.Question) == "" {
		return d, refuse("a question cannot be empty")
	}
	if _, err := r.Store.Run(ctx, d.RunID); err != nil {
		return d, err
	}
	d, err := r.Store.CreateDecision(ctx, d)
	if err == nil {
		r.Log.Info("decision held for the person", "decision", d.ID, "run", d.RunID)
		text := fmt.Sprintf("decision %d from %s: %s", d.ID, r.who(ctx, d.RunID), d.Question)
		for i, o := range d.Options {
			text += fmt.Sprintf("  [%d] %s", i+1, o)
		}
		if d.Recommendation != "" {
			text += "  (recommends: " + clip(d.Recommendation, 120) + ")"
		}
		r.feed(ctx, store.FeedDecision, d.ID, "%s", text)
	}
	return d, err
}

// EventReport is an agent's report; its status is progress, done or blocked.
const EventReport = "report"

var reportStatus = map[string]bool{"progress": true, "done": true, "blocked": true}

// Record stores something an agent told Shepherd.
func (r *Runner) Record(ctx context.Context, e store.Event) (store.Event, error) {
	switch {
	case e.Kind == EventReport && !reportStatus[e.Status]:
		return e, refuse("report status %q: want progress, done or blocked", e.Status)
	case e.Kind != EventReport:
		return e, refuse("event kind %q", e.Kind)
	case strings.TrimSpace(e.Text) == "":
		return e, refuse("say something")
	}
	if _, err := r.Store.Run(ctx, e.RunID); err != nil {
		return e, err
	}
	e.Text = redact.String(e.Text)
	e, err := r.Store.AddEvent(ctx, e)
	if err == nil {
		r.Log.Info("agent "+e.Kind, "run", e.RunID, "status", e.Status)
		r.feed(ctx, store.FeedReport, e.RunID, "%s reports %s: %s", r.who(ctx, e.RunID), e.Status, clip(e.Text, 200))
	}
	return e, err
}

// Stop asks a running agent to stop, and kills it if it has not after a grace period.
func (r *Runner) Stop(ctx context.Context, id int64) error {
	r.mu.Lock()
	p, ok := r.procs[id]
	if ok {
		p.stopped = true
	}
	r.mu.Unlock()
	if !ok {
		return refuse("run %d is not running", id)
	}
	terminate(p.cmd)
	go func() {
		time.Sleep(10 * time.Second)
		r.mu.Lock()
		_, still := r.procs[id]
		r.mu.Unlock()
		if still {
			kill(p.cmd)
		}
	}()
	return nil
}

// Shutdown stops every running agent and waits for their outcomes to be recorded, up
// to the context's deadline. Runs cut short this way are recorded as interrupted. No
// run's end sets anything off once it has begun (no answer delivered, no request
// routed, no push), and no run starts.
func (r *Runner) Shutdown(ctx context.Context) {
	r.mu.Lock()
	r.closing = true
	for _, p := range r.procs {
		terminate(p.cmd)
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		r.mu.Lock()
		for _, p := range r.procs {
			kill(p.cmd)
		}
		r.mu.Unlock()
	}
	r.Recover(context.Background())
}

// feed adds a line to the person's thread. A feed that cannot be written is logged, never
// a reason to fail the work it describes.
func (r *Runner) feed(ctx context.Context, kind string, ref int64, format string, a ...any) {
	if err := r.Store.AddFeed(ctx, kind, redact.String(fmt.Sprintf(format, a...)), ref); err != nil {
		r.Log.Error("write feed", "kind", kind, "err", err)
	}
}

// who names a run's agent and lane: "claude in lane feat/login".
func (r *Runner) who(ctx context.Context, runID int64) string {
	run, err := r.Store.Run(ctx, runID)
	if err != nil {
		return fmt.Sprintf("run %d", runID)
	}
	lane, err := r.Store.Lane(ctx, run.LaneID)
	if err != nil {
		return run.Agent
	}
	return run.Agent + " in lane " + lane.Name
}

// clip shortens text to one line of at most n bytes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

func (r *Runner) fail(ctx context.Context, run store.Run, err error) error {
	now := time.Now().UTC()
	run.State, run.Ended, run.Error = store.RunFailed, &now, redact.String(err.Error())
	r.Store.UpdateRun(ctx, run)
	return err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

// ReadLog returns the run log from offset, at most max bytes, and the offset to read
// from next.
func ReadLog(path string, offset int64, max int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, offset, err
	}
	return buf[:n], offset + int64(n), nil
}

// withoutEnv is env without the named variables.
func withoutEnv(env []string, names ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		keep := true
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// SetLookPath replaces how a runner finds agent executables (for tests in other packages).
func SetLookPath(r *Runner, f func(string) (string, error)) { r.lookPath = f }

// Wait blocks until every run the runner is watching has ended and its outcome, and any
// follow-up it set off, has been recorded.
func (r *Runner) Wait() { r.wg.Wait() }

// outcomeKind is the feed kind of a run that ended in state.
func outcomeKind(state string) string {
	switch state {
	case store.RunSucceeded:
		return store.FeedRunPassed
	case store.RunFailed:
		return store.FeedRunFailed
	case store.RunStopped, store.RunInterrupted:
		return store.FeedRunInterrupted
	}
	return store.FeedRunEnded
}

// latestSubject is the subject line of the lane's newest commit, or "".
func latestSubject(ctx context.Context, dir string) string {
	s, err := git.Run(ctx, dir, "log", "-1", "--format=%s")
	if err != nil {
		return ""
	}
	return s
}
