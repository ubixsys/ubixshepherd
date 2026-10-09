package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoint is where a client reaches a daemon once a Transport has set it up.
type Endpoint struct {
	// Base is the URL to call, for example http://127.0.0.1:50123.
	Base  string
	token string
}

// NewEndpoint builds an Endpoint.
func NewEndpoint(base, token string) Endpoint { return Endpoint{Base: base, token: token} }

// Token is the daemon's access token. Send it only in the Authorization header; never
// print or log it. Endpoint formats without it, so a stray %v is safe.
func (e Endpoint) Token() string { return e.token }

func (e Endpoint) String() string   { return "Endpoint{" + e.Base + ", token redacted}" }
func (e Endpoint) GoString() string { return e.String() }

// Transport sets up a way to a daemon and tears it down. SSH is the only one today; a
// direct TLS transport would implement the same two calls.
type Transport interface {
	// Dial resolves where the daemon is now and opens the way to it, closing any earlier
	// way first, so it serves both the first connection and a redial.
	Dial(ctx context.Context) (Endpoint, error)
	// Close ends the way to the daemon. It is safe to call more than once.
	Close() error
}

// Runner starts processes, so tests can supply their own ssh.
type Runner interface {
	// Output runs a command to completion and returns stdout and stderr.
	Output(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
	// Start starts a long-running command, stderr captured for the caller.
	Start(name string, args ...string) (Proc, error)
}

// Proc is a started process.
type Proc interface {
	// Wait returns when the process exits, with its exit error.
	Wait() error
	// Stop asks the process to end, then kills it if it will not.
	Stop() error
	// Stderr is what the process has written to stderr so far.
	Stderr() string
}

// ExecRunner runs real processes.
type ExecRunner struct{}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = nil
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (ExecRunner) Start(name string, args ...string) (Proc, error) {
	cmd := exec.Command(name, args...)
	p := &execProc{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &p.stderr
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p, nil
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type execProc struct {
	cmd    *exec.Cmd
	stderr lockedBuf
	done   chan struct{}
	err    error
}

func (p *execProc) Wait() error { <-p.done; return p.err }

func (p *execProc) Stderr() string { return p.stderr.String() }

func (p *execProc) Stop() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	// ssh ends cleanly on SIGTERM; Windows has no such signal, so it is killed.
	if p.cmd.Process.Signal(os.Interrupt) != nil {
		p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// SSH reaches a daemon through an ssh tunnel. It uses the person's own ssh and its
// configuration (keys, agent, jump hosts, aliases); host-key checking is never turned off.
type SSH struct {
	Target Target
	// Bin is the ssh program (default "ssh").
	Bin string
	// Runner starts it (default ExecRunner).
	Runner Runner
	// ControlDir is where shared-connection sockets live; used only if Target.Control.
	ControlDir string
	// ConnectTimeout bounds each ssh connection attempt; ReadyTimeout bounds waiting for the
	// local end of a tunnel to accept connections.
	ConnectTimeout time.Duration
	ReadyTimeout   time.Duration

	mu     sync.Mutex
	tunnel Proc
	// forward is the -L spec held in a shared connection's master, which outlives the
	// ssh process that asked for it and has to be cancelled by name.
	forward string
}

// shared says tunnels go through a shared connection (ControlMaster).
func (s *SSH) shared() bool { return s.Target.Control && s.ControlDir != "" }

func (s *SSH) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	return "ssh"
}

func (s *SSH) runner() Runner {
	if s.Runner != nil {
		return s.Runner
	}
	return ExecRunner{}
}

func (s *SSH) connectTimeout() time.Duration {
	if s.ConnectTimeout > 0 {
		return s.ConnectTimeout
	}
	return 10 * time.Second
}

func (s *SSH) readyTimeout() time.Duration {
	if s.ReadyTimeout > 0 {
		return s.ReadyTimeout
	}
	return 15 * time.Second
}

// baseArgs are the options every ssh call here carries. BatchMode means ssh never asks a
// question on the terminal (a password, a host-key prompt): a failure is reported instead.
func (s *SSH) baseArgs() []string {
	secs := int(s.connectTimeout() / time.Second)
	if secs < 1 {
		secs = 1
	}
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=" + strconv.Itoa(secs)}
	if s.Target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(s.Target.Port))
	}
	if s.Target.Control && s.ControlDir != "" {
		// %C is a hash of the connection, so the socket path stays short.
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(s.ControlDir, "%C"),
			"-o", "ControlPersist=60s")
	}
	return args
}

// ReadArgs is the ssh command line that prints the daemon's runtime file.
func (s *SSH) ReadArgs() []string {
	args := append(s.baseArgs(), "--", s.Target.Dest, "cat "+remotePath(s.Target.Home, "daemon.json"))
	return args
}

// TunnelArgs is the ssh command line that forwards 127.0.0.1:localPort to remoteAddr on
// the daemon's machine.
func (s *SSH) TunnelArgs(localPort int, remoteAddr string) []string {
	args := append(s.baseArgs(),
		"-N",
		"-L", "127.0.0.1:"+strconv.Itoa(localPort)+":"+remoteAddr,
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"--", s.Target.Dest)
	return args
}

