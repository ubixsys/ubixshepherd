package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// syncBuffer is a log the daemon's goroutines may write to at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// webDaemon runs a real daemon in-process, listening on loopback the way the binary does,
// logging everything to the buffer it returns.
func webDaemon(t *testing.T) (*Server, string, *syncBuffer) {
	t.Helper()
	s, _ := newServer(t)
	logs := &syncBuffer{}
	s.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rtPath := filepath.Join(t.TempDir(), "daemon.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, rtPath) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rt, err := ReadRuntime(rtPath); err == nil {
			return s, "http://" + rt.Addr, logs
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never wrote its runtime file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// browser is an HTTP client shaped like a browser on the daemon's page: a cookie jar,
// no redirects followed (the test looks at each), and the headers a browser adds.
type browser struct {
	t      *testing.T
	c      *http.Client
	base   string
	origin string // the Origin header to send; "" sends none
	site   string // Sec-Fetch-Site; "" sends none
	csrf   string // sent on writes when set
	host   string // overrides the Host header when set
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, origin: base, site: "same-origin", c: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) do(method, path string, body any) (*http.Response, string) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	if b.host != "" {
		// A browser would keep the daemon's cookie to the daemon's own host; send it
		// anyway, the strongest case for the Host check.
		req.Host = b.host
		for _, c := range b.c.Jar.Cookies(req.URL) {
			req.AddCookie(c)
		}
	}
	// A browser sends Origin on every write, and on a read only when it is cross-origin;
	// the test sends what it is told to.
	if b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	if b.site != "" {
		req.Header.Set("Sec-Fetch-Site", b.site)
	}
	if b.csrf != "" && method != http.MethodGet {
		req.Header.Set(api.CSRFHeader, b.csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

// operatorCall calls the daemon with the operator token, the way the CLI does.
func operatorCall(t *testing.T, s *Server, base, method, path string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func signinLink(t *testing.T, s *Server, base string) api.WebSignin {
	t.Helper()
	var link api.WebSignin
	if code := operatorCall(t, s, base, "POST", api.PathWebSignin, &link); code != http.StatusOK {
		t.Fatalf("mint a sign-in link: %d", code)
	}
	if !strings.HasPrefix(link.Path, api.PathWebSignin+"?code=") || strings.Contains(link.Path, s.Token) {
		t.Fatalf("link %q", link.Path)
	}
	return link
}

// signIn opens a fresh link in the browser and takes its CSRF token.
func signIn(t *testing.T, s *Server, b *browser) {
	t.Helper()
	link := signinLink(t, s, b.base)
	origin, site := b.origin, b.site
	b.origin, b.site = "", "none" // the person opens it from the terminal
	resp, _ := b.do("GET", link.Path, nil)
	b.origin, b.site = origin, site
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("exchange: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ws api.WebSession
	b.origin = ""
	resp, body := b.do("GET", api.PathWebSession, nil)
	b.origin = origin
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &ws) != nil || ws.Role != RoleWeb || ws.CSRF == "" {
		t.Fatalf("session: %d %s", resp.StatusCode, body)
	}
	b.csrf = ws.CSRF
}

// The sign-in link becomes a session cookie with the attributes the threat model needs,
// and the operator token shows up nowhere a browser or the log could keep it.
func TestWebSignin(t *testing.T) {
	s, base, logs := webDaemon(t)
	link := signinLink(t, s, base)
	b := newBrowser(t, base)
	b.origin, b.site = "", "none"
	resp, body := b.do("GET", link.Path, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("exchange: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("exchange headers: %v", resp.Header)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %v", cookies)
	}
	c := cookies[0]
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	if c.Name != "shepherd_"+port || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/v1" ||
		c.MaxAge != int(webSessionTTL/time.Second) || len(c.Value) < 64 {
		t.Errorf("cookie %+v", c)
	}
	if c.Value == s.Token {
		t.Error("the cookie is the operator token")
	}
	// The cookie is sent to the API only, never to the UI's files.
	if u, _ := url.Parse(base + "/"); len(b.c.Jar.Cookies(u)) != 0 {
		t.Error("the cookie is sent outside /v1")
	}

	// The UI loads, built or not, and holds no secret.
	b.origin, b.site = "", "same-origin"
	resp, body = b.do("GET", "/", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<title>Shepherd") {
		t.Errorf("/: %d %q", resp.StatusCode, body)
	}
	if strings.Contains(body, s.Token) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("page headers %v", resp.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS header on the page")
	}

	// The browser reads the API with its cookie.
	resp, body = b.do("GET", api.PathStatus, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, s.Token) {
		t.Errorf("status: %d %s", resp.StatusCode, body)
	}

	// Neither the token nor the link's code reaches the log.
	code := strings.TrimPrefix(link.Path, api.PathWebSignin+"?code=")
	for _, secret := range []string{s.Token, code, c.Value} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("a secret is in the log:\n%s", logs.String())
		}
	}
}

// A link works once, for a minute, and only when the person opens it.
func TestWebSigninLinkExpiryAndReuse(t *testing.T) {
	s, base, _ := webDaemon(t)
	open := func(path, site string) int {
		b := newBrowser(t, base)
		b.origin, b.site = "", site
		resp, body := b.do("GET", path, nil)
		if len(resp.Cookies()) != 0 && resp.StatusCode != http.StatusSeeOther {
			t.Errorf("a refused exchange set a cookie: %s", body)
		}
		return resp.StatusCode
	}

	link := signinLink(t, s, base)
	// From another site's page: refused, and the link is not spent.
	if code := open(link.Path, "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site exchange: %d", code)
	}
	if code := open(link.Path, "same-site"); code != http.StatusForbidden {
		t.Errorf("same-site exchange: %d", code)
	}
	if code := open(link.Path, "none"); code != http.StatusSeeOther {
		t.Errorf("first use: %d", code)
	}
	if code := open(link.Path, "none"); code != http.StatusUnauthorized {
		t.Errorf("second use: %d", code)
	}

	// Expired: a minute later it no longer works.
	now := time.Now()
	s.web.mu.Lock()
	s.web.now = func() time.Time { return now }
	s.web.mu.Unlock()
	link = signinLink(t, s, base)
	s.web.mu.Lock()
	s.web.now = func() time.Time { return now.Add(signinTTL + time.Second) }
	s.web.mu.Unlock()
	if code := open(link.Path, "none"); code != http.StatusUnauthorized {
		t.Errorf("expired link: %d", code)
	}

	// Wrong, empty and truncated codes.
	link = signinLink(t, s, base)
	for _, p := range []string{api.PathWebSignin, api.PathWebSignin + "?code=", link.Path[:len(link.Path)-1], link.Path + "0"} {
		if code := open(p, "none"); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, code)
		}
	}
}

