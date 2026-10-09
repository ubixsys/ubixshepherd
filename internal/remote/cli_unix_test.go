//go:build unix

package remote_test

// End to end through the shepherd command: cli.Run with --host, a fake ssh on PATH (the
// helper role in this test binary, see tunnel_unix_test.go in package remote), and a real
// daemon server on the far side of the fake tunnel.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/cli"
	"github.com/ubixsys/ubixshepherd/internal/config"
	"github.com/ubixsys/ubixshepherd/internal/daemon"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/paths"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

func TestMain(m *testing.M) {
	git.ClearEnvConfig()
	os.Exit(m.Run())
}

// far is a daemon on "the other machine": a server, and the daemon.json it would write.
type far struct {
	home  string // its Shepherd home, with daemon.json
	token string
	ts    *httptest.Server
}

func startFar(t *testing.T) *far {
	t.Helper()
	home := t.TempDir()
	st, err := sqlite.Open(context.Background(), filepath.Join(home, "shepherd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := daemon.NewServer(st, config.Default(), "cfg", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Runner = &dispatch.Runner{Store: st, Config: config.Default(), Dir: filepath.Join(home, "runs"), Log: srv.Log}
	f := &far{home: home, token: srv.Token, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(f.ts.Close)
	f.writeRuntime(t)
	return f
}

func (f *far) writeRuntime(t *testing.T) {
	t.Helper()
	rt, _ := json.Marshal(api.Runtime{Addr: strings.TrimPrefix(f.ts.URL, "http://"), Token: f.token, PID: 4242})
	if err := os.WriteFile(filepath.Join(f.home, "daemon.json"), rt, 0o600); err != nil {
		t.Fatal(err)
	}
}

// useFakeSSH puts a fake ssh on PATH whose `cat` prints fixture and whose tunnels forward
// like the real one. It returns the file the fake appends each tunnel's pid to.
func useFakeSSH(t *testing.T, fixture string) (pidfile string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nREMOTE_TEST_ROLE=ssh exec %q -test.run=TestHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile = filepath.Join(dir, "pids")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REMOTE_TEST_FIXTURE", fixture)
	t.Setenv("REMOTE_TEST_PIDFILE", pidfile)
	for _, k := range []string{"SHEPHERD_HOST", "SHEPHERD_WORKSPACE", "SHEPHERD_REPO", "SHEPHERD_HOME"} {
		t.Setenv(k, "")
	}
	return pidfile
}

func pids(t *testing.T, pidfile string) []int {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

func gone(pid int) bool {
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

type result struct {
	code        int
	out, errOut string
}

// shepherd runs the command from a client machine whose home has the given hosts.yaml.
func shepherd(t *testing.T, home string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env := cli.Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Layout: paths.Layout{Home: home}, Cwd: home, Client: "cli"}
	code := cli.Run(context.Background(), env, args)
	return result{code, out.String(), errb.String()}
}

func clientHome(t *testing.T, hosts string) string {
	t.Helper()
	home := t.TempDir()
	if hosts != "" {
		if err := os.WriteFile(filepath.Join(home, "hosts.yaml"), []byte(hosts), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestStatusOverTheTunnelLeavesNothingBehind(t *testing.T) {
	f := startFar(t)
	pidfile := useFakeSSH(t, filepath.Join(f.home, "daemon.json"))
	r := shepherd(t, clientHome(t, ""), "--host", "ssh://me@box", "status")
	if r.code != 0 || !strings.Contains(r.out, "daemon   running") {
		t.Fatalf("status: %d\n%s%s", r.code, r.out, r.errOut)
	}
	if strings.Contains(r.out+r.errOut, f.token) {
		t.Error("the token was printed")
	}
	ps := pids(t, pidfile)
	if len(ps) != 1 {
		t.Fatalf("tunnels started: %v", ps)
	}
	if !gone(ps[0]) {
		syscall.Kill(ps[0], syscall.SIGKILL)
		t.Fatal("ssh still running after the command")
	}
}

func TestHostsFileDefaultAndLocal(t *testing.T) {
	f := startFar(t)
	useFakeSSH(t, filepath.Join(f.home, "daemon.json"))
	home := clientHome(t, "default: far\nhosts:\n  far:\n    ssh: me@box\n")
	// No flag: the default host is used.
	if r := shepherd(t, home, "status"); r.code != 0 || !strings.Contains(r.out, "daemon   running") {
		t.Fatalf("default host: %d\n%s%s", r.code, r.out, r.errOut)
	}
	// --local ignores it: this machine has no daemon (and none is autostarted without Exe).
	if r := shepherd(t, home, "--local", "status"); r.code != 1 || !strings.Contains(r.errOut, "not running") {
		t.Errorf("--local: %d %s", r.code, r.errOut)
	}
	// An unknown name says what is known.
	if r := shepherd(t, home, "--host", "nope", "status"); r.code != 2 || !strings.Contains(r.errOut, "have: far") {
		t.Errorf("unknown host: %d %s", r.code, r.errOut)
	}
	if r := shepherd(t, home, "host", "list"); r.code != 0 || !strings.Contains(r.out, "far") || !strings.Contains(r.out, "(default)") {
		t.Errorf("host list: %d %s", r.code, r.out)
	}
	if r := shepherd(t, home, "host", "check", "far"); r.code != 0 || !strings.Contains(r.out, "far: daemon") || strings.Contains(r.out, f.token) {
		t.Errorf("host check: %d %s%s", r.code, r.out, r.errOut)
	}
}

func TestMissingDaemonJSONSaysSo(t *testing.T) {
	useFakeSSH(t, filepath.Join(t.TempDir(), "absent.json"))
	r := shepherd(t, clientHome(t, ""), "--host", "ssh://me@box", "status")
	if r.code != 1 || !strings.Contains(r.errOut, "is the daemon running") || !strings.Contains(r.errOut, "ssh me@box shepherd daemon status") {
		t.Errorf("%d %s", r.code, r.errOut)
	}
}

func TestLocalOnlyCommandsRefuseAHost(t *testing.T) {
	f := startFar(t)
	pidfile := useFakeSSH(t, filepath.Join(f.home, "daemon.json"))
	home := clientHome(t, "")
	for _, args := range [][]string{
		{"init", "/tmp"}, {"hook", "install"}, {"daemon", "start"}, {"daemon", "install"}, {"daemon", "uninstall"},
		{"fold", "gc"}, {"agents", "setup", "cursor"}, {"worker", "report"}, {"chat"},
		{"run", "attach", "1"}, {"session", "attach", "x"},
	} {
		r := shepherd(t, home, append([]string{"--host", "ssh://me@box"}, args...)...)
		if r.code != 2 || !strings.Contains(r.errOut, "me@box") || !strings.Contains(r.errOut, "shepherd") {
			t.Errorf("%v: %d %q", args, r.code, r.errOut)
		}
	}
	if ps := pids(t, pidfile); len(ps) != 0 {
		t.Errorf("a refused command opened a tunnel: %v", ps)
	}
}

func workspace(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, _ := paths.Canonical(t.TempDir())
	dir := filepath.Join(root, "app")
	os.MkdirAll(dir, 0o755)
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	exec.Command("git", "-C", dir, "remote", "add", "origin", "git@example.com:t/"+name+".git").Run()
	return root
}

func TestWorkspaceStandsInForTheCurrentDirectory(t *testing.T) {
	f := startFar(t)
	useFakeSSH(t, filepath.Join(f.home, "daemon.json"))
	// Register two workspaces on the far daemon, locally, as its owner would.
	t.Setenv("SHEPHERD_NO_AUTOSTART", "1")
	for _, name := range []string{"alpha", "beta"} {
		root := workspace(t, name)
		if r := shepherd(t, f.home, "init", root, "--name", name, "--yes"); r.code != 0 {
			t.Fatalf("init %s: %s", name, r.errOut)
		}
	}
	home := clientHome(t, "")
	remoteArgs := func(rest ...string) []string { return append([]string{"--host", "ssh://me@box"}, rest...) }

	r := shepherd(t, home, remoteArgs("lane", "list")...)
	if r.code != 1 || !strings.Contains(r.errOut, "--workspace NAME") {
		t.Errorf("no workspace: %d %q", r.code, r.errOut)
	}
	r = shepherd(t, home, remoteArgs("--workspace", "beta", "lane", "list")...)
	if r.code != 0 {
		t.Errorf("--workspace: %d %q", r.code, r.errOut)
	}
	r = shepherd(t, home, remoteArgs("--workspace", "beta", "--repo", "app", "lane", "list")...)
	if r.code != 0 {
		t.Errorf("--repo: %d %q", r.code, r.errOut)
	}
	r = shepherd(t, home, remoteArgs("--workspace", "zzz", "lane", "list")...)
	if r.code != 1 || !strings.Contains(r.errOut, "have: alpha, beta") {
		t.Errorf("unknown workspace: %d %q", r.code, r.errOut)
	}
	// The environment does the same as the flag, and a host's workspace in hosts.yaml
	// does it for a named host.
	t.Setenv("SHEPHERD_WORKSPACE", "alpha")
	if r = shepherd(t, home, remoteArgs("lane", "list")...); r.code != 0 {
		t.Errorf("$SHEPHERD_WORKSPACE: %d %q", r.code, r.errOut)
	}
	t.Setenv("SHEPHERD_WORKSPACE", "")
	named := clientHome(t, "hosts:\n  far:\n    ssh: me@box\n    workspace: beta\n")
	if r = shepherd(t, named, "--host", "far", "lane", "list"); r.code != 0 {
		t.Errorf("hosts.yaml workspace: %d %q", r.code, r.errOut)
	}
	// Without a host there is no stand-in: the flags are an error, not silently ignored.
	r = shepherd(t, home, "--workspace", "beta", "status")
	if r.code != 2 || !strings.Contains(r.errOut, "--host") {
		t.Errorf("local --workspace: %d %q", r.code, r.errOut)
	}
}

func TestMCPOverSSHSharesOneTunnelAndFollowsARestart(t *testing.T) {
	f := startFar(t)
	pidfile := useFakeSSH(t, filepath.Join(f.home, "daemon.json"))
	t.Setenv("SHEPHERD_HOST", "ssh://me@box")

	in, w := io.Pipe()
	pr, out := io.Pipe()
	env := cli.Env{Stdin: in, Stdout: out, Stderr: io.Discard, Layout: paths.Layout{Home: clientHome(t, "")}, Cwd: "/", Client: "cli"}
	done := make(chan int, 1)
	go func() { done <- cli.Run(context.Background(), env, []string{"mcp"}); out.Close() }()
	rd := bufio.NewReader(pr)
	call := func(id int) string {
		t.Helper()
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"shepherd_status","arguments":{}}}`+"\n", id)
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("no reply: %v", err)
		}
		var m struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if m.Result.IsError {
			t.Fatalf("tool error: %s", m.Result.Content[0].Text)
		}
		return m.Result.Content[0].Text
	}
	if txt := call(1); !strings.Contains(txt, "daemon   running") {
		t.Fatalf("status: %s", txt)
	}
	call(2)
	first := pids(t, pidfile)
	if len(first) != 1 {
		t.Fatalf("two tool calls opened %d tunnels, want 1", len(first))
	}

	// The daemon restarts: new server, new port, new token.
	f.ts.CloseClientConnections()
	f.ts.Close()
	nf := startFar(t)
	if err := os.Rename(filepath.Join(nf.home, "daemon.json"), filepath.Join(f.home, "daemon.json")); err != nil {
		t.Fatal(err)
	}
	if txt := call(3); !strings.Contains(txt, "daemon   running") {
		t.Fatalf("after the restart: %s", txt)
	}
	all := pids(t, pidfile)
	if len(all) != 2 {
		t.Fatalf("tunnels: %v", all)
	}
	if !gone(all[0]) {
		t.Error("the tunnel to the old daemon is still running")
	}

	w.Close() // the MCP client goes away
	if code := <-done; code != 0 {
		t.Errorf("mcp exited %d", code)
	}
	if !gone(all[1]) {
		syscall.Kill(all[1], syscall.SIGKILL)
		t.Error("ssh still running after mcp ended")
	}
}
