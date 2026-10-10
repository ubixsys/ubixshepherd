package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/client"
	"github.com/ubixsys/ubixshepherd/internal/dispatch"
	"github.com/ubixsys/ubixshepherd/internal/fold"
)

func runHook(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	if args[0] == "pre-push" {
		return hookPrePush(ctx, env, args[1:])
	}
	fs := flags("hook "+args[0], env)
	repo := fs.String("repo", "", "repo, by its name in the workspace (default: the one you are in)")
	if pos, err := parse(fs, args[1:]); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	switch args[0] {
	case "install", "uninstall", "status":
	default:
		return errUsage
	}
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	h, err := locate(ctx, env, c, *repo)
	if err != nil {
		return err
	}
	if h.Repo == nil {
		return errors.New("which repo? run this inside one, or pass --repo")
	}
	st, err := c.RepoHook(ctx, h.Repo.ID, args[0])
	if err != nil {
		return err
	}
	w := env.Stdout
	switch {
	case st.Ours:
		fmt.Fprintf(w, "Shepherd's pre-push hook is installed at %s\n", st.Path)
		if st.Tracked && args[0] == "install" {
			fmt.Fprintln(w, "That directory is outside .git: commit the hook if it is tracked.")
		}
	case st.Foreign:
		fmt.Fprintf(w, "%s is another pre-push hook. To chain Shepherd's check, add:\n    %s\n", st.Path, fold.HookLine)
	default:
		fmt.Fprintf(w, "no pre-push hook at %s\n", st.Path)
	}
	return nil
}

// hookPrePush is what the installed hook runs. git starts it in the worktree being
// pushed from, with the remote's name (or URL, when it has no name) and URL as
// arguments and the refs on stdin.
func hookPrePush(ctx context.Context, env Env, args []string) error {
	env.Client = "hook"
	in, err := io.ReadAll(env.Stdin)
	if err != nil {
		return err
	}
	refs, err := fold.ParsePushRefs(string(in))
	if err != nil {
		return err
	}
	c, err := hookClient(ctx, env)
	if err != nil {
		return fmt.Errorf("cannot check this push: %w\n(push without Shepherd's check: git push --no-verify)", err)
	}
	req := api.PrePush{Path: env.Cwd, Refs: refs}
	if len(args) > 0 {
		req.Remote = args[0]
	}
	v, err := c.PrePush(ctx, req)
	if err != nil {
		return fmt.Errorf("cannot check this push: %w\n(push without Shepherd's check: git push --no-verify)", err)
	}
	for _, n := range v.Notes {
		fmt.Fprintf(env.Stderr, "shepherd: %s\n", n)
	}
	if v.OK {
		return nil
	}
	fmt.Fprintf(env.Stderr, "shepherd: push refused for lane %s\n", v.Lane)
	for _, p := range v.Problems {
		fmt.Fprintf(env.Stderr, "  - %s\n", p)
	}
	fmt.Fprintln(env.Stderr, "Widen the scope with a new lane, move the change to the lane that owns it, or skip once: git push --no-verify")
	return errSilent
}

// hookClient is the daemon client the pre-push hook calls with. A push by an agent
// Shepherd started runs with that run's SHEPHERD_URL and SHEPHERD_TOKEN, which are the
// only credentials it needs: use them, and never start a daemon for it. Only when both
// are unset does the hook read the runtime file, as a push by the person does.
func hookClient(ctx context.Context, env Env) (*client.Client, error) {
	tok, base := os.Getenv(dispatch.EnvToken), os.Getenv(dispatch.EnvURL)
	if tok == "" && base == "" {
		return dial(ctx, env)
	}
	if tok == "" || base == "" {
		return nil, fmt.Errorf("%s and %s must be set together", dispatch.EnvURL, dispatch.EnvToken)
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%s is not a daemon URL", dispatch.EnvURL)
	}
	c := client.New(u.Scheme+"://"+u.Host, tok)
	c.Name = env.Client
	return c, nil
}
