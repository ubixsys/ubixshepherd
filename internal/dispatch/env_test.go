package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCreds struct{ revoked []int64 }

func (c *fakeCreds) MintWorker(id int64) (string, error) { return "worker-tok-for-run", nil }
func (c *fakeCreds) Revoke(id int64)                     { c.revoked = append(c.revoked, id) }
func (c *fakeCreds) URL() string                         { return "http://127.0.0.1:4242" }

// An agent reaches the daemon as its run: SHEPHERD_URL and SHEPHERD_TOKEN are the
// run's worker token, whatever the daemon's own environment held, and the operator's
// token is nowhere in what the agent receives.
func TestAgentEnvHasOnlyTheWorkerToken(t *testing.T) {
	const operator = "operator-secret-token"
	f := newFixture(t, "quick")
	dump := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("ENVDUMP", dump)
	// What a daemon started from an agent's shell, or by hand, could have inherited.
	t.Setenv(EnvToken, operator)
	t.Setenv(EnvURL, "http://stale.invalid:1")
	t.Setenv(EnvRun, "999")
	t.Setenv(EnvClient, "desk")
	agent := filepath.Join(t.TempDir(), "env-agent")
	os.WriteFile(agent, []byte("#!/bin/sh\ncat >/dev/null\nenv > \"$ENVDUMP\"\n"), 0o755)
	f.runner.lookPath = func(string) (string, error) { return agent, nil }
	creds := &fakeCreds{}
	f.runner.SetCredentials(creds)

	run, err := f.runner.Start(context.Background(), StartRequest{LaneID: f.lane.ID, Agent: "claude", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, run.ID)
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, operator) {
		t.Errorf("the operator token reached the agent:\n%s", got)
	}
	for _, want := range []string{"SHEPHERD_TOKEN=worker-tok-for-run\n", "SHEPHERD_URL=http://127.0.0.1:4242\n", "SHEPHERD_RUN=1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("agent env lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SHEPHERD_CLIENT=") {
		t.Errorf("the client name was inherited:\n%s", got)
	}
	f.runner.Wait()
	if len(creds.revoked) != 1 || creds.revoked[0] != run.ID {
		t.Errorf("revoked = %v, want the run's token revoked once it ended", creds.revoked)
	}
}
