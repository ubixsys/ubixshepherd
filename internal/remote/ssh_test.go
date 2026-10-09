package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretToken = "tok-0123456789abcdef-SECRET"

// fakeRunner stands in for ssh: it records command lines and answers from fields.
type fakeRunner struct {
	mu       sync.Mutex
	calls    [][]string
	out      []byte
	errb     []byte
	outErr   error
	startErr error
	// tunnelExit, when set, makes a started tunnel exit at once with this stderr.
	tunnelExit error
	tunnelErrb string
	procs      []*fakeProc
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.out, f.errb, f.outErr
}

func (f *fakeRunner) Start(name string, args ...string) (Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.startErr != nil {
		return nil, f.startErr
	}
	p := &fakeProc{done: make(chan struct{}), stderr: f.tunnelErrb}
	if f.tunnelExit != nil {
		p.err = f.tunnelExit
		close(p.done)
	} else {
		// Emulate the forward's local end: accept and drop connections.
		for i, a := range args {
			if a == "-L" {
				parts := strings.SplitN(args[i+1], ":", 3)
				l, err := net.Listen("tcp", parts[0]+":"+parts[1])
				if err != nil {
					p.err = errors.New("address already in use")
					p.stderr = "bind [127.0.0.1]:" + parts[1] + ": Address already in use"
					close(p.done)
					break
				}
				p.l = l
				go func() {
					for {
						c, err := l.Accept()
						if err != nil {
							return
						}
						c.Close()
					}
				}()
			}
		}
	}
	f.procs = append(f.procs, p)
	return p, nil
}

type fakeProc struct {
	done    chan struct{}
	err     error
	stderr  string
	l       net.Listener
	stopped bool
	once    sync.Once
}

func (p *fakeProc) Wait() error    { <-p.done; return p.err }
func (p *fakeProc) Stderr() string { return p.stderr }
func (p *fakeProc) Stop() error {
	p.once.Do(func() {
		p.stopped = true
		if p.l != nil {
			p.l.Close()
		}
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	})
	return nil
}

func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error: %v", err)
	}
	return err
}

func newSSH(f *fakeRunner, t Target) *SSH {
	return &SSH{Target: t, Runner: f, ReadyTimeout: 3 * time.Second}
}

func TestCommandLines(t *testing.T) {
	s := &SSH{Target: Target{Dest: "me@box", Home: "~/.shepherd"}}
	want := "-o BatchMode=yes -o ConnectTimeout=10 -- me@box cat ~/'.shepherd/daemon.json'"
	if got := strings.Join(s.ReadArgs(), " "); got != want {
		t.Errorf("read args:\n got %s\nwant %s", got, want)
	}
	want = "-o BatchMode=yes -o ConnectTimeout=10 -N -L 127.0.0.1:50123:127.0.0.1:7400 " +
		"-o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -- me@box"
	if got := strings.Join(s.TunnelArgs(50123, "127.0.0.1:7400"), " "); got != want {
		t.Errorf("tunnel args:\n got %s\nwant %s", got, want)
	}
	for _, a := range append(s.ReadArgs(), s.TunnelArgs(1, "127.0.0.1:2")...) {
		if strings.Contains(a, "StrictHostKeyChecking") || strings.Contains(a, "UserKnownHostsFile") {
			t.Errorf("host key checking touched: %s", a)
		}
	}

	s = &SSH{Target: Target{Dest: "box", Port: 2222, Home: "/srv/it's here", Control: true}, ControlDir: "/h/ssh"}
	got := strings.Join(s.ReadArgs(), " ")
	for _, frag := range []string{"-p 2222", "ControlMaster=auto", "ControlPath=/h/ssh/%C", "ControlPersist=60s",
		`cat '/srv/it'\''s here/daemon.json'`} {
		if !strings.Contains(got, frag) {
			t.Errorf("read args lack %q: %s", frag, got)
		}
	}
	// Without Control set there is no sharing, whatever the directory.
	s = &SSH{Target: Target{Dest: "box"}, ControlDir: "/h/ssh"}
	if strings.Contains(strings.Join(s.ReadArgs(), " "), "Control") {
		t.Error("control options without Control set")
	}
}

