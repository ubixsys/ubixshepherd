package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/remote"
)

// Environment variables that choose where and what a command acts on from a machine that
// is not the daemon's. SHEPHERD_HOST is read by remote.HostEnv.
const (
	WorkspaceEnv = "SHEPHERD_WORKSPACE"
	RepoEnv      = "SHEPHERD_REPO"
)

// globals are the options that come before the subcommand.
type globals struct {
	host, workspace, repo string
	local                 bool
}

// splitGlobals reads the leading --host, --local, --workspace and --repo of a command
// line and returns the rest. They are not parsed by each command's own flag set, so
// every command takes them the same way.
func splitGlobals(args []string) (globals, []string, error) {
	var g globals
	for len(args) > 0 {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
		if !strings.HasPrefix(args[0], "-") || name == "" {
			break
		}
		dst := map[string]*string{"host": &g.host, "workspace": &g.workspace, "repo": &g.repo}[name]
		switch {
		case name == "local" && !hasVal:
			g.local = true
			args = args[1:]
		case dst != nil && hasVal:
			*dst = val
			args = args[1:]
		case dst != nil && len(args) > 1:
			*dst = args[1]
			args = args[2:]
		case dst != nil:
			return g, nil, fmt.Errorf("--%s needs a value", name)
		default:
			return g, args, nil // -h, --help, or something the command should reject
		}
	}
	return g, args, nil
}

// remoteHost is a daemon on another machine, reached through a tunnel that is opened
// when a command first needs it and closed when the command line ends.
type remoteHost struct {
	target          remote.Target
	workspace, repo string
	controlDir      string
	mu              sync.Mutex
	tr              *remote.SSH
	c               *client.Client
}

// Label names the host in messages.
func (r *remoteHost) Label() string { return r.target.Label() }

// Close ends the tunnel. Run defers it, so it also runs when a command panics.
func (r *remoteHost) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr != nil {
		r.tr.Close()
	}
}

// redialTimeout bounds one re-resolve and re-tunnel done on behalf of a caller that has
// no context of its own (client.Redial).
const redialTimeout = 45 * time.Second

// client returns the client for the host, tunnelling first if no tunnel is up, and
// following a daemon that has restarted since the last call.
func (r *remoteHost) client(ctx context.Context, name string) (*client.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fresh := false
	if r.c == nil {
		r.tr = &remote.SSH{Target: r.target, ControlDir: r.controlDir}
		ep, err := r.tr.Dial(ctx)
		if err != nil {
			return nil, err
		}
		tr := r.tr
		r.c = client.NewRemote(r.target.Label(), ep.Base, ep.Token(), func() (string, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), redialTimeout)
			defer cancel()
			ep, err := tr.Dial(ctx)
			return ep.Base, ep.Token(), err
		})
		r.c.SetScope(r.workspace, r.repo)
		fresh = true
	}
	r.c.Name = name
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := r.c.Status(probe)
	if err != nil && !fresh && errors.Is(err, client.ErrNoDaemon) {
		// The tunnel or the daemon went away between two commands (an MCP session lives
		// across many): resolve again.
		if rerr := r.c.Redial(); rerr != nil {
			return nil, rerr
		}
		_, err = r.c.Status(probe)
	}
	if err != nil {
		return nil, err
	}
	return r.c, nil
}

