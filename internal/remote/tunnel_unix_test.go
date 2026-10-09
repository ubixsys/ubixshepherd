//go:build unix

package remote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run a real tunnel through a fake `ssh` found on PATH. The fake is this test
// binary run in a helper role: `ssh dest cat file` prints a fixture, and `ssh -N -L ...`
// forwards a local port like the real one, so everything between the client and the
// process (arguments, readiness, shutdown) is the real code.

const roleEnv = "REMOTE_TEST_ROLE"

func TestHelperProcess(t *testing.T) {
	switch os.Getenv(roleEnv) {
	case "ssh":
		fakeSSH()
	case "client":
		fakeClient()
	}
}

func fakeSSH() {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	var spec string
	tunnel := false
	for i, a := range args {
		if a == "-N" {
			tunnel = true
		}
		if a == "-L" && i+1 < len(args) {
			spec = args[i+1]
		}
	}
	if !tunnel {
		b, err := os.ReadFile(os.Getenv("REMOTE_TEST_FIXTURE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "cat: No such file or directory")
			os.Exit(1)
		}
		os.Stdout.Write(b)
		os.Exit(0)
	}
	if f, err := os.OpenFile(os.Getenv("REMOTE_TEST_PIDFILE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, os.Getpid())
		f.Close()
	}
	parts := strings.SplitN(spec, ":", 3) // bind host, bind port, destination
	l, err := net.Listen("tcp", parts[0]+":"+parts[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bind", parts[0]+":"+parts[1]+": Address already in use")
		os.Exit(255)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(1)
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("tcp", parts[2])
			if err != nil {
				return
			}
			defer u.Close()
			go io.Copy(u, c)
			io.Copy(c, u)
		}()
	}
}

// fakeClient is the shepherd side: it dials and then ends in the way the test asked.
func fakeClient() {
	s := &SSH{Target: Target{Dest: "me@box", Home: "~/.shepherd"}, ReadyTimeout: 5 * time.Second}
	mode := os.Getenv("REMOTE_TEST_MODE")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ep, err := s.Dial(ctx)
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(3)
	}
	if mode != "leak" {
		defer s.Close() // what cli.Run does; runs on a panic too
	}
	fmt.Println("UP", ep.Base)
	if mode == "signal" {
		<-ctx.Done()
		return
	}
	bufio.NewReader(os.Stdin).ReadString('\n') // the test says when to end
	if mode == "panic" {
		panic("boom")
	}
}

// setupFake puts a fake ssh on PATH and returns the env a helper needs.
func setupFake(t *testing.T, remoteAddr string) (env []string, pidfile string) {
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
	fixture := filepath.Join(dir, "daemon.json")
	body := `{"addr":"` + remoteAddr + `","pid":1,"token":"` + secretToken + `"}`
	if err := os.WriteFile(fixture, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pidfile = filepath.Join(dir, "pids")
	env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REMOTE_TEST_FIXTURE="+fixture, "REMOTE_TEST_PIDFILE="+pidfile)
	return env, pidfile
}

func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func readPids(t *testing.T, pidfile string) []int {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("fake ssh never started a tunnel: %v", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(f)
		pids = append(pids, n)
	}
	return pids
}

func waitGone(t *testing.T, pid int) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		if !alive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// runClient starts the helper client and returns once it reports the tunnel is up.
func runClient(t *testing.T, env []string, mode string) (cmd *exec.Cmd, base string, release func()) {
	t.Helper()
	cmd = exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(env, roleEnv+"=client", "REMOTE_TEST_MODE="+mode)
	cmd.Stderr = io.Discard
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	out := bufio.NewReader(pipe)
	line, err := out.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "UP ") {
		t.Fatalf("client said %q, %v", line, err)
	}
	return cmd, strings.TrimSpace(strings.TrimPrefix(line, "UP ")), func() { io.WriteString(in, "\n") }
}

func roundTrip(t *testing.T, base string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("tunnel does not accept: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("through the tunnel: %q, %v", buf, err)
	}
}

func TestRealTunnelCarriesTraffic(t *testing.T) {
	env, _ := setupFake(t, echoServer(t))
	t.Setenv("PATH", envValue(env, "PATH"))
	t.Setenv("REMOTE_TEST_FIXTURE", envValue(env, "REMOTE_TEST_FIXTURE"))
	t.Setenv("REMOTE_TEST_PIDFILE", envValue(env, "REMOTE_TEST_PIDFILE"))
	s := &SSH{Target: Target{Dest: "me@box", Home: "~/.shepherd"}, ReadyTimeout: 5 * time.Second}
	ep, err := s.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ep.Base)
	if ep.Token() != secretToken {
		t.Error("token not read")
	}
	s.Close()
}

func envValue(env []string, k string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], k+"=") {
			return strings.TrimPrefix(env[i], k+"=")
		}
	}
	return ""
}

func TestRealTunnelGoneAfterClientExits(t *testing.T) {
	for _, mode := range []string{"normal", "panic", "signal"} {
		t.Run(mode, func(t *testing.T) {
			env, pidfile := setupFake(t, echoServer(t))
			cmd, base, release := runClient(t, env, mode)
			pids := readPids(t, pidfile)
			if len(pids) != 1 || !alive(pids[0]) {
				t.Fatalf("pids = %v", pids)
			}
			roundTrip(t, base)
			if mode == "signal" {
				cmd.Process.Signal(syscall.SIGTERM)
			} else {
				release()
			}
			cmd.Wait()
			if !waitGone(t, pids[0]) {
				syscall.Kill(pids[0], syscall.SIGKILL)
				t.Fatalf("ssh pid %d still running after the client exited (%s)", pids[0], mode)
			}
		})
	}
}

// TestLeakCheckCatchesALeak proves the check above can fail: a client that never closes
// leaves its ssh behind.
func TestLeakCheckCatchesALeak(t *testing.T) {
	env, pidfile := setupFake(t, echoServer(t))
	cmd, _, release := runClient(t, env, "leak")
	pids := readPids(t, pidfile)
	release()
	cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	if !alive(pids[0]) {
		t.Fatal("the leak check cannot fail: ssh went away without Close")
	}
	syscall.Kill(pids[0], syscall.SIGKILL)
}
