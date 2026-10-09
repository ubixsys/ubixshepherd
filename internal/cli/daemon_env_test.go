package cli

import (
	"reflect"
	"testing"
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
