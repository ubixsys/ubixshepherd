package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ubixsys/ubixshepherd/internal/api"
)

// openLink opens a sign-in link the way a browser does when the person opens it.
func openLink(t *testing.T, u string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Sec-Fetch-Site", "none")
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestWebOpensASigninLink(t *testing.T) {
	h := newHarness(t, "", false)
	var opened []string
	defer func(f func(string) error) { openBrowser = f }(openBrowser)
	openBrowser = func(u string) error { opened = append(opened, u); return nil }

	if err := runWeb(context.Background(), h.env, nil); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || !strings.Contains(opened[0], api.PathWebSignin+"?code=") || strings.Contains(opened[0], h.srv.Token) {
		t.Fatalf("opened %q", opened)
	}
	if strings.Contains(h.out.String(), "code=") || strings.Contains(h.out.String()+h.err.String(), h.srv.Token) {
		t.Errorf("printed the link or the token: %s %s", h.out.String(), h.err.String())
	}
	if resp := openLink(t, opened[0]); resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 {
		t.Errorf("exchange: %d %v", resp.StatusCode, resp.Cookies())
	}
	if resp := openLink(t, opened[0]); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the link worked twice: %d", resp.StatusCode)
	}
}

// --print prints the link (and only the link) on stdout; a browser that will not open
// falls back to it.
func TestWebPrint(t *testing.T) {
	h := newHarness(t, "", false)
	defer func(f func(string) error) { openBrowser = f }(openBrowser)
	openBrowser = func(string) error { t.Error("--print opened a browser"); return nil }
	if err := runWeb(context.Background(), h.env, []string{"--print"}); err != nil {
		t.Fatal(err)
	}
	u := strings.TrimSpace(h.out.String())
	if !strings.HasPrefix(u, "http://127.0.0.1:") || strings.Contains(u, h.srv.Token) || !strings.Contains(h.err.String(), "works once") {
		t.Fatalf("out %q err %q", u, h.err.String())
	}
	if resp := openLink(t, u); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("printed link: %d", resp.StatusCode)
	}

	h.out.Reset()
	openBrowser = func(string) error { return errors.New("no display") }
	if err := runWeb(context.Background(), h.env, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), api.PathWebSignin+"?code=") {
		t.Errorf("no link to fall back on: %q", h.out.String())
	}
}

func TestWebSignOutAll(t *testing.T) {
	h := newHarness(t, "", false)
	if err := runWeb(context.Background(), h.env, []string{"--print"}); err != nil {
		t.Fatal(err)
	}
	openLink(t, strings.TrimSpace(h.out.String()))
	h.out.Reset()
	if err := runWeb(context.Background(), h.env, []string{"--sign-out-all"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(h.out.String()); got != "ended 1 browser session" {
		t.Errorf("out %q", got)
	}
	if err := runWeb(context.Background(), h.env, []string{"extra"}); !errors.Is(err, errUsage) {
		t.Errorf("extra argument: %v", err)
	}
}
