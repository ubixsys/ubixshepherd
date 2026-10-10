package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// rawStream opens a stream and sends each line it reads, comments included.
func rawStream(t *testing.T, url, token string, header map[string]string) (<-chan string, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, resp.StatusCode
	}
	ch := make(chan string, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch, resp.StatusCode
}

// feedEvents reads n feed events from a stream.
func feedEvents(t *testing.T, ch <-chan string, n int) []api.FeedEvent {
	t.Helper()
	var out []api.FeedEvent
	var id, kind string
	timeout := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended after %d of %d events", len(out), n)
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var e api.FeedEvent
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
				if e.Event != kind || id == "" || e.ID == 0 {
					t.Errorf("event %s %s with %+v", id, kind, e)
				}
				out = append(out, e)
			}
		case <-timeout:
			t.Fatalf("got %d of %d events", len(out), n)
		}
	}
	return out
}

func TestFeedStream(t *testing.T) {
	s, ts := newServer(t)
	ctx := context.Background()
	s.Store.AddFeed(ctx, store.FeedLaneOpened, "lane a opened", 1)
	s.Store.AddFeed(ctx, store.FeedRunPassed, "run 1 passed", 1)
	s.Store.AddFeed(ctx, store.FeedRequestStuck, "request 2 needs routing", 2)

	ch, _ := rawStream(t, ts.URL+api.PathFeedStream+"?after=1", s.Token, nil)
	got := feedEvents(t, ch, 2)
	if got[0].ID != 2 || got[0].Event != api.EventRunPassed || got[1].Event != api.EventRequestAttention {
		t.Errorf("after 1 = %+v", got)
	}
	// New items arrive without polling.
	s.Store.AddFeed(ctx, store.FeedMR, "!7 merged", 7)
	if got := feedEvents(t, ch, 1); got[0].ID != 4 || got[0].Text != "!7 merged" {
		t.Errorf("live = %+v", got)
	}
	// Last-Event-ID resumes; with neither it starts after the newest.
	ch2, _ := rawStream(t, ts.URL+api.PathFeedStream, s.Token, map[string]string{"Last-Event-ID": "3"})
	if got := feedEvents(t, ch2, 1); got[0].ID != 4 {
		t.Errorf("Last-Event-ID 3 = %+v", got)
	}
	ch3, _ := rawStream(t, ts.URL+api.PathFeedStream, s.Token, nil)
	s.Store.AddFeed(ctx, store.FeedTag, "v1.2.3 reserved", 0)
	if got := feedEvents(t, ch3, 1); got[0].ID != 5 {
		t.Errorf("from latest = %+v", got)
	}
	if _, code := rawStream(t, ts.URL+api.PathFeedStream+"?after=x", s.Token, nil); code != http.StatusBadRequest {
		t.Errorf("bad after: %d", code)
	}
	// Closing the daemon ends the streams.
	s.Close()
	for _, c := range []<-chan string{ch, ch2, ch3} {
		deadline := time.After(5 * time.Second)
	drain:
		for {
			select {
			case _, ok := <-c:
				if !ok {
					break drain
				}
			case <-deadline:
				t.Fatal("a stream outlived the daemon")
			}
		}
	}
}

func TestFeedStreamHeartbeatAndLimit(t *testing.T) {
	beat, max := heartbeat, maxFeedStreams
	heartbeat, maxFeedStreams = 50*time.Millisecond, 1
	t.Cleanup(func() { heartbeat, maxFeedStreams = beat, max })
	s, ts := newServer(t)
	ch, _ := rawStream(t, ts.URL+api.PathFeedStream, s.Token, nil)
	timeout := time.After(5 * time.Second)
	for pinged := false; !pinged; {
		select {
		case line := <-ch:
			pinged = line == ": ping"
		case <-timeout:
			t.Fatal("no heartbeat")
		}
	}
	if _, code := rawStream(t, ts.URL+api.PathFeedStream, s.Token, nil); code != http.StatusServiceUnavailable {
		t.Errorf("a stream past the limit: %d, want 503", code)
	}
	// A scoped token: the desk may follow the feed, a worker may not.
	tok, _ := s.MintWorker(1)
	if _, code := rawStream(t, ts.URL+api.PathFeedStream, tok, nil); code != http.StatusForbidden {
		t.Errorf("worker: %d, want 403", code)
	}
}
