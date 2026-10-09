package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// daemonStub answers the calls remote tests make, and remembers the token it was sent.
type daemonStub struct {
	*httptest.Server
	mu     sync.Mutex
	tokens []string
	paths  []string
	want   string // the token it accepts; empty accepts any
}

func newStub(t *testing.T, want string) *daemonStub {
	t.Helper()
	d := &daemonStub{want: want}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.tokens = append(d.tokens, r.Header.Get("Authorization"))
		d.paths = append(d.paths, r.URL.RequestURI())
		d.mu.Unlock()
		if d.want != "" && r.Header.Get("Authorization") != "Bearer "+d.want {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"missing or wrong token"}`))
			return
		}
		switch r.URL.Path {
		case api.PathStatus:
			json.NewEncoder(w).Encode(api.Status{PID: 7})
		case api.PathWorkspaces:
			json.NewEncoder(w).Encode([]api.WorkspaceDetail{
				{Workspace: store.Workspace{ID: 1, Name: "main", Path: "/srv/main"}, Repos: []store.Repo{{ID: 10, Name: "app", Path: "/srv/main/app"}}},
				{Workspace: store.Workspace{ID: 2, Name: "other", Path: "/srv/other"}},
			})
		case api.PathResolve:
			p := r.URL.Query().Get("path")
			json.NewEncoder(w).Encode(api.Resolution{Path: p, Workspace: &store.Workspace{Name: "resolved:" + p}})
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func TestRemoteRedialUsesTheResolver(t *testing.T) {
	old := newStub(t, "tok-old")
	next := newStub(t, "tok-new")
	calls := 0
	c := NewRemote("build", old.URL, "tok-old", func() (string, string, error) {
		calls++
		return next.URL, "tok-new", nil
	})
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The daemon restarted: the old address is gone.
	old.Close()
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v, want it to match ErrNoDaemon so the chat reconnects", err)
	}
	if !strings.Contains(err.Error(), "build") || !strings.Contains(err.Error(), "ssh tunnel") {
		t.Errorf("message does not say where: %v", err)
	}
	if err := c.Redial(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || c.Addr() != next.URL {
		t.Fatalf("calls = %d, addr = %s", calls, c.Addr())
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("after redial: %v", err)
	}
	if got := next.tokens[len(next.tokens)-1]; got != "Bearer tok-new" {
		t.Errorf("sent %q to the new daemon", got)
	}
}

func TestRemoteRedialFailureKeepsTheOldEndpoint(t *testing.T) {
	d := newStub(t, "")
	c := NewRemote("build", d.URL, "tok", func() (string, string, error) { return "", "", errors.New("ssh could not authenticate") })
	if err := c.Redial(); err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("err = %v", err)
	}
	if c.Addr() != d.URL {
		t.Error("a failed redial replaced the endpoint")
	}
}

func TestRefusedTokenIsAConnectionError(t *testing.T) {
	d := newStub(t, "right")
	for _, host := range []string{"", "build"} {
		var c *Client
		if host == "" {
			c = New(d.URL, "wrong")
		} else {
			c = NewRemote(host, d.URL, "wrong", nil)
		}
		_, err := c.Status(context.Background())
		if !errors.Is(err, ErrNoDaemon) || !strings.Contains(err.Error(), "refused the access token") {
			t.Errorf("host %q: err = %v", host, err)
		}
		if host != "" && !strings.Contains(err.Error(), host) {
			t.Errorf("remote message does not name the host: %v", err)
		}
		if strings.Contains(err.Error(), "wrong") {
			t.Errorf("error holds the token: %v", err)
		}
	}
}

func TestLocalConnectionErrorKeepsItsMessage(t *testing.T) {
	d := newStub(t, "")
	d.Close()
	_, err := New(d.URL, "t").Status(context.Background())
	if !errors.Is(err, ErrNoDaemon) || !strings.Contains(err.Error(), "shepherd daemon start") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveScope(t *testing.T) {
	d := newStub(t, "")
	ctx := context.Background()

	c := New(d.URL, "t")
	res, err := c.Resolve(ctx, RemoteCwd)
	if err != nil || res.Workspace != nil || len(d.paths) != 0 {
		t.Fatalf("no scope: %+v, %v, calls %v", res, err, d.paths)
	}
	// A real path passes through untouched, scope or not.
	c.SetScope("main", "")
	if res, _ := c.Resolve(ctx, "/srv/main/app/lane"); res.Workspace.Name != "resolved:/srv/main/app/lane" {
		t.Errorf("path was rewritten: %+v", res)
	}

	res, err = c.Resolve(ctx, RemoteCwd)
	if err != nil || res.Workspace.Name != "resolved:/srv/main" {
		t.Fatalf("workspace: %+v, %v", res, err)
	}
	c.SetScope("main", "app")
	if res, err = c.Resolve(ctx, RemoteCwd); err != nil || res.Workspace.Name != "resolved:/srv/main/app" {
		t.Fatalf("repo: %+v, %v", res, err)
	}
	c.SetScope("main", "nope")
	if _, err = c.Resolve(ctx, RemoteCwd); err == nil || !strings.Contains(err.Error(), "have: app") {
		t.Errorf("unknown repo: %v", err)
	}
	c.SetScope("zzz", "")
	if _, err = c.Resolve(ctx, RemoteCwd); err == nil || !strings.Contains(err.Error(), "have: main, other") {
		t.Errorf("unknown workspace: %v", err)
	}
	c.SetScope("", "app")
	if _, err = c.Resolve(ctx, RemoteCwd); err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("repo without workspace: %v", err)
	}
}

func TestShipLaneCopyKeepsTheHostLabel(t *testing.T) {
	d := newStub(t, "")
	d.Close()
	c := NewRemote("build", d.URL, "t", nil)
	if _, err := c.ShipLane(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "build") {
		t.Errorf("err = %v", err)
	}
}
