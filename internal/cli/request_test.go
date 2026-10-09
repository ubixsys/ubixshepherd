package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/store"
)

func TestRequestCloseAndNotes(t *testing.T) {
	h := newHarness(t, "", false)
	root, app := laneWorkspace(t)
	if code := h.run("init", root, "--yes"); code != 0 {
		t.Fatalf("init: %s", h.err)
	}
	h.env.Cwd = app
	if code := h.run("lane", "open", "feat/x", "--scope", "src/**"); code != 0 {
		t.Fatalf("lane open: %s", h.err)
	}
	ctx := context.Background()
	st := h.srv.Store
	run, err := st.CreateRun(ctx, store.Run{LaneID: 1, Agent: "claude", Prompt: "x", State: store.RunSucceeded, Log: "l"})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(note string) store.Request {
		q, err := st.CreateRequest(ctx, store.Request{FromRun: run.ID, Kind: "question", Message: "which port?",
			State: store.RequestNeedsRouting, Note: note, Depth: 1})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	one, two := ask("no lane named docs is open"), ask("")

	if code := h.run("request", "list"); code != 0 || !strings.Contains(h.out.String(), "note: no lane named docs is open") {
		t.Errorf("list shows the note: %d\n%s%s", code, h.out, h.err)
	}
	if code := h.run("request", "close", fmt.Sprint(one.ID), "--why", "asked by hand"); code != 0 ||
		!strings.Contains(h.out.String(), "is closed (closed: asked by hand)") {
		t.Errorf("close: %d\n%s%s", code, h.out, h.err)
	}
	if got, _ := st.Request(ctx, one.ID); got.State != store.RequestClosed || got.Note != "closed: asked by hand" {
		t.Errorf("closed request: %+v", got)
	}
	if code := h.run("request", "close", fmt.Sprint(one.ID)); code == 0 || !strings.Contains(h.err.String(), "closed already") {
		t.Errorf("closing twice: %d %s", code, h.err)
	}
	if code := h.run("request", "list"); code != 0 || strings.Contains(h.out.String(), fmt.Sprintf("request %d ", one.ID)) {
		t.Errorf("a closed request is still listed:\n%s", h.out)
	}
	if code := h.run("request", "close"); code == 0 {
		t.Error("close without an id accepted")
	}

	h.env.Cwd = root
	resps := mcpExchange(t, h.env,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"request_close","arguments":{"id":%d,"why":"stale"}}}`, two.ID),
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"request_close","arguments":{"id":"x"}}}`,
	)
	if text, isErr := toolText(t, resps[0]); isErr || !strings.Contains(text, "closed: stale") {
		t.Errorf("request_close: %v %s", isErr, text)
	}
	if _, isErr := toolText(t, resps[1]); !isErr {
		t.Error("request_close with a string id accepted")
	}
}

// ask_shepherd offers kind person, and passes it through to the worker command.
func TestAskShepherdOffersPerson(t *testing.T) {
	for _, tl := range workerTools() {
		if tl.Name != "ask_shepherd" {
			continue
		}
		kind := tl.InputSchema["properties"].(map[string]any)["kind"].(map[string]any)
		if !slices.Contains(kind["enum"].([]string), "person") {
			t.Errorf("kinds: %v", kind["enum"])
		}
		args, err := tl.args(map[string]any{"kind": "person", "message": "ship it?"})
		if err != nil || !slices.Equal(args, []string{"worker", "ask-shepherd", "ship it?", "--kind", "person"}) {
			t.Errorf("args: %v %v", args, err)
		}
		return
	}
	t.Fatal("no ask_shepherd tool")
}
