package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/daemon"
)

// runWeb is `shepherd web`: sign a browser in to the web UI the daemon serves. The daemon
// trades the operator token for a link that works once, for a minute; opening it gives
// the browser a session cookie, so the token never reaches the browser.
func runWeb(ctx context.Context, env Env, args []string) error {
	fs := flags("web", env)
	printOnly := fs.Bool("print", false, "print the sign-in link instead of opening a browser")
	endAll := fs.Bool("sign-out-all", false, "end every browser session")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	if env.Remote != nil {
		return errors.New("shepherd web signs in on the daemon's own machine. Reach it over an SSH tunnel to its loopback port " +
			"(pin daemon.listen to a fixed port, then ssh -L PORT:127.0.0.1:PORT HOST) and run shepherd web --print there")
	}
	// Dial starts the daemon if it is not running; the runtime file then has its token.
	c, err := dial(ctx, env)
	if err != nil {
		return err
	}
	rt, err := daemon.ReadRuntime(env.Layout.Runtime())
	if err != nil {
		return err
	}
	base := c.Addr()
	if *endAll {
		var out api.WebSignedOut
		if err := webCall(ctx, base, rt.Token, env.Client, http.MethodDelete, api.PathWebSessions, &out); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "ended %s\n", plural(out.Ended, "browser session", "browser sessions"))
		return nil
	}
	var link api.WebSignin
	if err := webCall(ctx, base, rt.Token, env.Client, http.MethodPost, api.PathWebSignin, &link); err != nil {
		return err
	}
	u := base + link.Path
	note := fmt.Sprintf("The link works once, until %s.", link.Expires.Local().Format("15:04:05"))
	if *printOnly {
		fmt.Fprintln(env.Stdout, u)
		fmt.Fprintln(env.Stderr, note)
		return nil
	}
	if err := openBrowser(u); err != nil {
		fmt.Fprintf(env.Stderr, "could not open a browser (%v); open this link yourself:\n", err)
		fmt.Fprintln(env.Stdout, u)
		fmt.Fprintln(env.Stderr, note)
		return nil
	}
	fmt.Fprintf(env.Stdout, "opened Shepherd at %s in your browser. %s\n", base, note)
	return nil
}

// webCall makes one call to the daemon with the operator token.
func webCall(ctx context.Context, base, token, client, method, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if client != "" {
		req.Header.Set(api.ClientHeader, client)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return fmt.Errorf("daemon: %s", e.Error)
		}
		return fmt.Errorf("daemon: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// openBrowser opens u in the person's browser; tests replace it.
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