// selectHost applies the global options and the environment: it returns the remote host
// to use, or nil for the local daemon, which is how every command ran before hosts existed.
func selectHost(env Env, g globals) (*remoteHost, error) {
	hosts := func() (remote.Hosts, error) { return remote.LoadHosts(env.Layout.Hosts()) }
	t, ok, err := remote.Select(g.host, os.Getenv(remote.HostEnv), g.local, hosts)
	if err != nil {
		return nil, err
	}
	if !ok {
		if g.workspace != "" || g.repo != "" {
			return nil, errors.New("--workspace and --repo stand in for a current directory that a remote host does not have; select a host with --host")
		}
		return nil, nil
	}
	rh := &remoteHost{target: t, controlDir: env.Layout.SSHDir()}
	rh.workspace = firstOf(g.workspace, os.Getenv(WorkspaceEnv), t.Workspace)
	rh.repo = firstOf(g.repo, os.Getenv(RepoEnv))
	return rh, nil
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// remoteOK says which commands may run against a remote daemon. The others act on this
// machine's files or processes (init reads a directory, hook edits a repo's hooks, daemon
// manages the local daemon) or start a program here (agents, session attach), so naming a
// host would silently do the wrong thing: they refuse instead.
//
// chat is not listed: its front desk runs as a Claude Code process in the workspace
// directory, which is the daemon's path and not this machine's, until the desk moves
// into the daemon.
var remoteOK = map[string]bool{
	"status": true, "where": true, "lane": true, "run": true, "decision": true,
	"request": true, "tag": true, "mcp": true, "host": true, "version": true,
}

// remoteRefusal explains why a command cannot take --host, or returns "" if it can.
func remoteRefusal(rh *remoteHost, cmd string, args []string) string {
	ssh := "ssh " + rh.target.Dest
	if cmd == "run" && len(args) > 0 && args[0] == "attach" || cmd == "session" && len(args) > 0 && args[0] == "attach" {
		return fmt.Sprintf("shepherd %s attach opens the agent's own terminal session on this machine, and the run is on %s; run it there: %s shepherd %s attach", cmd, rh.Label(), ssh, cmd)
	}
	if remoteOK[cmd] {
		return ""
	}
	if cmd == "chat" {
		return fmt.Sprintf("shepherd chat cannot run against %s yet: its front desk starts here, in a directory that exists only on the daemon's machine. Run it there: %s -t shepherd chat", rh.Label(), ssh)
	}
	return fmt.Sprintf("shepherd %s works on this machine's own files or processes, so it cannot be pointed at %s. "+
		"Run it where the work is: %s shepherd %s ... (or use --local)", cmd, rh.Label(), ssh, cmd)
}

// remoteHint adds what a person away from the daemon's machine needs to know to an error
// that was written for someone standing in a directory.
func remoteHint(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "not inside a workspace") {
		msg += " (on a remote host there is no current directory: name one with --workspace NAME, or set " + WorkspaceEnv + " or \"workspace\" for the host in hosts.yaml)"
	}
	return msg
}

// runHost is `shepherd host`: the hosts this machine knows, and a check that one works.
func runHost(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "list":
		return hostList(env, args[1:])
	case "check":
		return hostCheck(ctx, env, args[1:])
	}
	return errUsage
}

func hostList(env Env, args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	h, err := remote.LoadHosts(env.Layout.Hosts())
	if err != nil {
		return err
	}
	if len(h.Hosts) == 0 {
		fmt.Fprintf(env.Stdout, "No hosts defined. Add them to %s, or use ssh://[user@]host[:port] with --host.\n", env.Layout.Hosts())
		return nil
	}
	names := h.Names()
	sort.Strings(names)
	for _, n := range names {
		e := h.Hosts[n]
		line := fmt.Sprintf("%-14s %s", n, e.SSH)
		if e.Home != "" {
			line += "  home " + e.Home
		}
		if e.Workspace != "" {
			line += "  workspace " + e.Workspace
		}
		if e.Control {
			line += "  shared connection"
		}
		if n == h.Default {
			line += "  (default)"
		}
		fmt.Fprintln(env.Stdout, line)
	}
	return nil
}

// hostCheck resolves a host, opens the tunnel and asks the daemon for its status.
func hostCheck(ctx context.Context, env Env, args []string) error {
	var rh *remoteHost
	switch len(args) {
	case 0:
		rh = env.Remote
		if rh == nil {
			return errors.New("name a host: shepherd host check NAME (or select one with --host)")
		}
	case 1:
		var err error
		if rh, err = selectHost(env, globals{host: args[0]}); err != nil {
			return err
		}
		defer rh.Close()
	default:
		return errUsage
	}
	start := time.Now()
	c, err := rh.client(ctx, env.Client)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s: daemon %s, pid %d, reachable over ssh in %s\n", rh.Label(), st.Version, st.PID, time.Since(start).Round(time.Millisecond))
	return nil
}
