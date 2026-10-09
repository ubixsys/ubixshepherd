package daemon

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// FeedConfig is the feed kind for a config reload, applied or refused.
const FeedConfig = "config"

// ReloadedPrefix starts the feed line of a reload that was applied; a refused one says
// why instead.
const ReloadedPrefix = "config reloaded"

// LiveConfig returns the config in force: the one the daemon started with, or the last
// one a reload applied. Each call sees one whole config, never a mix of old and new.
func (s *Server) LiveConfig() config.Config {
	if p := s.live.Load(); p != nil {
		return *p
	}
	return s.Config
}

// ReloadConfig reads ConfigPath again. A config that does not load or validate is
// refused: the daemon keeps the one it has and says why on the feed. A valid one is put
// in force for whatever starts next (runs, gates, ships, lanes), and the feed says what
// changed. A running agent keeps what it was started with: its brief and gate were
// fixed when it started. The log level applies at once (unless SHEPHERD_LOG_LEVEL is
// set, which overrides it). The listen address and poll interval take a restart.
func (s *Server) ReloadConfig(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	old := s.LiveConfig()
	next, err := config.Load(s.ConfigPath)
	if err != nil {
		s.Log.Warn("config reload refused; keeping the current config", "err", err)
		s.postFeed(ctx, FeedConfig, 0, fmt.Sprintf("config not reloaded, keeping the current one: %v", err))
		return err
	}
	s.applyConfig(next)
	msg := ReloadedPrefix
	if changed := configChanges(old, next); changed != "" {
		msg += ": " + changed
	} else {
		msg += ", nothing changed"
	}
	s.Log.Info("config reloaded", "path", s.ConfigPath)
	s.postFeed(ctx, FeedConfig, 0, msg)
	return nil
}

// applyConfig puts cfg in force for the server, its Fold and its Runner. Each holds it
// behind an atomic pointer and reads it once per operation, so a reload never races
// one going.
func (s *Server) applyConfig(cfg config.Config) {
	s.live.Store(&cfg)
	if s.Level != nil {
		s.Level.Set(LogLevel(cfg.Daemon.LogLevel))
	}
	if s.Fold != nil {
		s.Fold.SetConfig(cfg)
	}
	if s.Runner != nil {
		s.Runner.SetConfig(cfg)
	}
}

// postFeed adds a line to the person's thread; a feed that cannot be written is logged.
func (s *Server) postFeed(ctx context.Context, kind string, ref int64, text string) {
	if s.Store == nil {
		return
	}
	if err := s.Store.AddFeed(ctx, kind, text, ref); err != nil {
		s.Log.Error("write feed", "kind", kind, "err", err)
	}
}

// configChanges names what moved between two configs, for a person to read; "" when
// nothing did.
func configChanges(old, next config.Config) string {
	var out []string
	d, n := old.Daemon, next.Daemon
	if d.Listen != n.Listen {
		out = append(out, fmt.Sprintf("daemon.listen %s to %s (takes a restart)", d.Listen, n.Listen))
	}
	if d.Poll != n.Poll {
		out = append(out, fmt.Sprintf("daemon.poll %s to %s (takes a restart)", d.Poll, n.Poll))
	}
	if d.MaxRuns != n.MaxRuns {
		out = append(out, fmt.Sprintf("daemon.max_runs %d to %d", d.MaxRuns, n.MaxRuns))
	}
	if !reflect.DeepEqual(d.Budget, n.Budget) {
		out = append(out, fmt.Sprintf("daemon.budget %s to %s", amount(d.Budget), amount(n.Budget)))
	}
	if !reflect.DeepEqual(d.CreditUSD, n.CreditUSD) {
		out = append(out, fmt.Sprintf("daemon.credit_usd %s to %s", amount(d.CreditUSD), amount(n.CreditUSD)))
	}
	if d.LogLevel != n.LogLevel {
		out = append(out, fmt.Sprintf("daemon.log_level %s to %s", d.LogLevel, n.LogLevel))
	}
	if !reflect.DeepEqual(old.Defaults, next.Defaults) {
		out = append(out, "defaults")
	}
	names := map[string]bool{}
	for name := range old.Repos {
		names[name] = true
	}
	for name := range next.Repos {
		names[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		o, had := old.Repos[name]
		x, has := next.Repos[name]
		switch {
		case !had:
			out = append(out, "repos."+name+" added")
		case !has:
			out = append(out, "repos."+name+" removed")
		case !reflect.DeepEqual(o, x):
			out = append(out, "repos."+name)
		}
	}
	return strings.Join(out, ", ")
}

func amount(v *float64) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%g", *v)
}

// ReapOrphans stops agents a previous daemon left running, before Runner.Recover marks
// their runs interrupted. A clean stop ends its agents itself (Runner.Shutdown); a daemon
// that crashed or was killed cannot, and because each agent leads its own process group
// it outlives the daemon, still working in its lane with nobody reading its output. A
// recorded pid is signalled only while it still leads its own process group and started
// when its run did, so a pid the system has since reused is left alone.
func (s *Server) ReapOrphans(ctx context.Context) {
	runs, err := s.Store.Runs(ctx, 0, store.RunRunning, 1000)
	if err != nil {
		s.Log.Error("list runs left running", "err", err)
		return
	}
	for _, run := range runs {
		text := fmt.Sprintf("run %d: %s was interrupted, the daemon stopped while it was going", run.ID, run.Agent)
		if run.PID > 0 && leftoverAgent(run.PID, run.Started) {
			if err := stopGroup(run.PID); err != nil {
				s.Log.Warn("stop leftover agent", "run", run.ID, "pid", run.PID, "err", err)
			} else {
				s.Log.Info("stopped leftover agent", "run", run.ID, "pid", run.PID)
				text += fmt.Sprintf("; its agent was still running, so Shepherd stopped it (pid %d)", run.PID)
			}
		}
		s.postFeed(ctx, store.FeedRunEnded, run.ID, text)
	}
}

// startedNear says whether a process that has been running for age started within a
// minute of when.
func startedNear(age time.Duration, when time.Time) bool {
	d := time.Since(when) - age
	return d < time.Minute && d > -time.Minute
}
