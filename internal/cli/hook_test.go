package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/config"
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
