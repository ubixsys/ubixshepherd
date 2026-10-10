package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// shepherd decision list | answer: the questions agents hold for the person.
func runDecision(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	fs := flags("decision "+args[0], env)
	all := fs.Bool("all", false, "list answered decisions too")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		if len(pos) > 0 {
			return errUsage
		}
		state := store.DecisionOpen
		if *all {
			state = ""
		}
		ds, err := c.Decisions(ctx, state)
		if err != nil {
			return err
		}
		if len(ds) == 0 {
			fmt.Fprintln(env.Stdout, "No decisions waiting for you.")
			return nil
		}
		w := env.Stdout
		for _, d := range ds {
			fmt.Fprintf(w, "decision %d  %s  from %s in lane %s (%s), run %d\n", d.ID, d.State, d.Agent, d.Lane, d.Repo, d.RunID)
			fmt.Fprintf(w, "  %s\n", d.Question)
			for i, o := range d.Options {
				fmt.Fprintf(w, "    %d. %s\n", i+1, o)
			}
			if d.Recommendation != "" {
				fmt.Fprintf(w, "  recommends: %s\n", d.Recommendation)
			}
			if d.Why != "" {
				fmt.Fprintf(w, "  why yours:  %s\n", d.Why)
			}
			if d.Answer != "" {
				fmt.Fprintf(w, "  answered:   %s\n", d.Answer)
			}
			fmt.Fprintln(w)
		}
		if state == store.DecisionOpen {
			fmt.Fprintln(w, `Answer one with: shepherd decision answer <id> "..." (a number picks that option)`)
		}
		return nil
	case "answer":
		if len(pos) != 2 {
			return errUsage
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return fmt.Errorf("decision id %q is not a number", pos[0])
		}
		answer := pos[1]
		// A bare number picks that option, so "2" means the second choice offered.
		if n, err := strconv.Atoi(strings.TrimSpace(answer)); err == nil {
			ds, err := c.Decisions(ctx, "")
			if err != nil {
				return err
			}
			for _, d := range ds {
				if d.ID == id && n >= 1 && n <= len(d.Options) {
					answer = fmt.Sprintf("option %d: %s", n, d.Options[n-1])
				}
			}
		}
		d, err := c.Answer(ctx, id, answer)
		if err != nil {
			return err
		}
		if d.AnswerRun != 0 {
			fmt.Fprintf(env.Stdout, "answered decision %d; the agent carries on as run %d (shepherd run logs -f %d)\n", d.ID, d.AnswerRun, d.AnswerRun)
		} else {
			fmt.Fprintf(env.Stdout, "answered decision %d; the agent is still running and gets the answer when its turn ends\n", d.ID)
		}
		return nil
	}
	return errUsage
}

// shepherd worker ...: what the worker tools run. Not for people: the run comes from
// SHEPHERD_RUN, which Shepherd sets for the agents it starts.
func runWorker(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	fs := flags("worker "+args[0], env)
	status := fs.String("status", "", "report: progress, done or blocked")
	var options listFlag
	fs.Var(&options, "option", "ask-human: an option (repeatable)")
	rec := fs.String("recommendation", "", "ask-human: what you recommend")
	why := fs.String("why", "", "ask-human: why it is the person's call")
	kind := fs.String("kind", "question", "ask-shepherd: question, handoff, review or person")
	lane := fs.String("lane", "", "ask-shepherd: the lane it is for, if known")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}
	c, runID, err := workerClient(ctx, env)
	if err != nil {
		return err
	}
	switch args[0] {
	case "report":
		if _, err := c.AddEvent(ctx, runID, store.Event{Kind: dispatch.EventReport, Status: *status, Text: pos[0]}); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Recorded (%s). Shepherd checks claims of done against the lane's commits and gate.\n", *status)
	case "ask-human":
		d, err := c.Ask(ctx, runID, store.Decision{Question: pos[0], Options: options, Recommendation: *rec, Why: *why})
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Held for the person as decision %d. End your turn now with a one-line summary of where you are; Shepherd continues this conversation with the answer.\n", d.ID)
	case "tag-reserve":
		run, err := c.Run(ctx, runID)
		if err != nil {
			return err
		}
		lane, err := c.Resolve(ctx, run.Worktree)
		if err != nil || lane.Repo == nil {
			return errors.New("cannot find this run's repo")
		}
		res, err := c.ReserveTag(ctx, api.Reserve{RepoID: lane.Repo.ID, LaneID: run.LaneID, Bump: pos[0]})
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "%s is reserved for your lane. Tag exactly that, on a commit that includes your merged work.\n", res.Tag)
	case "ask-shepherd":
		q, err := c.RequestHelp(ctx, runID, store.Request{Kind: *kind, Lane: *lane, Message: pos[0]})
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Request %d recorded; Shepherd routes it when you end your turn. End it now with a one-line summary; Shepherd continues this conversation with the reply.\n", q.ID)
	default:
		return errUsage
	}
	return nil
}

