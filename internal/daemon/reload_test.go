package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// reloadServer is a server whose config file the test writes, with a Runner holding the
// config the way the daemon's does.
func reloadServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, _ := newServer(t)
	s.ConfigPath = filepath.Join(t.TempDir(), "config.yaml")
	s.Runner = &dispatch.Runner{Store: s.Store, Config: s.Config, Log: s.Log}
	return s, s.ConfigPath
}

func lastFeed(t *testing.T, st store.Store) store.FeedItem {
	t.Helper()
	items, err := st.Feed(context.Background(), 0, 100)
	if err != nil || len(items) == 0 {
		t.Fatalf("feed: %v, %d items", err, len(items))
	}
	return items[len(items)-1]
}

func TestReloadConfig(t *testing.T) {
	s, path := reloadServer(t)
	cfg := "daemon:\n  max_runs: 2\nrepos:\n  app:\n    gate: make check\n    autonomy:\n      push: shepherd\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	live := s.LiveConfig()
	if live.Daemon.MaxRuns != 2 || live.Profile("app").Autonomy.Push != config.Shepherd {
		t.Errorf("live config not swapped: %+v", live.Daemon)
	}
	if s.Runner.Conf().Daemon.MaxRuns != 2 || s.Fold.Conf().Profile("app").Gate != "make check" {
		t.Error("the Runner and Fold still hold the old config")
	}
	got := lastFeed(t, s.Store)
	if got.Kind != FeedConfig || got.Text != "config reloaded: daemon.max_runs 4 to 2, repos.app added" {
		t.Errorf("feed = %s %q", got.Kind, got.Text)
	}

	// The same file again changes nothing, and says so.
	if err := s.ReloadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lastFeed(t, s.Store); got.Text != "config reloaded, nothing changed" {
		t.Errorf("feed = %q", got.Text)
	}
}

func TestReloadRefusedKeepsConfig(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, why string }{
		"unknown key":         {"daemon:\n  max_run: 2\n", "max_run"},
		"bad autonomy":        {"repos:\n  app:\n    autonomy:\n      merge: sometimes\n", "repos.app.autonomy.merge"},
		"push without a gate": {"repos:\n  app:\n    autonomy:\n      push: shepherd\n", "no gate is set"},
	} {
		t.Run(name, func(t *testing.T) {
			s, path := reloadServer(t)
			os.WriteFile(path, []byte("daemon:\n  max_runs: 3\n"), 0o600)
			if err := s.ReloadConfig(context.Background()); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(path, []byte(tc.yaml), 0o600)
			err := s.ReloadConfig(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("reload = %v, want %q", err, tc.why)
			}
			if s.LiveConfig().Daemon.MaxRuns != 3 || s.Runner.Conf().Daemon.MaxRuns != 3 {
				t.Error("a refused reload replaced the config")
			}
			got := lastFeed(t, s.Store)
			if got.Kind != FeedConfig || !strings.HasPrefix(got.Text, "config not reloaded, keeping the current one: ") ||
				!strings.Contains(got.Text, tc.why) {
				t.Errorf("feed = %s %q", got.Kind, got.Text)
			}
		})
	}
}

func TestConfigChanges(t *testing.T) {
	old := config.Default()
	next := config.Default()
	next.Daemon.Listen = "127.0.0.1:7777"
	next.Daemon.Budget = nil
	next.Defaults.Gate = "make test"
	old.Repos = map[string]config.Profile{"a": {}, "b": {Gate: "x"}}
	next.Repos = map[string]config.Profile{"b": {Gate: "y"}, "c": {}}
	want := "daemon.listen 127.0.0.1:0 to 127.0.0.1:7777 (takes a restart), daemon.budget 20 to unset, " +
		"defaults, repos.a removed, repos.b, repos.c added"
	if got := configChanges(old, next); got != want {
		t.Errorf("changes =\n %q\nwant\n %q", got, want)
	}
}

// A reload while the Fold and the Runner read their config is not a data race: both
// hold it behind an atomic pointer (run with -race to check).
func TestReloadWhileReading(t *testing.T) {
	s, path := reloadServer(t)
	if err := os.WriteFile(path, []byte("daemon:\n  max_runs: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = s.Fold.Conf().Profile("app")
			_ = s.Runner.Conf().Daemon.MaxRuns
		}
	}()
	for range 20 {
		if err := s.ReloadConfig(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	if s.Fold.Conf().Daemon.MaxRuns != 2 || s.Runner.Conf().Daemon.MaxRuns != 2 {
		t.Error("the reloaded config is not in force")
	}
}
