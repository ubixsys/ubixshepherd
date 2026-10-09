package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/daemon"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/service"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
	"github.com/ubixsys/ubixshepherd/internal/watch"
)

// serviceFor finds the OS service manager; tests replace it.
var serviceFor = service.For

func runDaemon(ctx context.Context, env Env, args []string) error {
	fs := flags("daemon", env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	sub := "run"
	switch len(pos) {
	case 0:
	case 1:
		sub = pos[0]
	default:
		return errUsage
	}
	switch sub {
	case "run":
		return daemonRun(ctx, env)
	case "start":
		if c, err := connect(ctx, env); err == nil {
			st, _ := c.Status(ctx)
			fmt.Fprintf(env.Stdout, "already running (pid %d)\n", st.PID)
			return nil
		}
		_, err := startDaemon(ctx, env, false)
		return err
	case "stop":
		return daemonStop(ctx, env)
	case "restart":
		// Refuse before stopping, so a bad config.yaml leaves the running daemon alone.
		if err := checkConfig(env.Layout); err != nil {
			return err
		}
		if err := daemonStop(ctx, env); err != nil {
			return err
		}
		_, err := startDaemon(ctx, env, false)
		return err
	case "reload":
		return daemonReload(ctx, env)
	case "status":
		return daemonStatus(ctx, env)
	case "install":
		return daemonInstall(ctx, env)
	case "uninstall":
		return daemonUninstall(ctx, env)
	}
	return errUsage
}

// daemonRun runs the daemon in the foreground until interrupted or asked to stop.
func daemonRun(ctx context.Context, env Env) error {
	l := env.Layout
	if c, err := connect(ctx, env); err == nil {
		st, _ := c.Status(ctx)
		return fmt.Errorf("a daemon is already running (pid %d, version %s); a command or make install started it in the background.\n"+
			"  watch it:        tail -f %s\n"+
			"  or run it here:  shepherd daemon stop && shepherd daemon", st.PID, st.Version, l.Log())
	}
	if err := os.MkdirAll(l.Home, 0o700); err != nil {
		return err
	}
	if wrote, err := config.WriteTemplate(l.Config()); err != nil {
		return err
	} else if wrote {
		fmt.Fprintf(env.Stderr, "Wrote %s: every setting commented out, defaults apply.\n", l.Config())
	}
	cfg, err := config.Load(l.Config())
	if err != nil {
		return err
	}
	st, err := sqlite.Open(ctx, l.Store())
	if err != nil {
		return err
	}
	defer st.Close()
	// The daemon writes and rotates its own log; whatever started it captures only what
	// the log cannot (see Layout.Console). Run by hand in a terminal, it logs there too.
	var also io.Writer
	if term.IsTerminal(int(os.Stderr.Fd())) {
		also = os.Stderr
	}
	level := new(slog.LevelVar)
	level.Set(daemon.LogLevel(cfg.Daemon.LogLevel))
	logger, logFile, err := daemon.OpenLogger(l.Log(), also, level)
	if err != nil {
		return err
	}
	defer logFile.Close()
	srv, err := daemon.NewServer(st, cfg, l.Config(), logger)
	if err != nil {
		return err
	}
	srv.Level = level
	srv.Fold.Exe = env.Exe
	srv.Runner = &dispatch.Runner{Store: st, Config: cfg, Dir: filepath.Join(l.Home, "runs"), Log: srv.Log, Exe: env.Exe}
	if cfg.Daemon.Poll != "off" {
		every, _ := time.ParseDuration(cfg.Daemon.Poll)
		w := &watch.Watcher{Store: st, Fold: srv.Fold, Runner: srv.Runner, Log: srv.Log, Interval: every}
		go w.Run(ctx)
	}
	return srv.Run(ctx, l.Runtime())
}

// startDaemon starts the daemon, through the service manager when it is installed and
// otherwise as a detached child, and waits for it to answer. auto marks a start that a
// command made on its own, which says so and suggests installing.
func startDaemon(ctx context.Context, env Env, auto bool) (*client.Client, error) {
	l := env.Layout
	if err := checkConfig(l); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.Home, 0o700); err != nil {
		return nil, err
	}
	// What the daemon prints before it dies goes to the console file; note where this
	// start's output begins, so a failure can quote it.
	var from int64
	if fi, err := os.Stat(l.Console()); err == nil {
		from = fi.Size()
	}
	mgr, _ := serviceFor()
	how := ""
	if mgr != nil && mgr.Installed() {
		if err := mgr.Start(); err != nil {
			return nil, err
		}
		how = "through " + mgr.Name()
	} else {
		pid, err := spawn(env)
		if err != nil {
			return nil, err
		}
		how = fmt.Sprintf("pid %d", pid)
	}
	c, err := waitForDaemon(ctx, env, 10*time.Second)
	if err != nil {
		if out := consoleSince(l, from); out != "" {
			return nil, fmt.Errorf("started the daemon (%s) but it did not answer; it said:\n%s\nsee %s", how, out, l.Console())
		}
		return nil, fmt.Errorf("started the daemon (%s) but it did not answer: %w; see %s and %s", how, err, l.Console(), l.Log())
	}
	st, _ := c.Status(ctx)
	fmt.Fprintf(env.Stderr, "started shepherd daemon (pid %d, log %s)\n", st.PID, l.Log())
	if auto && (mgr == nil || !mgr.Installed()) {
		fmt.Fprintln(env.Stderr, "run `shepherd daemon install` to start it at login")
	}
	return c, nil
}

