package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
)

func fastOpts() FollowOptions {
	return FollowOptions{MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
}

func TestDeskCalls(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case api.PathDeskTurn:
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"workspace_id":2,"turn":7,"ahead":1}`))
		case api.PathDeskHistory:
			w.Write([]byte(`{"events":[{"seq":5,"kind":"user","text":"hi"}],"more":true}`))
		case api.PathDeskInterrupt:
			w.Write([]byte(`{"interrupted":true}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	ctx := context.Background()
	acc, err := c.DeskTurn(ctx, 2, "hello")
	if err != nil || acc.Turn != 7 || acc.Ahead != 1 {
		t.Fatalf("turn: %+v %v", acc, err)
	}
	h, err := c.DeskHistory(ctx, 0, 9, 50)
	if err != nil || !h.More || len(h.Events) != 1 || h.Events[0].Seq != 5 {
		t.Fatalf("history: %+v %v", h, err)
	}
	if ok, err := c.DeskInterrupt(ctx, 0); err != nil || !ok {
		t.Fatalf("interrupt: %v %v", ok, err)
	}
	if err := c.DeskNew(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeskStatus(ctx, 3); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/desk/turn",
		"GET /v1/desk/history?before=9&limit=50",
		"POST /v1/desk/interrupt",
		"POST /v1/desk/new",
		"GET /v1/desk/status?workspace_id=3",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("requests:\n got %v\nwant %v", got, want)
	}
}

// A stream that drops resumes after the last stored event, partial pieces are marked
// and never move the resume point, and a clean end is reopened too.
func TestFollowDeskResumes(t *testing.T) {
	var mu sync.Mutex
	var resumes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == api.PathDeskHistory {
			w.Write([]byte(`{"events":[{"seq":10,"kind":"user"}]}`))
			return
		}
		mu.Lock()
		n := len(resumes)
		resumes = append(resumes, r.Header.Get("Last-Event-ID")+"/"+r.URL.Query().Get("after"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		switch n {
		case 0:
			fmt.Fprint(w, ": ping\n\n")
			fmt.Fprint(w, "id: 11\nevent: assistant\ndata: {\"seq\":11,\"kind\":\"assistant\",\"text\":\"a\"}\n\n")
			fmt.Fprint(w, "event: partial\ndata: {\"kind\":\"assistant\",\"text\":\"b\"}\n\n")
		case 1:
			fmt.Fprint(w, "id: 12\nevent: turn_end\ndata: {\"seq\":12,\"kind\":\"turn_end\",\"text\":\"done\"}\n\n")
		default:
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []string
	opt := fastOpts()
	err := c.FollowDesk(ctx, 0, -1, opt, func(it DeskItem) error {
		seen = append(seen, fmt.Sprintf("%d:%s:%v", it.Seq, it.Kind, it.Partial))
		if it.Seq == 12 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got, want := strings.Join(seen, " "), "11:assistant:false 0:assistant:true 12:turn_end:false"; got != want {
		t.Errorf("seen %q, want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(resumes) < 2 || resumes[0] != "10/10" || resumes[1] != "11/11" {
		t.Errorf("resumes = %v", resumes)
	}
}

func TestFollowFeedStopsOnRefusalAndHandlerError(t *testing.T) {
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not yours"}`))
	}))
	defer forbidden.Close()
	err := New(forbidden.URL, "t").FollowFeed(context.Background(), 5, fastOpts(), func(api.FeedEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not yours") {
		t.Errorf("refusal: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 3\nevent: info\ndata: {\"id\":3,\"kind\":\"session\",\"text\":\"x\"}\n\n")
	}))
	defer srv.Close()
	stop := errors.New("enough")
	err = New(srv.URL, "t").FollowFeed(context.Background(), 2, fastOpts(), func(e api.FeedEvent) error {
		if e.ID != 3 || e.Event != "info" {
			t.Errorf("event = %+v", e)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("handler error: %v", err)
	}
}

// A daemon that restarted answers 401 to the old token: the client is redialed and the
// stream continues on the new endpoint.
func TestFollowFeedRedialsAfterRestart(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\nevent: info\ndata: {\"id\":1,\"kind\":\"session\"}\n\n")
	}))
	defer good.Close()
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer old.Close()
	c := NewRemote("box", old.URL, "old", func() (string, string, error) { return good.URL, "new", nil })
	var reasons []error
	opt := fastOpts()
	opt.OnReconnect = func(err error) { reasons = append(reasons, err) }
	stop := errors.New("done")
	err := c.FollowFeed(context.Background(), 0, opt, func(api.FeedEvent) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
	if len(reasons) != 1 || !errors.Is(reasons[0], ErrNoDaemon) {
		t.Errorf("reasons = %v", reasons)
	}
}

func TestFollowTakesASilentStreamForDead(t *testing.T) {
	var mu sync.Mutex
	opens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		opens++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	opt := fastOpts()
	opt.Idle = 30 * time.Millisecond
	New(srv.URL, "t").FollowFeed(ctx, 1, opt, func(api.FeedEvent) error { return nil })
	mu.Lock()
	defer mu.Unlock()
	if opens < 2 {
		t.Errorf("silent stream was not reopened: %d opens", opens)
	}
}

func TestLaneConversationCalls(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		w.Write([]byte(`{"items":[{"seq":3,"kind":"tool","run":1,"tool":"Bash","output":"all of it"}],"cursor":3,"more":false,"running":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	th, err := c.LaneConversation(context.Background(), 4, 2, 50)
	if err != nil || th.Cursor != 3 || !th.Running || th.Items[0].Tool != "Bash" {
		t.Fatalf("thread = %+v, %v", th, err)
	}
	it, err := c.LaneConversationItem(context.Background(), 4, 3)
	if err != nil || it.Output != "all of it" {
		t.Fatalf("item = %+v, %v", it, err)
	}
	want := []string{"GET /v1/lanes/4/conversation?after=2&limit=50", "GET /v1/lanes/4/conversation?expand=3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v", got)
	}
}
