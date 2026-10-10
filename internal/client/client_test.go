package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
)

func TestRedialPicksUpNewAddr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")
	writeRT := func(addr string) {
		t.Helper()
		b, err := json.Marshal(api.Runtime{Addr: addr, Token: "tok-" + addr, PID: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRT("127.0.0.1:7400")
	c, err := FromRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr() != "http://127.0.0.1:7400" {
		t.Fatalf("addr = %s", c.Addr())
	}
	writeRT("127.0.0.1:7501")
	if err := c.Redial(); err != nil {
		t.Fatal(err)
	}
	if c.Addr() != "http://127.0.0.1:7501" {
		t.Fatalf("after redial addr = %s", c.Addr())
	}
	if c.token != "tok-127.0.0.1:7501" {
		t.Fatalf("token not updated: %s", c.token)
	}
}

func TestFeedFillsEventsForOlderDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"id":1,"kind":"pipeline","text":"x","created":"2026-10-08T00:00:00Z"},{"id":2,"kind":"session","text":"y","created":"2026-10-08T00:00:00Z"}],"last":2}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	f, err := c.Feed(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Events) != 2 || f.Events[0] != api.EventPipeline || f.Events[1] != api.EventInfo {
		t.Errorf("events = %v", f.Events)
	}
}

func TestHistoryQueries(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case api.PathHistoryLanes:
			w.Write([]byte(`[{"id":4,"name":"l","repo":"r","state":"closed","outcome":"merged","mr":9,"runs":2,"cost_usd":1.5}]`))
		default:
			w.Write([]byte(`{"runs":[{"id":1,"task":"t"}],"count":1,"cost_usd":0.5,"credits":0,"agents":["claude"],"lanes":[]}`))
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	since := time.Date(2026, 10, 10, 8, 0, 0, 0, time.FixedZone("x", 2*3600))

	lanes, err := c.LaneHistory(context.Background(), 3, "closed", since)
	if err != nil || len(lanes) != 1 || lanes[0].Outcome != api.OutcomeMerged || lanes[0].Runs != 2 || lanes[0].CostUSD != 1.5 {
		t.Fatalf("lanes = %+v, %v", lanes, err)
	}
	runs, err := c.RunHistory(context.Background(), RunHistoryQuery{WorkspaceID: 3, Agent: "claude", LaneID: 4, Since: since})
	if err != nil || runs.Count != 1 || runs.CostUSD != 0.5 || runs.Runs[0].Task != "t" {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if got[0] != "/v1/history/lanes?since=2026-10-10T06%3A00%3A00Z&state=closed&workspace_id=3" {
		t.Errorf("lanes query = %s", got[0])
	}
	if got[1] != "/v1/history/runs?agent=claude&lane_id=4&since=2026-10-10T06%3A00%3A00Z&workspace_id=3" {
		t.Errorf("runs query = %s", got[1])
	}
}