// checkConfig reads config.yaml as the daemon will, so a start that would fail on it
// says why at once instead of waiting for a daemon that has already exited.
func checkConfig(l paths.Layout) error {
	if _, err := config.Load(l.Config()); err != nil {
		return fmt.Errorf("the daemon will not start with this config: %w", err)
	}
	return nil
}

// consoleSince returns the last lines written to the console file after offset from,
// trimmed: what a daemon that failed to start printed.
func consoleSince(l paths.Layout, from int64) string {
	b, err := os.ReadFile(l.Console())
	if err != nil || int64(len(b)) <= from {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b[from:])), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return "  " + strings.Join(lines, "\n  ")
}

// spawn starts `shepherd daemon` detached from this terminal, its output going to the
// console file.
func spawn(env Env) (int, error) {
	logf, err := os.OpenFile(env.Layout.Console(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(env.Exe, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), paths.HomeEnv+"="+env.Layout.Home)
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

func waitForDaemon(ctx context.Context, env Env, limit time.Duration) (*client.Client, error) {
	deadline := time.Now().Add(limit)
	for {
		c, err := connect(ctx, env)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stopWait is how long stop waits for the daemon to exit.
const stopWait = 30 * time.Second

func daemonStop(ctx context.Context, env Env) error {
	c, err := connect(ctx, env)
	if err != nil {
		fmt.Fprintln(env.Stdout, "not running")
		return nil
	}
	st, _ := c.Status(ctx)
	if err := c.Shutdown(ctx); err != nil {
		return err
	}
	// A stopping daemon gives its agents up to 15 seconds to end (after 5 for requests in
	// flight) before it lets go of the runtime file, so restart must wait that long or
	// it starts a second daemon into the first one's lock.
	deadline := time.Now().Add(stopWait)
	for {
		if _, err := os.Stat(env.Layout.Runtime()); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(env.Stdout, "stopped (pid %d)\n", st.PID)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("asked pid %d to stop, but it is still running", st.PID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// daemonReload has the running daemon read config.yaml again (SIGHUP), then says what
// the daemon made of it: what changed, or why it kept the config it had.
func daemonReload(ctx context.Context, env Env) error {
	if runtime.GOOS == "windows" {
		return errors.New("daemon reload is not supported on Windows yet; use shepherd daemon restart")
	}
	c, err := connect(ctx, env)
	if err != nil {
		return fmt.Errorf("the daemon is not running; it reads %s when it starts", env.Layout.Config())
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	feed, err := c.Feed(ctx, -1)
	if err != nil {
		return err
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal the daemon (pid %d): %w", st.PID, err)
	}
	after := feed.Last
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		feed, err := c.Feed(ctx, after)
		if err != nil {
			return err
		}
		for _, it := range feed.Items {
			if it.Kind != daemon.FeedConfig {
				continue
			}
			if !strings.HasPrefix(it.Text, daemon.ReloadedPrefix) {
				return errors.New(it.Text)
			}
			fmt.Fprintln(env.Stdout, it.Text)
			return nil
		}
		after = feed.Last
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("asked pid %d to reload, but it has not said whether it did; see %s", st.PID, env.Layout.Log())
}

func daemonStatus(ctx context.Context, env Env) error {
	w := env.Stdout
	if c, err := connect(ctx, env); err == nil {
		st, _ := c.Status(ctx)
		fmt.Fprintf(w, "daemon   running, pid %d, version %s, since %s\n", st.PID, st.Version, st.Started.Local().Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintln(w, "daemon   not running")
	}
	mgr, err := serviceFor()
	switch {
	case err != nil:
		fmt.Fprintf(w, "login    %v\n", err)
	case mgr.Installed():
		fmt.Fprintf(w, "login    starts at login through %s (%s)\n", mgr.Name(), mgr.File())
	default:
		fmt.Fprintln(w, "login    not installed: `shepherd daemon install` starts it at login")
	}
	fmt.Fprintf(w, "home     %s\n", env.Layout.Home)
	fmt.Fprintf(w, "log      %s\n", env.Layout.Log())
	return nil
}

func daemonInstall(ctx context.Context, env Env) error {
	mgr, err := serviceFor()
	if err != nil {
		return err
	}
	if env.Exe == "" {
		return errors.New("cannot find this binary's path")
	}
	if warn := service.Warning(env.Exe); warn != "" {
		fmt.Fprintf(env.Stderr, "warning: registering %s: %s\n", env.Exe, warn)
	}
	if err := checkConfig(env.Layout); err != nil {
		return err
	}
	// A daemon started by hand would hold the lock and make the managed one fail.
	if _, err := connect(ctx, env); err == nil {
		if err := daemonStop(ctx, env); err != nil {
			return err
		}
	}
	spec := service.Spec{Exe: env.Exe, Log: env.Layout.Log(), Console: env.Layout.Console(), Path: os.Getenv("PATH")}
	if os.Getenv(paths.HomeEnv) != "" {
		spec.Home = env.Layout.Home
	}
	if err := os.MkdirAll(env.Layout.Home, 0o700); err != nil {
		return err
	}
	if err := mgr.Install(spec); err != nil {
		return err
	}
	w := env.Stdout
	fmt.Fprintf(w, "installed %s\n", mgr.File())
	fmt.Fprintf(w, "  runs   %s daemon\n", spec.Exe)
	fmt.Fprintf(w, "  PATH   %s\n", spec.Path)
	fmt.Fprintf(w, "  log    %s (rotated at %d MB, %d old files kept)\n", spec.Log, daemon.LogMaxBytes>>20, daemon.LogKeep)
	fmt.Fprintf(w, "  other  %s (panics and startup errors)\n", spec.Console)
	fmt.Fprintln(w, "It starts at login and restarts if it crashes; `shepherd daemon stop` stops it until the next login.")
	if mgr.Name() == "systemd" {
		fmt.Fprintln(w, "To keep it running while you are logged out: loginctl enable-linger $USER")
	}
	if _, err := waitForDaemon(ctx, env, 10*time.Second); err != nil {
		return fmt.Errorf("installed, but the daemon did not answer: %w; see %s and %s", err, spec.Console, spec.Log)
	}
	fmt.Fprintln(w, "daemon is running")
	return nil
}

func daemonUninstall(ctx context.Context, env Env) error {
	mgr, err := serviceFor()
	if err != nil {
		return err
	}
	if !mgr.Installed() {
		fmt.Fprintln(env.Stdout, "not installed")
		return nil
	}
	if err := mgr.Uninstall(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "removed %s; the daemon is stopped and will not start at login\n", mgr.File())
	return nil
}