// Only the operator mints links: not the front desk, an agent, or a browser session.
func TestWebSigninNeedsTheOperator(t *testing.T) {
	s, base, _ := webDaemon(t)
	desk, _ := s.MintDesk(true, "")
	worker, _ := s.MintWorker(1)
	for _, tok := range []string{desk, worker, "nonsense"} {
		req, _ := http.NewRequest("POST", base+api.PathWebSignin, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !denied(resp.StatusCode) {
			t.Errorf("token %q minted a link: %d", tok[:4], resp.StatusCode)
		}
	}
	b := newBrowser(t, base)
	signIn(t, s, b)
	if resp, _ := b.do("POST", api.PathWebSignin, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a browser session minted a link: %d", resp.StatusCode)
	}
}

// Cookie writes need the daemon's own Origin and the session's CSRF token; reads and
// streams need no other origin and no cross-site fetch.
func TestWebOriginAndCSRF(t *testing.T) {
	s, base, _ := webDaemon(t)
	b := newBrowser(t, base)
	signIn(t, s, b)
	// A write that passes authorization reaches the handler, which refuses it on its
	// merits: this daemon runs no agents.
	answer := api.PathDecisions + "/999/answer"
	write := func() int {
		resp, _ := b.do("POST", answer, api.Answer{Answer: "yes"})
		return resp.StatusCode
	}
	if code := write(); denied(code) {
		t.Fatalf("same-origin write with the CSRF token: %d", code)
	}

	csrf := b.csrf
	b.csrf = ""
	if code := write(); code != http.StatusForbidden {
		t.Errorf("write without the CSRF token: %d", code)
	}
	b.csrf = strings.Repeat("0", len(csrf))
	if code := write(); code != http.StatusForbidden {
		t.Errorf("write with a wrong CSRF token: %d", code)
	}
	b.csrf = csrf

	for _, o := range []string{"", "http://evil.example", "null", strings.Replace(base, "127.0.0.1", "localhost", 1),
		"http://127.0.0.1:1", "https://" + strings.TrimPrefix(base, "http://")} {
		b.origin = o
		if code := write(); code != http.StatusForbidden {
			t.Errorf("write with Origin %q: %d", o, code)
		}
	}
	b.origin = base
	for _, site := range []string{"cross-site", "same-site"} {
		b.site = site
		if code := write(); code != http.StatusForbidden {
			t.Errorf("write with Sec-Fetch-Site %s: %d", site, code)
		}
	}
	b.site = "same-origin"

	// Reads: a cross-origin read, or a cross-site fetch with no Origin (an <img>, a
	// <script>), is refused; a same-origin one, or an old browser's with neither header,
	// is not.
	read := func(origin, site string) int {
		b.origin, b.site = origin, site
		resp, _ := b.do("GET", api.PathDecisions, nil)
		return resp.StatusCode
	}
	if code := read("", "same-origin"); code != http.StatusOK {
		t.Errorf("same-origin read: %d", code)
	}
	if code := read("", ""); code != http.StatusOK {
		t.Errorf("read with neither header: %d", code)
	}
	if code := read("http://evil.example", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-origin read: %d", code)
	}
	if code := read("", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site no-cors read: %d", code)
	}
	if code := read("http://127.0.0.1:1", "same-site"); code != http.StatusForbidden {
		t.Errorf("read from another loopback port: %d", code)
	}
	b.origin, b.site = base, "same-origin"

	// Streams: the browser's EventSource. Same-origin streams; another origin's does not.
	stream := func(origin string) int {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", base+api.PathFeedStream, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		u, _ := url.Parse(base + api.PathFeedStream)
		for _, c := range b.c.Jar.Cookies(u) {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := stream(base); code != http.StatusOK {
		t.Errorf("same-origin stream: %d", code)
	}
	if code := stream("http://evil.example"); code != http.StatusForbidden {
		t.Errorf("cross-origin stream: %d", code)
	}
}

// A DNS name rebound to 127.0.0.1 is refused before authentication, with a cookie or a
// token; loopback names are not.
func TestWebHostAllowList(t *testing.T) {
	s, base, _ := webDaemon(t)
	b := newBrowser(t, base)
	signIn(t, s, b)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	for _, h := range []string{"evil.example:" + port, "evil.example", "127.0.0.1.evil.example:" + port,
		"localhost.evil.example:" + port, "10.0.0.1:" + port, "0.0.0.0:" + port} {
		b.host = h
		b.origin = "http://" + h
		if resp, _ := b.do("GET", api.PathStatus, nil); resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("cookie with Host %q: %d", h, resp.StatusCode)
		}
		if resp, _ := b.do("GET", "/", nil); resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("page with Host %q: %d", h, resp.StatusCode)
		}
		req, _ := http.NewRequest("GET", base+api.PathStatus, nil)
		req.Host = h
		req.Header.Set("Authorization", "Bearer "+s.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("token with Host %q: %d", h, resp.StatusCode)
		}
	}
	// Loopback names, at any port: an SSH tunnel's local port is not the daemon's.
	for _, h := range []string{"localhost:" + port, "LOCALHOST:" + port, "127.0.0.1:" + port, "[::1]:" + port, "127.0.0.2:8080", "localhost"} {
		b.host = h
		b.origin = "http://" + h
		if resp, body := b.do("GET", api.PathStatus, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("Host %q: %d %s", h, resp.StatusCode, body)
		}
	}
}

// The daemon grants no CORS: a preflight gets nothing that would let a page through.
func TestWebNoCORS(t *testing.T) {
	_, base, _ := webDaemon(t)
	req, _ := http.NewRequest("OPTIONS", base+api.PathDecisions+"/1/answer", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, "+api.CSRFHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("preflight answered with %s", k)
		}
	}
	if resp.StatusCode/100 == 2 {
		t.Errorf("preflight: %d", resp.StatusCode)
	}
}

