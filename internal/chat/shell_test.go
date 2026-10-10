package chat

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestParseShell(t *testing.T) {
	for _, c := range []struct {
		in         string
		lane, cmd  string
		wantErrSub string
	}{
		{"!ls -la", "", "ls -la", ""},
		{"! git status", "", "git status", ""},
		{"!@feat/x go test ./...", "feat/x", "go test ./...", ""},
		{"!@app/feat/x   make", "app/feat/x", "make", ""},
		{"!", "", "", "usage"},
		{"!@feat/x", "", "", "usage"},
		{"!@ ls", "", "", "usage"},
	} {
		r, err := parseShell(c.in)
		if c.wantErrSub != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErrSub) {
				t.Errorf("%q: err = %v", c.in, err)
			}
			continue
		}
		if err != nil || r.lane != c.lane || r.command != c.cmd {
			t.Errorf("%q = %+v, %v", c.in, r, err)
		}
	}
}

type shellCall struct{ dir, command string }

// fakeShell records the calls and prints out, then returns code.
func fakeShell(calls *[]shellCall, out string, code int) ShellFunc {
	return func(_ context.Context, dir, command string, w io.Writer) (int, error) {
		*calls = append(*calls, shellCall{dir, command})
		io.WriteString(w, out)
		return code, nil
	}
}

func TestShellRunsInTheWorkspaceAndNeverReachesTheDesk(t *testing.T) {
	m, d, _ := newTestModel()
	var calls []shellCall
	m.shell = fakeShell(&calls, "a.go\nb.go\n", 0)
	typeLine(t, m, "!ls")
	if len(calls) != 1 || calls[0] != (shellCall{"/w", "ls"}) {
		t.Fatalf("calls = %+v", calls)
	}
	if len(d.got) != 0 {
		t.Errorf("the desk was sent %q", d.got)
	}
	if !has(m.Lines(), KindShell, "$ ls\na.go\nb.go\nexit 0") {
		t.Errorf("thread = %+v", m.Lines())
	}
	if m.shellCancel != nil {
		t.Error("command still marked running")
	}
}

func TestShellNonZeroExitIsShown(t *testing.T) {
	m, _, _ := newTestModel()
	var calls []shellCall
	m.shell = fakeShell(&calls, "boom\n", 3)
	typeLine(t, m, "!false")
	if !has(m.Lines(), KindShell, "boom\nexit 3") {
		t.Errorf("thread = %+v", m.Lines())
	}
}

func TestShellInALane(t *testing.T) {
	m, _, a := newTestModel()
	a.lanes = []api.LaneView{
		{Lane: store.Lane{Name: "feat/x", State: store.LaneOpen, Worktree: "/wt/app-x"}, Repo: "app"},
		{Lane: store.Lane{Name: "feat/y", State: store.LaneOpen, Worktree: "/wt/app-y"}, Repo: "app"},
		{Lane: store.Lane{Name: "fix/z", State: store.LaneOpen, Worktree: "/wt/app-z"}, Repo: "app"},
		{Lane: store.Lane{Name: "fix/z", State: store.LaneOpen, Worktree: "/wt/lib-z"}, Repo: "lib"},
		{Lane: store.Lane{Name: "old", State: store.LaneClosed, Worktree: "/wt/old"}, Repo: "app"},
	}
	var calls []shellCall
	m.shell = fakeShell(&calls, "ok\n", 0)

	typeLine(t, m, "!@feat/x git status")
	typeLine(t, m, "!@lib/fix/z make")
	typeLine(t, m, "!@app/feat/y make")
	want := []shellCall{{"/wt/app-x", "git status"}, {"/wt/lib-z", "make"}, {"/wt/app-y", "make"}}
	if len(calls) != 3 || calls[0] != want[0] || calls[1] != want[1] || calls[2] != want[2] {
		t.Fatalf("calls = %+v", calls)
	}

	typeLine(t, m, "!@nope ls")
	if !has(m.Lines(), KindError, `no open lane "nope"`) {
		t.Errorf("unknown lane: %+v", m.Lines())
	}
	typeLine(t, m, "!@old ls")
	if !has(m.Lines(), KindError, `no open lane "old"`) {
		t.Errorf("closed lane: %+v", m.Lines())
	}
	typeLine(t, m, "!@fix/z ls")
	if !has(m.Lines(), KindError, "several repos: use !@app/fix/z") {
		t.Errorf("ambiguous lane: %+v", m.Lines())
	}
	if len(calls) != 3 {
		t.Errorf("a command ran for a bad lane: %+v", calls)
	}
}