func TestRemotePath(t *testing.T) {
	for in, want := range map[string]string{
		"~/.shepherd":      "~/'.shepherd/daemon.json'",
		"~/.shepherd/":     "~/'.shepherd/daemon.json'",
		"/var/lib/s":       "'/var/lib/s/daemon.json'",
		"":                 "~/'.shepherd/daemon.json'",
		"/a b/$(rm -rf x)": "'/a b/$(rm -rf x)/daemon.json'",
	} {
		if got := remotePath(in, "daemon.json"); got != want {
			t.Errorf("remotePath(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseRuntime(t *testing.T) {
	rt, err := ParseRuntime([]byte(`{"addr":"127.0.0.1:7400","pid":4,"token":"` + secretToken + `","version":"x"}`))
	if err != nil || rt.Addr != "127.0.0.1:7400" || rt.Token != secretToken {
		t.Fatalf("good file: %+v, %v", rt, err)
	}
	for name, in := range map[string]string{
		"empty":      "  \n",
		"truncated":  `{"addr":"127.0.0.1:7400","token":"` + secretToken,
		"not json":   "token=" + secretToken,
		"wrong type": `{"addr":7400,"token":"` + secretToken + `"}`,
		"no address": `{"token":"` + secretToken + `"}`,
		"no token":   `{"addr":"127.0.0.1:7400"}`,
		"html":       "<html>" + secretToken + "</html>",
		"array":      `["` + secretToken + `"]`,
	} {
		_, err := ParseRuntime([]byte(in))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error leaks the token: %v", name, err)
		}
	}
}

func TestForwardTarget(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7400": "127.0.0.1:7400",
		"localhost:7400": "127.0.0.1:7400",
		"0.0.0.0:7400":   "127.0.0.1:7400",
		"[::]:7400":      "127.0.0.1:7400",
		"[::1]:7400":     "[::1]:7400",
		"127.0.0.2:1":    "127.0.0.2:1",
	} {
		if got, err := forwardTarget(in); err != nil || got != want {
			t.Errorf("forwardTarget(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"10.0.0.5:7400", "example.com:80", "7400", "127.0.0.1:0", "127.0.0.1:x", "127.0.0.1:70000", ""} {
		if got, err := forwardTarget(in); err == nil {
			t.Errorf("forwardTarget(%q) = %q, want an error", in, got)
		}
	}
}

func TestClassifyReadFailures(t *testing.T) {
	tgt := Target{Name: "build", Dest: "me@build.example", Home: "~/.shepherd"}
	for _, tc := range []struct {
		name   string
		code   int
		stderr string
		want   []string
		not    []string
	}{
		{"unreachable", 255, "ssh: connect to host build.example port 22: Connection refused", []string{"cannot reach build"}, nil},
		{"resolve", 255, "ssh: Could not resolve hostname build.example: Name or service not known", []string{"cannot reach build", "resolve"}, nil},
		{"auth", 255, "me@build.example: Permission denied (publickey).", []string{"could not authenticate", "key", "ssh-agent"}, []string{"StrictHostKeyChecking"}},
		{"unknown host key", 255, "Host key verification failed.", []string{"does not trust build", "known_hosts", "ssh me@build.example"}, []string{"StrictHostKeyChecking", "-o"}},
		{"changed host key", 255, "@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@\nHost key verification failed.", []string{"host key for build has changed", "intercepting"}, []string{"StrictHostKeyChecking", "UserKnownHostsFile"}},
		{"missing daemon.json", 1, "cat: /home/me/.shepherd/daemon.json: No such file or directory", []string{"no daemon.json on build", "is the daemon running", "ssh me@build.example shepherd daemon status"}, nil},
		{"other remote failure", 2, "boom", []string{"exit 2", "boom"}, nil},
	} {
		f := &fakeRunner{outErr: exitErr(t, tc.code), errb: []byte(tc.stderr)}
		_, err := newSSH(f, tgt).Dial(context.Background())
		if err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q lacks %q", tc.name, err, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(err.Error(), n) {
				t.Errorf("%s: %q contains %q", tc.name, err, n)
			}
		}
	}
	// ssh itself missing.
	f := &fakeRunner{outErr: &exec.Error{Name: "ssh", Err: exec.ErrNotFound}}
	if _, err := newSSH(f, tgt).Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("no ssh: %v", err)
	}
}

func goodRuntime() []byte {
	return []byte(`{"addr":"127.0.0.1:7400","pid":9,"token":"` + secretToken + `"}`)
}

func TestDialOpensTunnel(t *testing.T) {
	f := &fakeRunner{out: goodRuntime()}
	s := newSSH(f, Target{Dest: "me@box", Home: "~/.shepherd"})
	ep, err := s.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ep.Token() != secretToken {
		t.Error("token not carried")
	}
	port := strings.TrimPrefix(ep.Base, "http://127.0.0.1:")
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	tun := strings.Join(f.calls[1], " ")
	if !strings.Contains(tun, "-N -L 127.0.0.1:"+port+":127.0.0.1:7400") || !strings.HasSuffix(tun, "-- me@box") {
		t.Errorf("tunnel call: %s", tun)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("tunnel not listening: %v", err)
	}
	c.Close()

	// A redial reads the file again, ends the old tunnel and opens another.
	f.out = []byte(`{"addr":"127.0.0.1:7555","token":"tok-new"}`)
	ep2, err := s.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ep2.Token() != "tok-new" || ep2.Base == ep.Base && !f.procs[0].stopped {
		t.Errorf("redial: %v", ep2)
	}
	if !f.procs[0].stopped {
		t.Error("old tunnel left running")
	}
	if !strings.Contains(strings.Join(f.calls[len(f.calls)-1], " "), ":127.0.0.1:7555 ") {
		t.Errorf("new tunnel does not go to the new address: %v", f.calls[len(f.calls)-1])
	}
	s.Close()
	s.Close() // twice is fine
	if !f.procs[len(f.procs)-1].stopped {
		t.Error("Close left the tunnel")
	}
}