// A token request a browser sent from another origin is refused too: no page should
// hold one.
func TestWebTokenFromAnotherOrigin(t *testing.T) {
	s, base, _ := webDaemon(t)
	req, _ := http.NewRequest("GET", base+api.PathStatus, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("token from another origin: %d", resp.StatusCode)
	}
}

// What a browser session may do: the front desk's routes, the desk conversation and
// answers, never the daemon's setup.
func TestWebRole(t *testing.T) {
	s, base, _ := webDaemon(t)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := s.Store.SaveWorkspace(ctx, store.Workspace{Name: "ws", Path: root}, nil); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, base)
	signIn(t, s, b)
	cases := []struct {
		method, path string
		body         any
		allowed      bool
	}{
		{"GET", api.PathStatus, nil, true},
		{"GET", api.PathWorkspaces, nil, true},
		{"GET", api.PathLanes + "?workspace_id=1", nil, true},
		{"GET", api.PathRuns, nil, true},
		{"GET", api.PathDecisions, nil, true},
		{"GET", api.PathRequests, nil, true},
		{"GET", api.PathFeed, nil, true},
		{"GET", api.PathSpend, nil, true},
		{"POST", api.PathDecisions + "/1/answer", api.Answer{Answer: "x"}, true},
		{"GET", api.PathDeskHistory, nil, true},
		{"GET", api.PathDeskStatus, nil, true},
		{"POST", api.PathDeskInterrupt, api.DeskWorkspace{}, true},
		{"GET", api.PathWebSession, nil, true},
		{"POST", api.PathShutdown, nil, false},
		{"POST", api.PathWorkspaces, map[string]any{}, false},
		{"POST", api.PathLanes + "/1/scope", map[string]any{}, false},
		{"POST", "/v1/repos/1/hook", map[string]any{}, false},
		{"POST", api.PathFoldImport, map[string]any{}, false},
		{"POST", api.PathFoldRetire, map[string]any{}, false},
		{"POST", api.PathSessionsImport, map[string]any{}, false},
		{"POST", api.PathSpend, map[string]any{}, false},
		{"GET", api.PathSettings + "/desk.model", nil, false},
		{"PUT", api.PathSettings + "/desk.model", api.Setting{Value: "x"}, false},
		{"POST", api.PathWebSignin, nil, false},
		{"DELETE", api.PathWebSessions, nil, false},
		{"POST", api.PathPrePush, map[string]any{}, false},
	}
	for _, c := range cases {
		resp, body := b.do(c.method, c.path, c.body)
		if got := !denied(resp.StatusCode); got != c.allowed {
			t.Errorf("%s %s: %d %s, want allowed %v", c.method, c.path, resp.StatusCode, body, c.allowed)
		}
	}
	// The desk and agents still cannot reach the conversation.
	desk, _ := s.MintDesk(true, "")
	req, _ := http.NewRequest("GET", base+api.PathDeskHistory, nil)
	req.Header.Set("Authorization", "Bearer "+desk)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("desk token on the desk history: %d", resp.StatusCode)
	}
}

