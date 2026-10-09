package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/redact"
)

// Environment an agent Shepherd starts gets, so the tools it runs reach the daemon as
// that run and nothing more.
const (
	// EnvRun is the run's id.
	EnvRun = "SHEPHERD_RUN"
	// EnvToken is the run's worker token: allowed only what the worker tools need, for
	// this run, and revoked when the run ends or the daemon restarts.
	EnvToken = "SHEPHERD_TOKEN"
	// EnvURL is the daemon's API, http://host:port.
	EnvURL = "SHEPHERD_URL"
)

// Credentials mint and revoke the token a run calls the daemon with. The daemon
// provides them; a runner without them starts agents with no token.
type Credentials interface {
	// MintWorker returns a new worker token for the run.
	MintWorker(runID int64) (string, error)
	// Revoke ends every token minted for the run.
	Revoke(runID int64)
	// URL is the daemon's API base, http://host:port.
	URL() string
}

// SetCredentials gives the runner what mints its runs' tokens. The daemon calls it once
// it listens; runs started before then get no token.
func (r *Runner) SetCredentials(c Credentials) { r.creds.Store(&c) }

func (r *Runner) credentials() Credentials {
	if c := r.creds.Load(); c != nil {
		return *c
	}
	return nil
}

// RunFile is where, in a lane worktree's private git directory, a run's credentials
// are also written, for agent CLIs that start MCP servers without passing their
// environment on. It is outside the work tree, so it is never committed, and removed
// when the run ends.
const RunFile = "shepherd-run.json"

// RunCredentials is what RunFile holds.
type RunCredentials struct {
	Run   int64  `json:"run"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

// runFilePath is RunFile's absolute path for a worktree.
func runFilePath(ctx context.Context, worktree string) (string, error) {
	return git.Run(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-path", RunFile)
}

// ReadRunFile reads the credentials of the run going in the worktree that holds dir.
func ReadRunFile(ctx context.Context, dir string) (RunCredentials, error) {
	var rc RunCredentials
	p, err := runFilePath(ctx, dir)
	if err != nil {
		return rc, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return rc, err
	}
	if err := json.Unmarshal(b, &rc); err != nil {
		return rc, err
	}
	if rc.Run == 0 || rc.Token == "" || rc.URL == "" {
		return rc, errors.New(RunFile + " is incomplete")
	}
	return rc, nil
}

// grant mints a run's token and returns the environment the agent gets, and how to undo
// it. With no credentials it returns only the run's id.
func (r *Runner) grant(ctx context.Context, runID int64, worktree string) (env []string, token string, undo func(), err error) {
	env = []string{fmt.Sprintf("%s=%d", EnvRun, runID)}
	c := r.credentials()
	if c == nil {
		return env, "", func() {}, nil
	}
	token, err = c.MintWorker(runID)
	if err != nil {
		return nil, "", nil, err
	}
	file := ""
	if p, err := runFilePath(ctx, worktree); err == nil {
		b, _ := json.Marshal(RunCredentials{Run: runID, URL: c.URL(), Token: token})
		if err := writePrivate(p, b); err == nil {
			file = p
		} else {
			r.Log.Warn("write run credentials file", "run", runID, "err", err)
		}
	}
	undo = func() {
		c.Revoke(runID)
		if file != "" {
			os.Remove(file)
		}
	}
	return append(env, EnvToken+"="+token, EnvURL+"="+c.URL()), token, undo, nil
}

// writePrivate writes a file only its owner can read, replacing any there.
func writePrivate(path string, b []byte) error {
	os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// scrub hides a run's own token in its output, on top of redact's patterns.
func scrub(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, redact.Mask)
	}
	return redact.String(s)
}
