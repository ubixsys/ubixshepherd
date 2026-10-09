package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/paths"
)

func TestScrubRunEnv(t *testing.T) {
	cases := []struct {
		name     string
		in, want []string
	}{
		{"empty", nil, []string{}},
		{"ordinary kept",
			[]string{"PATH=/bin", "HOME=/h", "SSH_AUTH_SOCK=/s", "SHEPHERD_HOME=/sh", "SHEPHERD_LOG_LEVEL=debug"},
			[]string{"PATH=/bin", "HOME=/h", "SSH_AUTH_SOCK=/s", "SHEPHERD_HOME=/sh", "SHEPHERD_LOG_LEVEL=debug"}},
		{"run identity",
			[]string{"PATH=/bin", "SHEPHERD_RUN=7", "SHEPHERD_LANE=x", "SHEPHERD_CLIENT=desk", "SHEPHERD_URL=http://u", "SHEPHERD_TOKEN=t"},
			[]string{"PATH=/bin"}},
		{"claude session",
			[]string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_SSE_PORT=9", "CLAUDE_HOME=/c", "A=b"},
			[]string{"CLAUDE_HOME=/c", "A=b"}},
		{"push block of two",
			[]string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_0=v0",
				"GIT_CONFIG_KEY_1=k1", "GIT_CONFIG_VALUE_1=v1", "GIT_CONFIG_KEY_2=mine", "PATH=/bin"},
			[]string{"GIT_CONFIG_KEY_2=mine", "PATH=/bin"}},
		{"parameters",
			[]string{"GIT_CONFIG_PARAMETERS='a.b=c'", "PATH=/bin"}, []string{"PATH=/bin"}},
		{"user's own git settings kept",
			[]string{"GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_SYSTEM=/s", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh -i k",
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a", "GIT_CONFIG_VALUE_0=b"},
			[]string{"GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_SYSTEM=/s", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh -i k"}},
		{"count zero or junk",
			[]string{"GIT_CONFIG_COUNT=zero", "GIT_CONFIG_KEY_0=a"},
			[]string{"GIT_CONFIG_KEY_0=a"}},
		{"value containing equals",
			[]string{"SHEPHERD_TOKEN=a=b", "X=y=z"}, []string{"X=y=z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scrubRunEnv(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDaemonCommandsRefuseFromRun(t *testing.T) {
	t.Setenv("SHEPHERD_RUN", "7")
	for _, sub := range []string{"start", "restart", "install"} {
		l := paths.Layout{Home: t.TempDir()}
		errOut := &bytes.Buffer{}
		env := Env{Stdout: io.Discard, Stderr: errOut, Layout: l, Cwd: l.Home}
		if code := Run(context.Background(), env, []string{"daemon", sub}); code != 1 {
			t.Errorf("daemon %s: exit %d, want 1", sub, code)
		}
		for _, want := range []string{"agent run", "--force-from-run", "stops that agent's own run"} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("daemon %s: %q is missing %q", sub, errOut, want)
			}
		}
		if _, err := os.Stat(l.Console()); err == nil {
			t.Errorf("daemon %s: started a daemon anyway", sub)
		}
	}
}

func TestForceFromRunWarnsAndProceeds(t *testing.T) {
	t.Setenv("SHEPHERD_RUN", "7")
	l := paths.Layout{Home: t.TempDir()}
	if err := os.WriteFile(l.Config(), []byte(badConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	errOut := &bytes.Buffer{}
	env := Env{Stdout: io.Discard, Stderr: errOut, Layout: l, Cwd: l.Home}
	if code := Run(context.Background(), env, []string{"daemon", "start", "--force-from-run"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	// Past the guard, the start reaches the config check.
	if !strings.Contains(errOut.String(), "warning:") || !strings.Contains(errOut.String(), "is not human or agent") {
		t.Errorf("want a warning, then the start to proceed: %q", errOut)
	}
}

func TestNoGuardOutsideRun(t *testing.T) {
	t.Setenv("SHEPHERD_RUN", "")
	if err := refuseFromRun(Env{Stderr: io.Discard}, "start", false); err != nil {
		t.Errorf("refused outside a run: %v", err)
	}
}

// The suite may itself run inside an agent run, whose SHEPHERD_RUN would trip the
// guard in every daemon test; tests that want it set it with t.Setenv.
func init() { os.Unsetenv("SHEPHERD_RUN") }

func TestUnsetRunEnv(t *testing.T) {
	for k, v := range map[string]string{
		"SHEPHERD_RUN": "7", "SHEPHERD_TOKEN": "t", "CLAUDECODE": "1", "CLAUDE_CODE_X": "y",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "k", "GIT_CONFIG_VALUE_0": "v",
		"GIT_CONFIG_PARAMETERS": "'a=b'", "GIT_CONFIG_GLOBAL": "/g", "KEEP_ME": "1",
	} {
		t.Setenv(k, v)
	}
	unsetRunEnv()
	for _, k := range []string{"SHEPHERD_RUN", "SHEPHERD_TOKEN", "CLAUDECODE", "CLAUDE_CODE_X",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_PARAMETERS"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still set", k)
		}
	}
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "KEEP_ME"} {
		if os.Getenv(k) == "" {
			t.Errorf("%s was removed", k)
		}
	}
}

func TestDaemonRunDropsRunEnv(t *testing.T) {
	t.Setenv("SHEPHERD_TOKEN", "t")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	// A bad config makes daemonRun return right after the scrub, without serving.
	l := paths.Layout{Home: t.TempDir()}
	if err := os.WriteFile(l.Config(), []byte(badConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	env := Env{Stdout: io.Discard, Stderr: io.Discard, Layout: l, Cwd: l.Home}
	if err := daemonRun(context.Background(), env); err == nil {
		t.Fatal("want the config error")
	}
	for _, k := range []string{"SHEPHERD_TOKEN", "GIT_CONFIG_COUNT"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s survived daemonRun", k)
		}
	}
}