// Sessions end: by signing out, by the operator ending them all, by time, and when the
// same browser signs in again.
func TestWebSessionEnds(t *testing.T) {
	s, base, _ := webDaemon(t)
	status := func(b *browser) int {
		resp, _ := b.do("GET", api.PathStatus, nil)
		return resp.StatusCode
	}

	// Sign out: the session ends and the cookie is cleared. Signing out is a write, so
	// another origin cannot do it for the person.
	b := newBrowser(t, base)
	signIn(t, s, b)
	b.origin = "http://evil.example"
	if resp, _ := b.do("POST", api.PathWebSignout, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin sign-out: %d", resp.StatusCode)
	}
	b.origin = base
	resp, body := b.do("POST", api.PathWebSignout, nil)
	var out api.WebSignedOut
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.Ended != 1 {
		t.Fatalf("sign out: %d %s", resp.StatusCode, body)
	}
	if code := status(b); code != http.StatusUnauthorized {
		t.Errorf("after sign-out: %d", code)
	}

	// The operator ends every session.
	b1, b2 := newBrowser(t, base), newBrowser(t, base)
	signIn(t, s, b1)
	signIn(t, s, b2)
	if code := operatorCall(t, s, base, "DELETE", api.PathWebSessions, &out); code != http.StatusOK || out.Ended != 2 {
		t.Errorf("end all: %d %+v", code, out)
	}
	if status(b1) != http.StatusUnauthorized || status(b2) != http.StatusUnauthorized {
		t.Error("a session outlived DELETE /v1/web/sessions")
	}

	// Signing in again ends the browser's old session: a copy of the old cookie no
	// longer works.
	b = newBrowser(t, base)
	signIn(t, s, b)
	u, _ := url.Parse(base + "/v1/")
	old := b.c.Jar.Cookies(u)
	signIn(t, s, b)
	stale := newBrowser(t, base)
	stale.c.Jar.SetCookies(u, []*http.Cookie{{Name: old[0].Name, Value: old[0].Value, Path: "/v1"}})
	if code := status(stale); code != http.StatusUnauthorized {
		t.Errorf("old session after signing in again: %d", code)
	}
	if code := status(b); code != http.StatusOK {
		t.Errorf("new session: %d", code)
	}

	// Twelve hours on, the session is over.
	now := time.Now()
	s.web.mu.Lock()
	s.web.now = func() time.Time { return now.Add(webSessionTTL + time.Minute) }
	s.web.mu.Unlock()
	if code := status(b); code != http.StatusUnauthorized {
		t.Errorf("expired session: %d", code)
	}
}
