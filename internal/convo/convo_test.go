package convo

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func writeSession(t *testing.T, dir, id string, lines ...string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	f := filepath.Join(dir, id+".jsonl")
	os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	return f
}

func TestScanAndRead(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(root, "repo-worktrees", "x"), 0o755)
	os.MkdirAll(repo, 0o755)
	projects := filepath.Join(root, "projects")
	writeSession(t, filepath.Join(projects, "a"), "aaa-111",
		`{"type":"mode","sessionId":"aaa-111"}`,
		`{"type":"user","sessionId":"aaa-111","cwd":"`+repo+`","gitBranch":"dev","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		`{"type":"user","sessionId":"aaa-111","cwd":"`+repo+`","gitBranch":"feat/x","timestamp":"2026-09-01T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Set up the payments integration, token glpat-AbCdEfGhIjKlMnOpQrStUv"}]}}`,
		`{"type":"assistant","sessionId":"aaa-111","cwd":"`+repo+`","gitBranch":"feat/x","timestamp":"2026-09-03T12:00:00Z"}`,
	)
	writeSession(t, filepath.Join(projects, "b"), "bbb-222",
		`{"type":"user","sessionId":"bbb-222","cwd":"`+filepath.Join(root, "repo-worktrees", "x")+`","timestamp":"2026-09-05T09:00:00Z","message":{"role":"user","content":"in a worktree"}}`,
		`{"type":"summary","summary":"Worktree work, summarised"}`,
	)
	writeSession(t, filepath.Join(projects, "c"), "ccc-333",
		`{"type":"user","sessionId":"ccc-333","cwd":"/somewhere/else","message":{"role":"user","content":"not ours"}}`,
	)
	found, err := Scan(projects, []string{repo, filepath.Join(root, "repo-worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].ID != "bbb-222" {
		t.Fatalf("found = %+v", found)
	}
	a := found[1]
	if a.ID != "aaa-111" || !strings.HasPrefix(a.Title, "Set up the payments integration") || strings.Contains(a.Title, "glpat-") {
		t.Errorf("title = %q", a.Title)
	}
	if a.Branches[0] != "feat/x" || a.Started.Day() != 1 || a.Last.Day() != 3 {
		t.Errorf("a = %+v", a)
	}
	if found[0].Title != "Worktree work, summarised" {
		t.Errorf("summary title = %q", found[0].Title)
	}
}

func TestInUseAndArgs(t *testing.T) {
	f := writeSession(t, t.TempDir(), "x", `{}`)
	if !InUse(f) {
		t.Error("a file written now should look in use")
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(f, old, old)
	if InUse(f) {
		t.Error("an hour-old file should not look in use")
	}
	args := strings.Join(AskArgs("S"), " ")
	for _, want := range []string{"-p --resume S", "--disallowedTools Edit Write Bash"} {
		if !strings.Contains(args, want) {
			t.Errorf("ask args lack %q: %s", want, args)
		}
	}
}

// fakeClaude answers with what it read on standard input, and records its command line
// in args.txt next to it.
const fakeClaude = `#!/bin/sh
printf '%s ' "$@" > "$(dirname "$0")/args.txt"
q=$(cat)
printf '{"type":"result","result":"you asked: %s","total_cost_usd":0.01}\n' "$q"
`

// The question goes on standard input, never on the command line.
func TestAskSendsTheQuestionOnStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaude), 0o755)
	a, err := Ask(context.Background(), bin, store.Conversation{ID: "S1", Dir: t.TempDir()}, "where is the webhook secret?")
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "you asked: where is the webhook secret?" {
		t.Errorf("answer = %q", a.Text)
	}
	args, _ := os.ReadFile(filepath.Join(filepath.Dir(bin), "args.txt"))
	if strings.Contains(string(args), "webhook") || !strings.Contains(string(args), "--resume S1") {
		t.Errorf("command line: %s", args)
	}
}
