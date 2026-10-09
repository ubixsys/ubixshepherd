package daemon_test

// Tests of how the clients that run inside agents pick their token. They live here, as
// an external test of the daemon's token rules, and drive the CLI through cli.Run.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/cli"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/paths"
)

// tokenRecorder is a daemon that records the token each request carried.
func tokenRecorder(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

// A home whose daemon.json holds the operator token, at an address that must not be used.
func operatorHome(t *testing.T) paths.Layout {
	l := paths.Layout{Home: t.TempDir()}
	b, _ := json.Marshal(api.Runtime{Addr: "127.0.0.1:1", Token: "OPERATOR"})
	os.WriteFile(l.Runtime(), b, 0o600)
	return l
}

// The worker tools call the daemon with the run's token from the environment, and
// never read daemon.json.
func TestWorkerUsesTheRunToken(t *testing.T) {
	ts, got := tokenRecorder(t)
	t.Setenv(dispatch.EnvToken, "RUNTOKEN")
	t.Setenv(dispatch.EnvURL, ts.URL)
	t.Setenv(dispatch.EnvRun, "7")
	var out, errb bytes.Buffer
	env := cli.Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Layout: operatorHome(t), Cwd: t.TempDir()}
	if code := cli.Run(context.Background(), env, []string{"worker", "report", "--status", "progress", "half way"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	g := got()
	if len(g) == 0 {
		t.Fatal("no call reached the daemon")
	}
	for _, h := range g {
		if h != "Bearer RUNTOKEN" {
			t.Errorf("sent %q, want the run's token", h)
		}
	}
}

// With no run token, the worker tools refuse rather than borrow the operator's.
func TestWorkerWithoutATokenRefuses(t *testing.T) {
	for _, k := range []string{dispatch.EnvToken, dispatch.EnvURL, dispatch.EnvRun} {
		t.Setenv(k, "")
	}
	var errb bytes.Buffer
	env := cli.Env{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &errb, Layout: operatorHome(t), Cwd: t.TempDir()}
	if code := cli.Run(context.Background(), env, []string{"worker", "report", "--status", "progress", "x"}); code == 0 || !strings.Contains(errb.String(), "SHEPHERD_TOKEN") {
		t.Errorf("exit %d: %s", code, errb.String())
	}
}

// shepherd mcp started with a token (the daemon's front desk, or an agent's tools)
// sends that token on every call, not daemon.json's.
func TestMCPUsesTheTokenItWasGiven(t *testing.T) {
	ts, got := tokenRecorder(t)
	t.Setenv(dispatch.EnvToken, "DESKTOKEN")
	t.Setenv(dispatch.EnvURL, ts.URL)
	var out bytes.Buffer
	env := cli.Env{
		Stdin:  strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"decision_list","arguments":{}}}` + "\n"),
		Stdout: &out, Stderr: &bytes.Buffer{}, Layout: operatorHome(t), Cwd: t.TempDir(), Autostart: true, Exe: "/nonexistent",
	}
	if code := cli.Run(context.Background(), env, []string{"mcp"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	g := got()
	if len(g) == 0 {
		t.Fatalf("no call reached the daemon: %s", out.String())
	}
	for _, h := range g {
		if h != "Bearer DESKTOKEN" {
			t.Errorf("sent %q, want the desk's token", h)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "shepherd-mcp-*", "daemon.json"))
	for _, m := range matches {
		if b, _ := os.ReadFile(m); strings.Contains(string(b), "DESKTOKEN") {
			t.Errorf("scoped runtime file left behind: %s", m)
		}
	}
}