func TestShellOutputIsRedactedAndCapped(t *testing.T) {
	m, _, _ := newTestModel()
	var calls []shellCall
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	b.WriteString("token=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")
	m.shell = fakeShell(&calls, b.String(), 0)
	typeLine(t, m, "!dump")
	var l Line
	for _, x := range m.Lines() {
		if x.Kind == KindShell {
			l = x
		}
	}
	if strings.Contains(l.Text, "ghp_abc") || strings.Contains(l.Full, "ghp_abc") {
		t.Errorf("secret shown: %q", l.Text)
	}
	if n := strings.Count(l.Text, "\n"); n > shellThreadLines+4 {
		t.Errorf("thread shows %d lines", n)
	}
	if !strings.Contains(l.Text, "Ctrl-O for all") || strings.Count(l.Full, "line\n") != 100 {
		t.Errorf("full output not kept: text %q", l.Text)
	}
	if !strings.Contains(m.renderTranscript(), "exit 0") || strings.Count(m.renderTranscript(), "line") < 100 {
		t.Error("transcript lacks the full output")
	}
}

func TestShellResultGoesToTheDeskWithTheNextMessage(t *testing.T) {
	m, d, _ := newTestModel()
	var calls []shellCall
	m.shell = fakeShell(&calls, "FAIL: TestX\n", 1)
	typeLine(t, m, "!@x go test")
	typeLine(t, m, "!go test ./...")
	if len(d.got) != 0 {
		t.Fatalf("a shell command alone started a desk turn: %q", d.got)
	}
	typeLine(t, m, "why did it fail?")
	if len(d.got) != 1 {
		t.Fatalf("desk got %q", d.got)
	}
	got := d.got[0]
	for _, want := range []string{"the person ran `go test ./...` in /w", "exit code 1", "FAIL: TestX", "why did it fail?"} {
		if !strings.Contains(got, want) {
			t.Errorf("note lacks %q:\n%s", want, got)
		}
	}
	typeLine(t, m, "and again")
	if strings.Contains(d.got[1], "the person ran") {
		t.Errorf("the note was sent twice: %q", d.got[1])
	}
}

func TestShellCtrlCStopsTheCommandNotTheChat(t *testing.T) {
	m, d, _ := newTestModel()
	started := make(chan struct{})
	m.shell = func(ctx context.Context, dir, command string, w io.Writer) (int, error) {
		io.WriteString(w, "partial\n")
		close(started)
		<-ctx.Done()
		return -1, ctx.Err()
	}
	m.input.SetValue("!sleep 100")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	<-started
	if m.shellCancel == nil {
		t.Fatal("not running")
	}
	_, quit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m.quitting {
		t.Fatal("Ctrl-C quit the chat")
	}
	_ = quit
	drive(t, m, cmd)
	if !has(m.Lines(), KindShell, "stopped (Ctrl-C)") {
		t.Errorf("thread = %+v", m.Lines())
	}
	typeLine(t, m, "ok")
	if !strings.Contains(d.got[0], "stopped it with Ctrl-C") {
		t.Errorf("desk note: %q", d.got)
	}
	// With nothing running, Ctrl-C quits as before.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.quitting {
		t.Error("Ctrl-C did not quit when idle")
	}
}

func TestRunShellReallyRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Setenv("SHELL", "/bin/sh")
	dir := t.TempDir()
	var b shellBuffer
	code, err := runShell(context.Background(), dir, `pwd; echo "pager=$PAGER/$GIT_PAGER" >&2; exit 3`, &b)
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v", code, err)
	}
	out := b.text()
	if !strings.Contains(out, "pager=cat/cat") || !strings.Contains(out, "/") {
		t.Errorf("output %q", out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runShell(ctx, dir, "sleep 5", io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled run: %v", err)
	}
}

func TestDeskStillHasNoShell(t *testing.T) {
	d := ClaudeDesk{Bin: "claude", Shepherd: "/bin/shepherd", Dir: "/w"}
	if !strings.Contains(strings.Join(d.Args("S", true), " "), "--disallowedTools Edit Write Bash") {
		t.Error("the desk may run Bash")
	}
}
