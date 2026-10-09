package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/paths"
)

const badConfig = "repos:\n  x:\n    autonomy:\n      merge: sometimes\n"

func TestStartRefusesBadConfig(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	if err := os.WriteFile(l.Config(), []byte(badConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	errOut := &bytes.Buffer{}
	env := Env{Stdout: io.Discard, Stderr: errOut, Layout: l, Cwd: l.Home}
	if code := Run(context.Background(), env, []string{"daemon", "start"}); code != 1 {
		t.Fatalf("daemon start: %d", code)
	}
	if !strings.Contains(errOut.String(), `"sometimes" is not human or agent`) {
		t.Errorf("the config error is not shown: %q", errOut)
	}
	if _, err := os.Stat(l.Console()); err == nil {
		t.Error("a daemon was started anyway")
	}
}

func TestRestartKeepsDaemonOnBadConfig(t *testing.T) {
	h := newHarness(t, "", false)
	if err := os.WriteFile(h.env.Layout.Config(), []byte(badConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("daemon", "restart"); code != 1 || !strings.Contains(h.err.String(), "is not human or agent") {
		t.Fatalf("daemon restart: %d %q", code, h.err)
	}
	if h.run("daemon", "status"); !strings.Contains(h.out.String(), "running, pid") {
		t.Errorf("restart stopped the daemon: %q", h.out)
	}
}

func TestConsoleSince(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	if got := consoleSince(l, 0); got != "" {
		t.Errorf("no console file: %q", got)
	}
	old := "an earlier start\n"
	var b strings.Builder
	b.WriteString(old)
	for i := 0; i < 12; i++ {
		b.WriteString("line " + string(rune('a'+i)) + "\n")
	}
	if err := os.WriteFile(l.Console(), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := consoleSince(l, int64(len(old)))
	if strings.Contains(got, "earlier") || strings.Contains(got, "line b") || !strings.HasPrefix(got, "  line c\n") || !strings.HasSuffix(got, "  line l") {
		t.Errorf("consoleSince = %q", got)
	}
	if got := consoleSince(l, int64(b.Len())); got != "" {
		t.Errorf("nothing new: %q", got)
	}
}
