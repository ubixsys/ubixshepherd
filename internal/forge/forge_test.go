package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := map[string]Remote{
		"git@gitlab.example.com:group/app.git":        {"gitlab.example.com", "group/app"},
		"ssh://git@gitlab.example.com:2222/a/b/c.git": {"gitlab.example.com", "a/b/c"},
		"https://gitlab.example.com/group/app":        {"gitlab.example.com", "group/app"},
		"https://u:tok@github.com/o/r.git/":           {"github.com", "o/r"},
	}
	for in, want := range cases {
		got, err := ParseRemote(in)
		if err != nil || got != want {
			t.Errorf("ParseRemote(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/local/path", "file"} {
		if _, err := ParseRemote(bad); err == nil {
			t.Errorf("ParseRemote(%q) accepted", bad)
		}
	}
	if _, err := For("git@github.com:o/r.git"); err == nil {
		t.Error("GitHub accepted as a lane forge")
	}
	if f, err := For("git@gitlab.example.com:g/a.git"); err != nil || f.Name() != "gitlab" {
		t.Errorf("For gitlab = %v, %v", f, err)
	}
}

func TestGitLabMRForBranch(t *testing.T) {
	var calls []string
	g := &GitLab{Host: "gl.example.com", Project: "group/app", Run: func(_ context.Context, args ...string) ([]byte, error) {
		path := args[len(args)-1]
		calls = append(calls, path)
		switch {
		case strings.Contains(path, "source_branch=feat%2Fx"):
			return []byte(`[{"iid":7,"state":"opened"}]`), nil
		case strings.HasSuffix(path, "merge_requests/7"):
			return []byte(`{"iid":7,"state":"merged","sha":"abc","merge_commit_sha":"m1","squash_commit_sha":"s1","web_url":"u","head_pipeline":{"id":9,"status":"failed","sha":"abc","web_url":"p"}}`), nil
		case strings.Contains(path, "source_branch="):
			return []byte(`[]`), nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}}
	mr, err := g.MRForBranch(context.Background(), "feat/x")
	if err != nil || mr == nil || mr.IID != 7 || mr.MergeSHA != "m1" || mr.Pipeline.ID != 9 || mr.Pipeline.Status != "failed" {
		t.Fatalf("mr = %+v, %v", mr, err)
	}
	if !strings.HasPrefix(calls[0], "projects/group%2Fapp/") {
		t.Errorf("project not escaped: %s", calls[0])
	}
	if mr, err := g.MRForBranch(context.Background(), "none"); mr != nil || err != nil {
		t.Errorf("no MR = %+v, %v", mr, err)
	}
}

func TestGitLabRefPipeline(t *testing.T) {
	g := &GitLab{Host: "gl.example.com", Project: "group/app", Run: func(_ context.Context, args ...string) ([]byte, error) {
		switch path := args[len(args)-1]; {
		case strings.Contains(path, "pipelines?ref=v1.2.0&"):
			return []byte(`[{"id":11,"status":"success","sha":"abc","web_url":"p"}]`), nil
		case strings.Contains(path, "pipelines?ref="):
			return []byte(`[]`), nil
		default:
			return nil, fmt.Errorf("unexpected %s", path)
		}
	}}
	p, err := g.RefPipeline(context.Background(), "v1.2.0")
	if err != nil || p == nil || p.ID != 11 || p.Status != "success" {
		t.Fatalf("pipeline = %+v, %v", p, err)
	}
	if p, err := g.RefPipeline(context.Background(), "v9.9.9"); p != nil || err != nil {
		t.Errorf("no pipeline = %+v, %v", p, err)
	}
}

func TestCleanLog(t *testing.T) {
	raw := "2026-10-02T01:08:32.769621Z 01O \x1b[32;1m$ make check\x1b[0;m\n" +
		"2026-10-02T01:08:32.769643Z 01O --- FAIL: TestX (0.00s)\n" +
		"2026-10-02T01:08:32.771208Z 00O section_end:1790903312:step_script\x1b[0K\n" +
		"\n2026-10-02T01:08:33.990996Z 00O \x1b[31;1mERROR: Job failed: exit status 1\x1b[0;m\n"
	got := CleanLog(raw, 10)
	want := "$ make check\n--- FAIL: TestX (0.00s)\nERROR: Job failed: exit status 1"
	if got != want {
		t.Errorf("CleanLog =\n%q\nwant\n%q", got, want)
	}
	if got := CleanLog(raw, 1); got != "ERROR: Job failed: exit status 1" {
		t.Errorf("last line = %q", got)
	}
}

func TestUnreachable(t *testing.T) {
	for err, want := range map[error]bool{
		nil:                      false,
		context.DeadlineExceeded: true,
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}:      true,
		errors.New("glab api --hostname h p: dial tcp: lookup h: no such host"):          true,
		errors.New("glab api p: Get \"https://h/api\": net/http: TLS handshake timeout"): true,
		errors.New("glab api p: 502 Bad Gateway (HTTP 502)"):                             true,
		errors.New("glab api p: 503 Service Unavailable (HTTP 503)"):                     true,
		errors.New("glab api p: 401 Unauthorized (HTTP 401)"):                            false,
		errors.New("glab api p: 404 Not Found (HTTP 404)"):                               false,
		errors.New("glab api p: 404 timeout of a project (HTTP 404)"):                    false,
		errors.New("glab api p: invalid character 'x'"):                                  false,
	} {
		if got := Unreachable(err); got != want {
			t.Errorf("Unreachable(%v) = %v", err, got)
		}
	}
}
