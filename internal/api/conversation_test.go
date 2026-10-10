package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/store/sqlite"
)

func TestLaneConversation(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	ws, err := st.SaveWorkspace(ctx, store.Workspace{Name: "w", Path: root}, []store.Repo{{Name: "r", Path: filepath.Join(root, "r")}})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := st.Repos(ctx, ws.ID)
	lane, err := st.CreateLane(ctx, store.Lane{RepoID: repos[0].ID, Name: "l", Branch: "feat/l", Base: "dev", State: store.LaneClosed, Created: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, store.Run{LaneID: lane.ID, Agent: "claude", Session: "s-1", State: store.RunSucceeded, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	projects := t.TempDir()
	os.MkdirAll(filepath.Join(projects, "p"), 0o755)
	os.WriteFile(filepath.Join(projects, "p", "s-1.jsonl"), []byte(
		`{"type":"user","uuid":"1","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello glpat-AbCdEfGhIjKlMnOpQrStUv"}}`+"\n"), 0o644)

	h := LaneConversation(st, convo.Source{ClaudeProjects: projects})
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathLanes+"/{id}/conversation", h)
	get := func(path string) (int, convo.Thread, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var th convo.Thread
		json.Unmarshal(rec.Body.Bytes(), &th)
		return rec.Code, th, rec.Body.String()
	}
	code, th, body := get(PathLaneConversation(lane.ID))
	if code != 200 || len(th.Items) != 2 || th.Items[1].Kind != convo.KindUser {
		t.Fatalf("code %d body %s", code, body)
	}
	if strings.Contains(body, "glpat-") {
		t.Errorf("unredacted: %s", body)
	}
	if code, th, _ = get(PathLaneConversation(lane.ID) + "?after=2"); code != 200 || len(th.Items) != 1 {
		t.Errorf("after=2: %d %+v", code, th.Items)
	}
	if code, _, _ = get(PathLaneConversation(999)); code != 404 {
		t.Errorf("unknown lane: %d", code)
	}
	if code, _, _ = get(PathLanes + "/x/conversation"); code != 400 {
		t.Errorf("bad id: %d", code)
	}
}
