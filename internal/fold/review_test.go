package fold

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/forge"
)

// commitAt commits a file in dir with an author and committer date.
func commitAt(t *testing.T, dir, file, date string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
	os.WriteFile(filepath.Join(dir, file), []byte(file+date+"\n"), 0o644)
	gitT(t, dir, "add", ".")
	cmd := exec.Command("git", "-C", dir, "commit", "-q", "-m", "add "+file)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
}

func TestReviewVerdicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	root := filepath.Join(f.ws, "app-worktrees")
	add := func(name string) string {
		dir := filepath.Join(root, name)
		gitT(t, f.repo.Path, "worktree", "add", "-q", "-b", name, dir, "origin/main")
		return dir
	}
	// landed: its change reached main by another commit with the same content (a rebase).
	landed := add("landed")
	commitAt(t, landed, "src/a.go", "2026-08-01T10:00:00Z")
	// The main checkout shares the worktrees' objects; land a copy of the commit from it.
	gitT(t, f.repo.Path, "cherry-pick", gitT(t, landed, "rev-parse", "HEAD"))
	gitT(t, f.repo.Path, "push", "-q", "origin", "HEAD:main")
	// old: unlanded work, nothing recent.
	commitAt(t, add("old"), "src/old.go", "2026-06-01T10:00:00Z")
	// fresh: unlanded but recent.
	f.commit(add("fresh"), "src/fresh.go")
	// dirty: uncommitted work.
	os.WriteFile(filepath.Join(add("dirty"), "wip.txt"), []byte("x"), 0o644)
	// merged: in a merged request (git cannot see it: the forge can).
	merged := add("merged")
	commitAt(t, merged, "src/m.go", "2026-06-01T10:00:00Z")
	head := gitT(t, merged, "rev-parse", "HEAD")
	f.fold.ForgeFor = func(string) (forge.Forge, error) { return mergedForge{"merged", head}, nil }

	reviews, err := f.fold.ReviewRepo(ctx, f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Review{}
	for _, r := range reviews {
		got[r.Branch] = r
	}
	want := map[string]string{"landed": Finished, "old": Unclear, "fresh": Live, "dirty": Live, "merged": Finished}
	for b, v := range want {
		if got[b].Verdict != v {
			t.Errorf("%s: %s (%s), want %s; evidence %v", b, got[b].Verdict, got[b].Why, v, got[b].Evidence)
		}
	}
	if len(got["old"].Unlanded) != 1 || !strings.Contains(got["old"].Unlanded[0], "add src/old.go") {
		t.Errorf("old's unlanded = %v", got["old"].Unlanded)
	}

	// Retire: finished goes (branch kept); unclear, live and lanes stay.
	if _, err := f.fold.Retire(ctx, f.repo.ID, landed); err != nil {
		t.Fatalf("retire finished: %v", err)
	}
	if _, err := os.Stat(landed); !os.IsNotExist(err) {
		t.Error("finished worktree still there")
	}
	gitT(t, f.repo.Path, "rev-parse", "--verify", "refs/heads/landed")
	for _, name := range []string{"old", "fresh", "dirty"} {
		if _, err := f.fold.Retire(ctx, f.repo.ID, filepath.Join(root, name)); !errors.Is(err, ErrRefused) {
			t.Errorf("retired %s: %v", name, err)
		}
	}
	lane := f.open("feat/lane")
	if _, err := f.fold.Retire(ctx, f.repo.ID, lane.Worktree); !errors.Is(err, ErrRefused) {
		t.Errorf("retired a lane: %v", err)
	}
}

func TestInferScope(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.ws, "app-worktrees", "inf")
	gitT(t, f.repo.Path, "worktree", "add", "-q", "-b", "inf", dir, "origin/main")
	f.commit(dir, "app/web/src/routes/earnings/page.tsx")
	f.commit(dir, "app/web/src/routes/earnings/data.ts")
	f.commit(dir, "php/Service/Earnings.php")
	f.commit(dir, "CHANGELOG.md")
	got := strings.Join(inferScope(context.Background(), f.repo.Path, "inf", "origin/main"), " ")
	if got != "CHANGELOG.md app/web/src/routes/earnings/** php/Service/**" {
		t.Errorf("inferred = %q", got)
	}
}

func TestReviewIgnoresARootCommit(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.ws, "app-worktrees", "rooted")
	// `worktree add --orphan` needs git 2.42; `checkout --orphan` in a detached
	// worktree gives the same parentless branch on any git.
	gitT(t, f.repo.Path, "worktree", "add", "-q", "--detach", dir)
	gitT(t, dir, "checkout", "-q", "--orphan", "rooted")
	gitT(t, dir, "rm", "-rfq", ".")
	commitAt(t, dir, "everything.txt", "2026-01-01T10:00:00Z")
	reviews, _ := f.fold.ReviewRepo(context.Background(), f.repo.ID)
	for _, r := range reviews {
		if r.Branch == "rooted" && r.Verdict != Finished {
			t.Errorf("a root-commit-only branch = %s (%s)", r.Verdict, r.Why)
		}
	}
}
