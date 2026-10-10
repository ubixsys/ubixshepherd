// Package cli is the shepherd command. Every command except daemon is a client of the
// HTTP API, so the CLI behaves the same against a local or a remote daemon.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/remote"
)

// Env is what a command runs against, so tests can supply their own.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Interactive is true when Stdin is a terminal a person can answer prompts on.
	Interactive bool
	Layout      paths.Layout
	Cwd         string
	// Exe is this binary, used to start a daemon in the background. Autostart says
	// whether commands may do that when no daemon answers.
	Exe       string
	Autostart bool
	// Client names this caller in the daemon's log: cli, mcp or hook.
	Client string
	// Remote is the daemon on another machine this command line is aimed at, or nil for
	// the local one. Run sets it from --host, $SHEPHERD_HOST or hosts.yaml; a command run
	// by another (an MCP tool) inherits it, and so shares the one tunnel.
	Remote *remoteHost
	// selected says Run has already applied the global options to this Env.
	selected bool
}

type command struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, env Env, args []string) error
}

// errUsage asks Main to print the command's usage and exit 2.
var errUsage = errors.New("usage")

// errSilent exits 1 after the command has already said why.
var errSilent = errors.New("silent")

func commands() []command {
	return []command{
		{"chat", "The one conversation: talk to Shepherd's front desk, with the swarm's events in the thread",
			"shepherd chat [--model M]", runChat},
		{"daemon", "Run or manage the daemon (start, stop, restart, status, install, uninstall)",
			"shepherd daemon [run | start | stop | restart | status | install | uninstall]", runDaemon},
		{"init", "Register a workspace and choose which of its repos Shepherd manages",
			"shepherd init [dir] [--name NAME] [--yes | --all | --only a,b]", runInit},
		{"lane", "Open, list and close lanes: a branch and worktree per stream of work",
			"shepherd lane open <name> --scope '<globs>' [--branch B] [--repo R] | list [--all] [--json] | close [name] [--force] | scope [lane] --add G --remove G | review [--repo R] [--only V] | ship [lane] [--repo R] | run [lane] --agent claude|copilot|cursor [--model M] [--new] [--detach] \"task\"", runLane},
		{"run", "Agent runs: list, show, follow, continue, attach to and stop them", "shepherd run list [--all] | show <id> | logs [-f] <id> | continue <id> \"message\" [--detach] | attach <id> | stop <id>", runRun},
		{"hook", "Install or check the pre-push hook that keeps a lane's pushes in its scope",
			"shepherd hook install | uninstall | status [--repo R]", runHook},
		{"fold", "Import lanes from a coordination file, keep its view, and find stale worktrees",
			"shepherd fold import [--repo R] [--file F] [--adopt BRANCH] [--apply] | view [--repo R] [--write] | retire <worktree>... [--repo R] | gc [--json]", runFold},
		{"decision", "The questions agents hold for you: list and answer them",
			"shepherd decision list [--all] | answer <id> \"answer\"", runDecision},
		{"session", "Conversations had outside Shepherd: adopt, list, reopen and ask them",
			"shepherd session import [--repo R] | list [--repo R] | attach <id> | ask <id> \"question\"", runSession},
		{"tag", "Reserve release versions, so no two lanes take the same one",
			"shepherd tag reserve major|minor|patch [--lane L | --no-lane] [--repo R] | list | release <tag>", runTag},
		{"request", "Requests between lanes: list them, route the ones Shepherd cannot, close stale ones",
			"shepherd request list [--all] | route <id> --lane L [--agent A] | close <id> [--why TEXT]", runRequest},
		{"agents", "Set up an agent CLI for Shepherd", "shepherd agents setup cursor", runAgents},
		{"worker", "", "shepherd worker report|ask-human|ask-shepherd (for agents Shepherd starts)", runWorker},
		{"mcp", "Serve Shepherd's operator tools over MCP on stdio, for Claude Code and other agents",
			"shepherd mcp   (register: claude mcp add --scope user shepherd -- shepherd mcp)", runMCP},
		{"status", "Show the daemon, its workspaces, and where you are", "shepherd status [--json]", runStatus},
		{"where", "Show the workspace, repo and lane for a directory", "shepherd where [dir] [--json]", runWhere},
		{"host", "List the machines this one can reach, and check that one answers",
			"shepherd host list | check [NAME]", runHost},
		{"version", "Print the version", "shepherd version", runVersion},
	}
}