// workerClient is a client for the run a worker tool call belongs to, with the run's
// own token: SHEPHERD_RUN, SHEPHERD_TOKEN and SHEPHERD_URL, which Shepherd sets for the
// agents it starts, or else the run file in this worktree's git directory (for CLIs
// that do not pass their environment on to MCP servers). It never reads daemon.json:
// the operator token there is the person's, not an agent's.
func workerClient(ctx context.Context, env Env) (*client.Client, int64, error) {
	tok, url := os.Getenv(dispatch.EnvToken), os.Getenv(dispatch.EnvURL)
	run, _ := strconv.ParseInt(os.Getenv(dispatch.EnvRun), 10, 64)
	if tok == "" || url == "" || run == 0 {
		rc, err := dispatch.ReadRunFile(ctx, env.Cwd)
		if err != nil {
			return nil, 0, errors.New("these tools are for agents Shepherd started in a lane, and this is not one of them (no run token in SHEPHERD_TOKEN or in this worktree)")
		}
		tok, url, run = rc.Token, rc.URL, rc.Run
	}
	c := client.New(url, tok)
	c.Name = "worker"
	return c, run, nil
}

// shepherd request list | route: requests between lanes, and routing the ones Shepherd
// cannot route by rule.
func runRequest(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	fs := flags("request "+args[0], env)
	all := fs.Bool("all", false, "list finished requests too")
	lane := fs.String("lane", "", "route: the target lane")
	agent := fs.String("agent", "", "route: the agent to ask (default: the lane's, or another provider for a review)")
	why := fs.String("why", "", "close: why it is closed")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		states := strings.Join([]string{store.RequestNeedsRouting, store.RequestPending, store.RequestRouted, store.RequestReplyReady}, ",")
		if *all {
			states = ""
		}
		reqs, err := c.Requests(ctx, states)
		if err != nil {
			return err
		}
		if len(reqs) == 0 {
			fmt.Fprintln(env.Stdout, "No requests between lanes.")
			return nil
		}
		w := env.Stdout
		for _, q := range reqs {
			fmt.Fprintf(w, "request %d  %s  %s from %s in lane %s (%s)", q.ID, q.State, q.Kind, q.FromAgent, q.FromLane, q.Repo)
			if q.Lane != "" {
				fmt.Fprintf(w, ", for lane %s", q.Lane)
			}
			if q.Agent != "" {
				fmt.Fprintf(w, " (%s)", q.Agent)
			}
			fmt.Fprintf(w, "\n  %s\n", oneLine(q.Message, 110))
			if q.Note != "" {
				fmt.Fprintf(w, "  note: %s\n", q.Note)
			}
			if q.Reply != "" {
				fmt.Fprintf(w, "  reply: %s\n", oneLine(q.Reply, 110))
			}
		}
		return nil
	case "route":
		if len(pos) != 1 || *lane == "" {
			return errUsage
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return fmt.Errorf("request id %q is not a number", pos[0])
		}
		q, err := c.RouteRequest(ctx, id, *lane, *agent)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "request %d is %s", q.ID, q.State)
		if q.TargetRun != 0 {
			fmt.Fprintf(env.Stdout, ": %s is on it as run %d", q.Agent, q.TargetRun)
		}
		if q.Note != "" {
			fmt.Fprintf(env.Stdout, " (%s)", q.Note)
		}
		fmt.Fprintln(env.Stdout)
		return nil
	case "close":
		if len(pos) != 1 {
			return errUsage
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return fmt.Errorf("request id %q is not a number", pos[0])
		}
		q, err := c.CloseRequest(ctx, id, *why)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "request %d is %s (%s)\n", q.ID, q.State, q.Note)
		return nil
	}
	return errUsage
}

// shepherd agents setup cursor: give Cursor the worker tools.
func runAgents(ctx context.Context, env Env, args []string) error {
	if len(args) != 2 || args[0] != "setup" || args[1] != "cursor" {
		return errUsage
	}
	if env.Exe == "" {
		return errors.New("cannot find this binary's path")
	}
	changed, err := dispatch.SetupCursor(env.Exe)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(env.Stdout, "%s already has Shepherd's worker server\n", dispatch.CursorConfig())
		return nil
	}
	fmt.Fprintf(env.Stdout, "added shepherd-worker to %s (other servers untouched)\n", dispatch.CursorConfig())
	fmt.Fprintln(env.Stdout, "It serves tools only to agents Shepherd starts; in your own Cursor sessions it reports that it has no run.")
	return nil
}
