package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/config"
	"unicode/utf8"
)

func mcpExchange(t *testing.T, env Env, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := serveMCP(context.Background(), env, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, mcpTools(), mcpInstructions, 0); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, m)
	}
	return resps
}

func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"].(bool)
}

func TestMCPHandshakeAndList(t *testing.T) {
	h := newHarness(t, "", false)
	resps := mcpExchange(t, h.env,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
		`not json`,
	)
	if len(resps) != 4 {
		t.Fatalf("got %d responses: %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["instructions"] == "" {
		t.Errorf("initialize = %v", init)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"shepherd_status", "lane_open", "lane_close", "lane_ship", "lane_list", "fold_gc", "shepherd_where"} {
		if !names[n] {
			t.Errorf("tool %s missing", n)
		}
	}
	if resps[2]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", resps[2])
	}
	if resps[3]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Errorf("parse error: %v", resps[3])
	}
}

func TestMCPLaneTools(t *testing.T) {
	h := newHarness(t, "", false)
	root, _ := laneWorkspace(t)
	if code := h.run("init", root, "--yes"); code != 0 {
		t.Fatalf("init: %s", h.err)
	}
	h.env.Cwd = root
	resps := mcpExchange(t, h.env,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lane_open","arguments":{"repo":"app","name":"feat/mcp","scope":["src/**"]}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lane_open","arguments":{"repo":"app","name":"feat/other","scope":["src/x/**"]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lane_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lane_open","arguments":{"repo":"app","name":"bad","scope":"src/**"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"lane_ship","arguments":{"repo":"app","name":"feat/mcp"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"lane_close","arguments":{"repo":"app","name":"feat/mcp"}}}`,
	)
	text, isErr := toolText(t, resps[0])
	if isErr || !strings.Contains(text, filepath.Join("app-worktrees", "feat-mcp")) {
		t.Errorf("lane_open: %v %s", isErr, text)
	}
	text, isErr = toolText(t, resps[1])
	if !isErr || !strings.Contains(text, "overlaps lane feat/mcp") {
		t.Errorf("overlapping lane_open: %v %s", isErr, text)
	}
	text, _ = toolText(t, resps[2])
	if !strings.Contains(text, "feat/mcp") {
		t.Errorf("lane_list: %s", text)
	}
	if _, isErr = toolText(t, resps[3]); !isErr {
		t.Error("scope as a string accepted")
	}
	// app has not opted in to Shepherd pushing: lane_ship refuses and says why.
	text, isErr = toolText(t, resps[4])
	if !isErr || !strings.Contains(text, "app has not opted in to Shepherd pushing") || !strings.Contains(text, "push lane feat/mcp yourself") {
		t.Errorf("lane_ship: %v %s", isErr, text)
	}
	text, isErr = toolText(t, resps[5])
	if isErr || !strings.Contains(text, "closed lane feat/mcp") {
		t.Errorf("lane_close: %v %s", isErr, text)
	}
}

func TestCapOutput(t *testing.T) {
	if got := capOutput("short", 500, "x"); got != "short" {
		t.Errorf("short output changed: %q", got)
	}
	long := "START " + strings.Repeat("é middle ", 2000) + " END"
	got := capOutput(long, 1000, "Read the log file.")
	if n := len([]rune(got)); n > 1000 {
		t.Errorf("capped to %d characters, want at most 1000", n)
	}
	if !strings.HasPrefix(got, "START ") || !strings.HasSuffix(got, " END") ||
		!strings.Contains(got, "characters cut: the front desk sees at most 1000") || !strings.Contains(got, "Read the log file.") {
		t.Errorf("capped = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Error("a character was split")
	}
}

// The desk's server caps what a tool returns and says how to read more; a server with
// no cap of its own (an external client's) returns it whole.
func TestMCPMaxOutput(t *testing.T) {
	h := newHarness(t, "", false)
	help := mcpTool{Name: "help", InputSchema: obj(map[string]any{}), more: "Ask for less.",
		args: func(map[string]any) ([]string, error) { return []string{"help"}, nil }}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"help","arguments":{}}}` + "\n"
	serve := func(max int) string {
		var out bytes.Buffer
		if err := serveMCP(context.Background(), h.env, strings.NewReader(call), &out, []mcpTool{help}, "", max); err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		json.Unmarshal(out.Bytes(), &resp)
		text, _ := toolText(t, resp)
		return text
	}
	whole := serve(0)
	if len(whole) < 1000 || strings.Contains(whole, "characters cut") {
		t.Fatalf("uncapped = %d characters: %q", len(whole), whole)
	}
	capped := serve(config.MinToolOutputChars)
	if len([]rune(capped)) > config.MinToolOutputChars || !strings.Contains(capped, "Ask for less.") || !strings.HasPrefix(capped, whole[:50]) ||
		!strings.HasSuffix(capped, whole[len(whole)-50:]) {
		t.Errorf("capped = %q", capped)
	}
}
