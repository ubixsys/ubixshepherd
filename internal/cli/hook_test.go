package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// The hook passes git's remote argument through to the daemon, which lets a running
// agent push its own lane to origin only when the repo says autonomy.push: agent.
func TestHookPrePushRemote(t *testing.T) {
	h := newHarness(t, "", false)
	root, app := laneWorkspace(t)
	if code := h.run("init", root, "--yes"); code != 0 {
		t.Fatalf("init: %s", h.err)
	}
	h.env.Cwd = app
	if code := h.run("lane", "open", "feat/x", "--scope", "src/**"); code != 0 {
		t.Fatalf("lane open: %s", h.err)
	}
	wt := filepath.Join(root, "app-worktrees", "feat-x")
	os.MkdirAll(filepath.Join(wt, "src"), 0o755)
	os.WriteFile(filepath.Join(wt, "src", "a.go"), []byte("package a\n"), 0o644)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("add", ".")
	git("commit", "-q", "-m", "feat: a")
	head := git("rev-parse", "HEAD")
	url := git("remote", "get-url", "origin")

	ctx := context.Background()
	lane, err := h.srv.Store.Lane(ctx, 1)
	if err != nil || lane.Name != "feat/x" {
		t.Fatalf("lane: %+v %v", lane, err)
	}
	if _, err := h.srv.Store.CreateRun(ctx, store.Run{LaneID: lane.ID, Agent: "claude", Prompt: "x", State: store.RunRunning, Log: "l"}); err != nil {
		t.Fatal(err)
	}
	profile := func(push string) {
		cfg := config.Default()
		p := config.Profile{}
		p.Autonomy.Push = push
		if push == config.Shepherd {
			p.Gate = "true"
		}
		cfg.Repos = map[string]config.Profile{"app": p}
		h.srv.Fold.SetConfig(cfg)
	}
	push := func(args ...string) int {
		t.Helper()
		h.env.Cwd = wt
		h.env.Stdin = strings.NewReader("refs/heads/feat/x " + head + " refs/heads/feat/x " + strings.Repeat("0", 40) + "\n")
		return h.run(append([]string{"hook", "pre-push"}, args...)...)
	}

	profile(config.Agent)
	if code := push("origin", url); code != 0 {
		t.Errorf("agent push to origin under push: agent refused: %d %s", code, h.err)
	}
	if code := push(url, url); code != 0 {
		t.Errorf("agent push to origin's URL refused: %d %s", code, h.err)
	}
	if code := push("elsewhere", "https://example.com/x.git"); code == 0 || !strings.Contains(h.err.String(), "only to the repo's origin") {
		t.Errorf("agent push to another remote allowed: %d %s", code, h.err)
	}
	if code := push(); code == 0 {
		t.Errorf("an old hook, with no remote, was let through: %s", h.err)
	}
	for _, p := range []string{"", config.Human, config.Shepherd} {
		profile(p)
		if code := push("origin", url); code == 0 || !strings.Contains(h.err.String(), "never push") {
			t.Errorf("push: %q let the agent push: %d %s", p, code, h.err)
		}
	}
}

// A push by an agent Shepherd started is checked with the run's SHEPHERD_URL and
// SHEPHERD_TOKEN, with no runtime file to read; only when both are unset does the hook
// fall back to the runtime file.
func TestHookPrePushPrefersTheRunsToken(t *testing.T) {
	h := newHarness(t, "", false)
	root, app := laneWorkspace(t)
	if code := h.run("init", root, "--yes"); code != 0 {
		t.Fatalf("init: %s", h.err)
	}
	h.env.Cwd = app
	if code := h.run("lane", "open", "feat/x", "--scope", "src/**"); code != 0 {
		t.Fatalf("lane open: %s", h.err)
	}
	wt := filepath.Join(root, "app-worktrees", "feat-x")
	out, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))
	push := func() int {
		t.Helper()
		h.env.Cwd = wt
		h.env.Stdin = strings.NewReader("refs/heads/feat/x " + head + " refs/heads/feat/x " + strings.Repeat("0", 40) + "\n")
		return h.run("hook", "pre-push", "origin", "git@example.com:t/app.git")
	}
	rtPath := h.env.Layout.Runtime()
	b, err := os.ReadFile(rtPath)
	if err != nil {
		t.Fatal(err)
	}
	var rt api.Runtime
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	tok, err := h.srv.MintWorker(1)
	if err != nil {
		t.Fatal(err)
	}

	// Without a runtime file only the environment can reach the daemon.
	if err := os.Remove(rtPath); err != nil {
		t.Fatal(err)
	}
	if code := push(); code == 0 || !strings.Contains(h.err.String(), "cannot check this push") {
		t.Fatalf("no env, no runtime file: %d %s", code, h.err)
	}
	t.Setenv(dispatch.EnvURL, "http://"+rt.Addr)
	t.Setenv(dispatch.EnvToken, tok)
	if code := push(); code != 0 {
		t.Errorf("the run's token was not used: %d %s", code, h.err)
	}

	// A wrong token in the environment is refused: it does not fall back to the
	// operator's token in the runtime file.
	os.WriteFile(rtPath, b, 0o600)
	t.Setenv(dispatch.EnvToken, "revoked")
	if code := push(); code == 0 || !strings.Contains(h.err.String(), "cannot check this push") {
		t.Errorf("a bad run token fell back to the runtime file: %d %s", code, h.err)
	}
	// One of the two set is a mistake, not a reason to fall back.
	t.Setenv(dispatch.EnvToken, "")
	if code := push(); code == 0 || !strings.Contains(h.err.String(), "must be set together") {
		t.Errorf("URL without a token: %d %s", code, h.err)
	}
	// Both unset: the runtime file, as for the person's own push.
	t.Setenv(dispatch.EnvURL, "")
	if code := push(); code != 0 {
		t.Errorf("runtime file fallback: %d %s", code, h.err)
	}
}
