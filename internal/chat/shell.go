package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// KindShell is the result of a `!` command the person ran from the chat. Its text is
// "$ <command>", the output, then "exit <code>"; Line.Full holds the whole output.
const KindShell = "shell"

const (
	// shellThreadLines is how many lines of output the thread shows; the rest is in the
	// transcript (Ctrl-O).
	shellThreadLines = 30
	// shellKeepBytes bounds the output kept for one command.
	shellKeepBytes = 1 << 20
	// shellNoteLines and shellNoteBytes bound the tail of output handed to the desk.
	shellNoteLines = 20
	shellNoteBytes = 2000
	// shellNotesKept is how many commands the desk is told about at most.
	shellNotesKept = 5
)

// ShellFunc runs command in dir, writing its stdout and stderr to out, and returns its
// exit code. The error is for a command that could not run, or that ctx stopped.
type ShellFunc func(ctx context.Context, dir, command string, out io.Writer) (int, error)

// shellRequest is a parsed `!` line: lane is "" for the workspace root.
type shellRequest struct {
	lane, command string
}

// parseShell reads a line starting with "!": `!<cmd>` or `!@<lane> <cmd>`.
func parseShell(text string) (shellRequest, error) {
	body := strings.TrimSpace(strings.TrimPrefix(text, "!"))
	const usage = "usage: !<command>, or !@<lane> <command> to run it in a lane's worktree"
	if body == "" {
		return shellRequest{}, errors.New(usage)
	}
	if !strings.HasPrefix(body, "@") {
		return shellRequest{command: body}, nil
	}
	lane, cmd, _ := strings.Cut(body[1:], " ")
	cmd = strings.TrimSpace(cmd)
	if lane == "" || cmd == "" {
		return shellRequest{}, errors.New(usage)
	}
	return shellRequest{lane: lane, command: cmd}, nil
}

// shellDir finds the directory for a request: the workspace root, or the worktree of
// the open lane named lane or repo/lane.
func (m *Model) shellDir(ctx context.Context, req shellRequest) (string, error) {
	if req.lane == "" {
		if m.workspace.Path == "" {
			return "", errors.New("the workspace has no path")
		}
		return m.workspace.Path, nil
	}
	lanes, err := m.api.Lanes(ctx, m.workspace.ID, 0)
	if err != nil {
		return "", err
	}
	var byName, byRepo []string // worktrees; names to show
	var names []string
	for _, l := range lanes {
		if l.State != store.LaneOpen {
			continue
		}
		if l.Repo+"/"+l.Name == req.lane {
			byRepo = append(byRepo, l.Worktree)
		}
		if l.Name == req.lane {
			byName = append(byName, l.Worktree)
			names = append(names, l.Repo+"/"+l.Name)
		}
	}
	switch {
	case len(byName) == 1:
		return checkWorktree(req.lane, byName[0])
	case len(byName) > 1:
		sort.Strings(names)
		return "", fmt.Errorf("lane %s is in several repos: use !@%s <command>", req.lane, strings.Join(names, " or !@"))
	case len(byRepo) == 1:
		return checkWorktree(req.lane, byRepo[0])
	}
	return "", fmt.Errorf("no open lane %q (the dock lists the open lanes; a lane in a repo of the same name as another is !@<repo>/<lane>)", req.lane)
}

func checkWorktree(lane, dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("lane %s has no worktree", lane)
	}
	return dir, nil
}

// runShell is the real ShellFunc: the person's shell, no stdin, pagers off.
func runShell(ctx context.Context, dir, command string, out io.Writer) (int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, sh, "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PAGER=cat", "GIT_PAGER=cat")
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second
	setProcessGroup(cmd)
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return -1, ctx.Err()
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	case err != nil:
		return -1, err
	}
	return 0, nil
}

// shellBuffer keeps what a command printed, up to shellKeepBytes, and tells the model
// as it comes.
type shellBuffer struct {
	mu      sync.Mutex
	buf     []byte
	dropped int
	notify  func(string)
}

func (b *shellBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	room := shellKeepBytes - len(b.buf)
	if room < 0 {
		room = 0
	}
	keep := p
	if len(keep) > room {
		keep = keep[:room]
		b.dropped += len(p) - room
	}
	b.buf = append(b.buf, keep...)
	b.mu.Unlock()
	if b.notify != nil && len(keep) > 0 {
		b.notify(string(keep))
	}
	return len(p), nil
}

func (b *shellBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	if b.dropped > 0 {
		s += fmt.Sprintf("\n[%d more bytes not kept]", b.dropped)
	}
	return s
}

// shellChunkMsg is output arriving from the running command.
type shellChunkMsg string

type shellDoneMsg struct {
	command, dir, output string
	code                 int
	err                  error
	notRun               bool // the directory could not be found; err says why
}

// shellNote is what the desk is told of a finished command with its next message.
type shellNote struct {
	command, dir, tail string
	code               int
	failed             string // why it did not run to its end, if so
}