// remotePath makes a path for the remote shell: a leading ~/ is left for it to expand and
// the rest is quoted, so a space or a quote in the home directory cannot break the command.
func remotePath(home, file string) string {
	if home == "" {
		home = DefaultHome
	}
	p := strings.TrimSuffix(home, "/") + "/" + file
	if strings.HasPrefix(p, "~/") {
		return "~/" + shellQuote(p[2:])
	}
	return shellQuote(p)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Dial reads the daemon's runtime file over ssh and opens a tunnel to the address in it.
func (s *SSH) Dial(ctx context.Context) (Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	if s.Target.Control && s.ControlDir != "" {
		if err := os.MkdirAll(s.ControlDir, 0o700); err != nil {
			return Endpoint{}, fmt.Errorf("make %s for shared ssh connections: %w", s.ControlDir, err)
		}
		os.Chmod(s.ControlDir, 0o700)
	}
	rt, err := s.readRuntime(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	fwd, err := forwardTarget(rt.Addr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%s: %w", s.Target.Label(), err)
	}
	var last error
	for try := 0; try < 3; try++ {
		port, err := freePort()
		if err != nil {
			return Endpoint{}, fmt.Errorf("find a local port for the tunnel: %w", err)
		}
		proc, err := s.runner().Start(s.bin(), s.TunnelArgs(port, fwd)...)
		if err != nil {
			return Endpoint{}, fmt.Errorf("start ssh: %w", err)
		}
		last = s.waitReady(ctx, proc, port, rt.Token)
		if last == nil {
			s.tunnel = proc
			if s.shared() {
				s.forward = "127.0.0.1:" + strconv.Itoa(port) + ":" + fwd
			}
			return Endpoint{Base: "http://127.0.0.1:" + strconv.Itoa(port), token: rt.Token}, nil
		}
		proc.Stop()
		s.cancelForward("127.0.0.1:" + strconv.Itoa(port) + ":" + fwd)
		var be *bindError
		if !errors.As(last, &be) {
			break // only a local port taken in the meantime is worth another try
		}
	}
	return Endpoint{}, last
}

// Close stops the tunnel.
func (s *SSH) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	return nil
}

func (s *SSH) stopLocked() {
	if s.tunnel != nil {
		s.tunnel.Stop()
		s.tunnel = nil
	}
	if s.forward != "" {
		s.cancelForward(s.forward)
		s.forward = ""
	}
}

// cancelForward asks a shared connection's master to drop a forward. Through a master,
// ssh -N -L may return as soon as the master has the forward, and killing a client does
// not end it, so without this the local port would stay open until the master exits.
func (s *SSH) cancelForward(spec string) {
	if !s.shared() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append(s.baseArgs(), "-O", "cancel", "-L", spec, "--", s.Target.Dest)
	s.runner().Output(ctx, s.bin(), args...)
}

// RuntimeInfo is the part of the daemon's runtime file this layer reads.
type RuntimeInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func (s *SSH) readRuntime(ctx context.Context) (RuntimeInfo, error) {
	out, errb, err := s.runner().Output(ctx, s.bin(), s.ReadArgs()...)
	if err != nil {
		return RuntimeInfo{}, s.classify(err, string(errb), tokenGuess(out))
	}
	rt, err := ParseRuntime(out)
	if err != nil {
		return RuntimeInfo{}, fmt.Errorf("%s:%s/daemon.json: %w",
			s.Target.Label(), s.Target.Home, err)
	}
	return rt, nil
}

// waitReady waits until the local end of the tunnel accepts connections, or ssh exits.
func (s *SSH) waitReady(ctx context.Context, p Proc, port int, token string) error {
	exited := make(chan error, 1)
	go func() { exited <- p.Wait() }()
	deadline := time.NewTimer(s.readyTimeout())
	defer deadline.Stop()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		select {
		case err := <-exited:
			if err == nil && s.shared() {
				// A client of a shared connection hands its forward to the master and
				// returns; the forward is up if the port answers.
				exited = nil
				continue
			}
			if err == nil {
				err = errors.New("ssh exited")
			}
			return s.classifyTunnel(err, p.Stderr(), token)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("the ssh tunnel to %s did not come up in %s%s", s.Target.Label(), s.readyTimeout(), detail(p.Stderr(), token))
		case <-tick.C:
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				c.Close()
				// Something answers on the port; make sure it is ssh and not a stranger who
				// took the port after we freed it.
				select {
				case err := <-exited:
					return s.classifyTunnel(fmt.Errorf("ssh exited: %v", err), p.Stderr(), token)
				default:
					return nil
				}
			}
		}
	}
}

// bindError means the local end of the tunnel could not be bound.
type bindError struct{ msg string }

func (e *bindError) Error() string { return e.msg }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// forwardTarget turns the daemon's listen address into the destination of the forward,
// as the daemon's machine sees it. The daemon listens on loopback; an address that is
// not is refused, since forwarding to it would make the remote ssh server open a
// connection wherever a file said.
func forwardTarget(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("daemon.json has an address that is not host:port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("daemon.json has a bad port")
	}
	ip := net.ParseIP(host)
	switch {
	case host == "localhost":
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified():
		host = "127.0.0.1" // listening on all interfaces, which includes loopback
	case ip != nil && ip.IsLoopback():
	default:
		return "", fmt.Errorf("daemon.json says the daemon listens on %s, which is not loopback; not tunnelling to it", host)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port, nil
}

var _ io.Writer = (*lockedBuf)(nil)