func TestDialRefusesNonLoopbackDaemon(t *testing.T) {
	f := &fakeRunner{out: []byte(`{"addr":"10.1.2.3:7400","token":"` + secretToken + `"}`)}
	_, err := newSSH(f, Target{Dest: "box"}).Dial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("err = %v", err)
	}
	if len(f.procs) != 0 {
		t.Error("a tunnel was started")
	}
}

func TestTunnelFailures(t *testing.T) {
	tgt := Target{Name: "build", Dest: "box"}
	f := &fakeRunner{out: goodRuntime(), tunnelExit: exitErr(t, 255),
		tunnelErrb: "bind [127.0.0.1]:5: Address already in use\nchannel_setup_fwd_listener_tcpip: cannot listen to port: 5\nCould not request local forwarding."}
	_, err := newSSH(f, tgt).Dial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not bind") {
		t.Fatalf("bind: %v", err)
	}
	if n := len(f.procs); n != 3 {
		t.Errorf("bind failures retried %d times, want 3", n)
	}

	f = &fakeRunner{out: goodRuntime(), tunnelExit: exitErr(t, 255), tunnelErrb: "Warning: remote port forwarding failed; administratively prohibited"}
	if _, err := newSSH(f, tgt).Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "does not allow port forwarding") {
		t.Errorf("prohibited: %v", err)
	}

	f = &fakeRunner{out: goodRuntime(), tunnelExit: exitErr(t, 255), tunnelErrb: "Connection timed out during banner exchange"}
	if _, err := newSSH(f, tgt).Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot reach build") {
		t.Errorf("tunnel unreachable: %v", err)
	}

	f = &fakeRunner{out: goodRuntime(), startErr: errors.New("fork failed")}
	if _, err := newSSH(f, tgt).Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "start ssh") {
		t.Errorf("start: %v", err)
	}
}

func TestTokenNeverInErrorsOrFormatting(t *testing.T) {
	tgt := Target{Name: "build", Dest: "me@box"}
	leaky := secretToken + " Authorization: Bearer " + secretToken
	var errs []error
	// Every failure path with the token in the remote's stderr (an ssh that printed part
	// of the file before it failed, or a server that echoes).
	for _, code := range []int{1, 2, 255} {
		f := &fakeRunner{out: goodRuntime(), outErr: exitErr(t, code), errb: []byte(leaky + "\nPermission denied")}
		_, err := newSSH(f, tgt).Dial(context.Background())
		errs = append(errs, err)
		f = &fakeRunner{out: goodRuntime(), outErr: exitErr(t, code), errb: []byte(leaky)}
		_, err = newSSH(f, tgt).Dial(context.Background())
		errs = append(errs, err)
	}
	for _, exit := range []int{1, 255} {
		f := &fakeRunner{out: goodRuntime(), tunnelExit: exitErr(t, exit), tunnelErrb: leaky}
		_, err := newSSH(f, tgt).Dial(context.Background())
		errs = append(errs, err)
		f = &fakeRunner{out: goodRuntime(), tunnelExit: exitErr(t, exit), tunnelErrb: "Address already in use " + leaky}
		_, err = newSSH(f, tgt).Dial(context.Background())
		errs = append(errs, err)
	}
	// And a ready timeout, which quotes stderr too.
	s := newSSH(&fakeRunner{out: goodRuntime()}, tgt)
	s.ReadyTimeout = 50 * time.Millisecond
	p := &fakeProc{done: make(chan struct{}), stderr: leaky}
	errs = append(errs, s.waitReady(context.Background(), p, 1, secretToken))
	p.Stop()
	// Malformed daemon.json holding the token.
	f := &fakeRunner{out: []byte("{" + secretToken)}
	_, err := newSSH(f, tgt).Dial(context.Background())
	errs = append(errs, err)

	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: no error", i)
			continue
		}
		if strings.Contains(err.Error(), "0123456789abcdef") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("case %d leaks the token: %v", i, err)
		}
	}
	ep := NewEndpoint("http://127.0.0.1:1", secretToken)
	for _, s := range []string{fmt.Sprint(ep), fmt.Sprintf("%v %+v %#v %s", ep, ep, ep, ep), fmt.Sprint(&ep)} {
		if strings.Contains(s, "SECRET") {
			t.Errorf("endpoint formatting leaks the token: %s", s)
		}
	}
}

func TestDialHonoursContext(t *testing.T) {
	f := &fakeRunner{out: goodRuntime()}
	s := newSSH(f, Target{Dest: "box"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The fake tunnel listens at once, so the readiness loop may win the race with the
	// cancelled context; either way no tunnel is left behind on an error.
	if _, err := s.Dial(ctx); err != nil {
		for _, p := range f.procs {
			if !p.stopped {
				t.Error("tunnel left running after a cancelled dial")
			}
		}
	}
	s.Close()
}