// runShellLine starts a `!` command. Its output streams into the live region; the
// result is one KindShell entry when it ends.
func (m *Model) runShellLine(text string) tea.Cmd {
	req, err := parseShell(text)
	if err != nil {
		m.add(Line{Kind: KindError, Text: err.Error()})
		return nil
	}
	if m.shellCancel != nil {
		m.add(Line{Kind: KindError, Text: "a command is already running; Ctrl-C stops it"})
		return nil
	}
	m.add(Line{Kind: KindYou, Text: text})
	ctx, cancel := context.WithCancel(m.ctx)
	m.shellCancel, m.shellLive, m.shellCommand = cancel, "", req.command
	ch := make(chan tea.Msg, 256)
	m.shellCh = ch
	run := m.shell
	go func() {
		dir, err := m.shellDir(ctx, req)
		if err != nil {
			ch <- shellDoneMsg{command: req.command, err: err, notRun: true}
			return
		}
		buf := &shellBuffer{notify: func(s string) {
			// A chunk is dropped rather than stall the command when the screen lags.
			select {
			case ch <- shellChunkMsg(s):
			default:
			}
		}}
		code, err := run(ctx, dir, req.command, buf)
		ch <- shellDoneMsg{command: req.command, dir: dir, output: buf.text(), code: code, err: err}
	}()
	return waitShell(ch)
}

func waitShell(ch chan tea.Msg) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return <-ch }
}

// onShellDone adds the entry and keeps a note for the desk's next message.
func (m *Model) onShellDone(d shellDoneMsg) {
	m.shellCancel, m.shellCh, m.shellLive, m.shellCommand = nil, nil, "", ""
	if d.notRun {
		m.add(Line{Kind: KindError, Text: d.err.Error()})
		return
	}
	out := redact.String(d.output)
	note := shellNote{command: redact.String(d.command), dir: d.dir, code: d.code, tail: tailOf(out, shellNoteLines, shellNoteBytes)}
	status := fmt.Sprintf("exit %d", d.code)
	switch {
	case errors.Is(d.err, context.Canceled):
		status, note.failed = "stopped (Ctrl-C)", "the person stopped it with Ctrl-C"
	case d.err != nil:
		status, note.failed = "could not run: "+redact.String(d.err.Error()), "it could not run: "+redact.String(d.err.Error())
	}
	shown, omitted := lastLines(strings.TrimRight(out, "\n"), shellThreadLines)
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", note.command)
	if omitted > 0 {
		fmt.Fprintf(&b, "… %d earlier line(s) not shown; Ctrl-O for all\n", omitted)
	}
	if shown != "" {
		b.WriteString(shown + "\n")
	}
	short := b.String() + status
	full := fmt.Sprintf("$ %s\n", note.command) + strings.TrimRight(out, "\n") + "\n" + status
	if omitted == 0 {
		full = ""
	}
	m.add(Line{Kind: KindShell, Text: short, Full: full})
	m.shellNotes = append(m.shellNotes, note)
	if len(m.shellNotes) > shellNotesKept {
		m.shellNotes = m.shellNotes[len(m.shellNotes)-shellNotesKept:]
	}
}

// withShellNotes puts the notes of commands the person ran ahead of their message, so the
// desk can act on the results, and clears them. It is one turn with the message, not a
// turn of its own.
func (m *Model) withShellNotes(text string) string {
	if len(m.shellNotes) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString("[Shepherd] The person ran shell command(s) themselves from the chat (you have no shell); this is context, not a request:\n")
	for _, n := range m.shellNotes {
		fmt.Fprintf(&b, "- the person ran `%s` in %s: ", n.command, n.dir)
		if n.failed != "" {
			fmt.Fprintf(&b, "%s.", n.failed)
		} else {
			fmt.Fprintf(&b, "exit code %d.", n.code)
		}
		if n.tail != "" {
			b.WriteString(" End of its output:\n" + indentLines(n.tail, "    "))
		} else {
			b.WriteString(" No output.")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nTheir message:\n" + text)
	m.shellNotes = nil
	return b.String()
}

func indentLines(s, p string) string {
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}

// lastLines keeps the last n lines of s, and says how many it left out.
func lastLines(s string, n int) (string, int) {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s, 0
	}
	return strings.Join(lines[len(lines)-n:], "\n"), len(lines) - n
}

// tailOf is the end of s: at most lines lines and bytes bytes.
func tailOf(s string, lines, bytes int) string {
	s, _ = lastLines(strings.TrimRight(s, "\n"), lines)
	if len(s) > bytes {
		s = "…" + s[len(s)-bytes:]
	}
	return s
}

// shellLiveRows is the tail of a running command's output, for the live region.
func (m *Model) shellLiveRows(room int) []string {
	if m.shellCancel == nil {
		return nil
	}
	if room < 2 {
		room = 2
	}
	rows := []string{styleHead.Render("$ "+m.shellCommand+"  ") + styleInfo.Render("running; Ctrl-C stops it")}
	if m.shellLive != "" {
		tail, _ := lastLines(strings.TrimRight(redact.String(m.shellLive), "\n"), room-1)
		for _, l := range strings.Split(tail, "\n") {
			rows = append(rows, "  "+styleInfo.Render(l))
		}
	}
	return rows
}