// Main runs the shepherd command and returns its exit code.
func Main(args []string) int {
	home, err := paths.Home()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shepherd:", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shepherd:", err)
		return 1
	}
	env := Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
		Layout:      paths.Layout{Home: home}, Cwd: cwd,
		Autostart: os.Getenv(NoAutostartEnv) == "",
		Client:    "cli",
	}
	if exe, err := os.Executable(); err == nil {
		env.Exe = exe
	}
	// A closed terminal (SIGHUP) must end the command the way Ctrl-C does, so that Run's
	// deferred cleanup closes any ssh tunnel instead of leaving it behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return Run(ctx, env, args)
}

// Run dispatches args to a command.
func Run(ctx context.Context, env Env, args []string) int {
	if !env.selected {
		g, rest, err := splitGlobals(args)
		if err == nil {
			env.Remote, err = selectHost(env, g)
		}
		if err != nil {
			fmt.Fprintln(env.Stderr, "shepherd:", err)
			return 2
		}
		args, env.selected = rest, true
		if env.Remote != nil {
			// Whatever ends this call, a panic included, ends the tunnel with it.
			defer env.Remote.Close()
			env.Cwd = client.RemoteCwd
		}
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(env.Stdout)
		return 0
	}
	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		if env.Remote != nil {
			if why := remoteRefusal(env.Remote, c.name, args[1:]); why != "" {
				fmt.Fprintln(env.Stderr, "shepherd:", why)
				return 2
			}
		}
		err := c.run(ctx, env, args[1:])
		switch {
		case err == nil:
			return 0
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintln(env.Stdout, "usage:", c.usage)
			return 0
		case errors.Is(err, errSilent):
			return 1
		case errors.Is(err, errUsage):
			fmt.Fprintln(env.Stderr, "usage:", c.usage)
			return 2
		default:
			msg := err.Error()
			if env.Remote != nil {
				msg = remoteHint(err)
			}
			fmt.Fprintln(env.Stderr, "shepherd:", msg)
			return 1
		}
	}
	fmt.Fprintf(env.Stderr, "shepherd: unknown command %q\n\n", args[0])
	usage(env.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "shepherd: one voice to direct a swarm of AI agents")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	cs := commands()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	for _, c := range cs {
		if c.summary == "" {
			continue // internal: used by agents, not people
		}
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Files live in ~/.shepherd, or $%s if set.\n", paths.HomeEnv)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "To act on a daemon on another machine, over ssh, put these before the command:")
	fmt.Fprintf(w, "  --host NAME|ssh://[user@]host[:port]  (or $%s; NAME is in ~/.shepherd/hosts.yaml)\n", remote.HostEnv)
	fmt.Fprintf(w, "  --workspace NAME  --repo NAME         what a current directory would pick (or $%s, $%s)\n", WorkspaceEnv, RepoEnv)
	fmt.Fprintln(w, "  --local                               ignore a default host")
}

// flags returns a FlagSet that reports errors instead of exiting, and parses
// interspersed flags and positional arguments (shepherd where ~/git --json).
func flags(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// NoAutostartEnv, when set, stops commands from starting a daemon (for scripts and CI,
// where a daemon left running would be a surprise).
const NoAutostartEnv = "SHEPHERD_NO_AUTOSTART"

// dial returns a client for a running daemon, starting one first if none answers and
// autostart is allowed.
func dial(ctx context.Context, env Env) (*client.Client, error) {
	if env.Remote != nil {
		// Never start a daemon on someone else's machine: the person does that there.
		return env.Remote.client(ctx, env.Client)
	}
	if c, err := connect(ctx, env); err == nil {
		return c, nil
	}
	if !env.Autostart || env.Exe == "" {
		return nil, client.ErrNoDaemon
	}
	return startDaemon(ctx, env, true)
}

// connect returns a client for the daemon in the runtime file, if it answers.
func connect(ctx context.Context, env Env) (*client.Client, error) {
	c, err := client.FromRuntime(env.Layout.Runtime())
	if err != nil {
		return nil, err
	}
	c.Name = env.Client
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.Status(probe); err != nil {
		return nil, err
	}
	return c, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func joinOr(s []string, empty string) string {
	if len(s) == 0 {
		return empty
	}
	return strings.Join(s, ", ")
}
