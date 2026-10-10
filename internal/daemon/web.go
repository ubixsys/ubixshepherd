package daemon

// The browser's way in: the web UI, sign-in links, session cookies, and the checks every
// browser request passes (see api/web.go for the routes).
//
// Threat model. The daemon listens on loopback only; remote use is an SSH tunnel to that
// port. What it defends against:
//
//   - A web page in the person's browser (any site, or another server on loopback at a
//     different port, which a browser counts as the same site) making the browser call
//     the API. Cookies do not isolate ports and SameSite treats every loopback port as one
//     site, so SameSite=Strict alone is not enough: a cookie request must also carry
//     the daemon's own Origin (or same-origin Sec-Fetch-Site), and a write must carry the
//     session's CSRF token in a header, which a cross-origin page can neither read nor
//     set without CORS, which the daemon never grants.
//   - DNS rebinding: a name the attacker controls, re-pointed at 127.0.0.1, makes the
//     page same-origin with the daemon in the browser's eyes. Every request must name a
//     loopback host in its Host header, so a rebound name is refused before auth.
//   - The operator token leaking through the browser. The browser never sees it: the CLI
//     trades it for a one-time sign-in code (random, a minute long, single use, compared
//     in constant time), and the code for a session cookie (random, HttpOnly,
//     SameSite=Strict, Path=/v1, 12 hours, kept in the store only as a hash, revocable).
//     The code is in the link's query, which the request log never records (it logs the
//     path), and the exchange redirects at once with no-referrer.
//   - Clickjacking an answer: pages refuse framing (CSP frame-ancestors and
//     X-Frame-Options).
//   - A stolen cookie's reach: a browser session has the web role, the front desk's
//     powers plus the conversation and the person's answers, not the operator's setup
//     routes or sign-in minting, so it cannot make itself a fresh link.
//
// What it does not: a process running as the same OS user can read daemon.json and use
// the operator token (design.md §3.17); with the operator token, a sign-in link is one
// call away. Another local user can see the link in the browser opener's arguments for
// the moment it runs, and could race the browser to spend it; `shepherd web --print` on a
// shared machine avoids that. Plain HTTP on loopback has no Secure cookies; TLS for direct
// remote access is later work and off by default.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
	"github.com/ubixsys/ubixshepherd/internal/webui"
)

// RoleWeb is a browser session: the front desk's routes, the front desk conversation, and
// the person's answers; not the daemon's setup.
const RoleWeb = "web"

const (
	// signinTTL is how long a sign-in link works; it works once.
	signinTTL = time.Minute
	// webSessionTTL is how long a browser session lasts; signing in again starts a new
	// one.
	webSessionTTL = 12 * time.Hour
	// At most this many links wait to be used, and this many sessions are live; past
	// that the oldest goes.
	maxSigninCodes = 16
	maxWebSessions = 32
	// webSeenEvery is the least time between writes of a session's last-seen time, so
	// page loads do not each write to the store.
	webSeenEvery = time.Minute
)

// webAuth holds the sign-in codes and browser sessions, by the SHA-256 of their values:
// the raw values are handed out once and never kept. Sessions are kept in the store too,
// so they survive a restart; sign-in codes are memory only, a minute long.
type webAuth struct {
	mu    sync.Mutex
	codes []signinCode
	// st keeps the sessions; nil keeps them in memory only. They are loaded on first use.
	st       store.Store
	loaded   bool
	sessions map[[32]byte]*webSession
	// warn reports a store failure; memory stays authoritative.
	warn func(msg string, err error)
	// now is the clock, for tests.
	now func() time.Time
}

type signinCode struct {
	hash    [32]byte
	expires time.Time
}

type webSession struct {
	hash     [32]byte
	csrf     string
	port     string
	created  time.Time
	lastSeen time.Time
	// saved is when lastSeen was last written to the store.
	saved   time.Time
	expires time.Time
}

func (ws *webSession) key() string { return hex.EncodeToString(ws.hash[:]) }

func (a *webAuth) warnf(msg string, err error) {
	if a.warn != nil {
		a.warn(msg, err)
	}
}

// load reads the live sessions from the store, once; a failure is tried again on the next
// call. The caller holds mu.
func (a *webAuth) load() {
	if a.loaded || a.st == nil {
		return
	}
	if a.sessions == nil {
		a.sessions = map[[32]byte]*webSession{}
	}
	rows, err := a.st.LiveWebSessions(context.Background(), a.clock())
	if err != nil {
		a.warnf("web sessions not loaded", err)
		return
	}
	a.loaded = true
	for _, r := range rows {
		raw, err := hex.DecodeString(r.Hash)
		if err != nil || len(raw) != 32 {
			continue
		}
		ws := &webSession{csrf: r.CSRF, port: r.Port, created: r.Created, lastSeen: r.LastSeen, saved: r.LastSeen, expires: r.Expires}
		copy(ws.hash[:], raw)
		a.sessions[ws.hash] = ws
	}
}

// lastPort is the listen port of the newest live session, "" if none: the port the
// daemon tries first, so that session's cookie still names it.
func (a *webAuth) lastPort() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	var newest *webSession
	now := a.clock()
	for _, s := range a.sessions {
		if now.Before(s.expires) && (newest == nil || s.created.After(newest.created)) {
			newest = s
		}
	}
	if newest == nil {
		return ""
	}
	return newest.port
}

// forget drops a session from the store, best effort. The caller holds mu.
func (a *webAuth) forget(ws *webSession) {
	if a.st == nil {
		return
	}
	if err := a.st.DeleteWebSession(context.Background(), ws.key()); err != nil {
		a.warnf("web session not removed from the store", err)
	}
}

func (a *webAuth) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// mintCode returns a new sign-in code and when it stops working.
func (a *webAuth) mintCode() (string, time.Time, error) {
	code, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	a.codes = slices.DeleteFunc(a.codes, func(c signinCode) bool { return !now.Before(c.expires) })
	if len(a.codes) >= maxSigninCodes {
		a.codes = a.codes[1:]
	}
	exp := now.Add(signinTTL)
	a.codes = append(a.codes, signinCode{hash: sha256.Sum256([]byte(code)), expires: exp})
	return code, exp, nil
}

// spend uses up a sign-in code: true if it was live. Every waiting code is compared, in
// constant time, so how long it takes says nothing about the code.
func (a *webAuth) spend(code string) bool {
	if code == "" {
		return false
	}
	h := sha256.Sum256([]byte(code))
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	found := -1
	for i, c := range a.codes {
		if subtle.ConstantTimeCompare(h[:], c.hash[:]) == 1 && now.Before(c.expires) {
			found = i
		}
	}
	if found < 0 {
		return false
	}
	a.codes = slices.Delete(a.codes, found, found+1)
	return true
}

// start begins a browser session: its cookie value, and the session.
func (a *webAuth) start(port string) (string, *webSession, error) {
	val, err := newToken()
	if err != nil {
		return "", nil, err
	}
	csrf, err := newToken()
	if err != nil {
		return "", nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	if a.sessions == nil {
		a.sessions = map[[32]byte]*webSession{}
	}
	now := a.clock()
	var oldest *webSession
	for k, s := range a.sessions {
		if !now.Before(s.expires) {
			delete(a.sessions, k)
			a.forget(s)
		} else if oldest == nil || s.expires.Before(oldest.expires) {
			oldest = s
		}
	}
	if len(a.sessions) >= maxWebSessions && oldest != nil {
		delete(a.sessions, oldest.hash)
		a.forget(oldest)
	}
	ws := &webSession{hash: sha256.Sum256([]byte(val)), csrf: csrf, port: port,
		created: now, lastSeen: now, saved: now, expires: now.Add(webSessionTTL)}
	if a.st != nil {
		err := a.st.SaveWebSession(context.Background(), store.WebSession{
			Hash: ws.key(), CSRF: csrf, Port: port, Created: now, LastSeen: now, Expires: ws.expires})
		if err != nil {
			return "", nil, fmt.Errorf("keep the session: %w", err)
		}
	}
	a.sessions[ws.hash] = ws
	return val, ws, nil
}

// session finds the live session a cookie value names.
func (a *webAuth) session(val string) (*webSession, bool) {
	if val == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	h := sha256.Sum256([]byte(val))
	ws, ok := a.sessions[h]
	if !ok {
		return nil, false
	}
	now := a.clock()
	if !now.Before(ws.expires) {
		delete(a.sessions, h)
		a.forget(ws)
		return nil, false
	}
	ws.lastSeen = now
	if a.st != nil && now.Sub(ws.saved) >= webSeenEvery {
		if err := a.st.TouchWebSession(context.Background(), ws.key(), now); err != nil {
			a.warnf("web session's last-seen time not saved", err)
		} else {
			ws.saved = now
		}
	}
	return ws, true
}

// end ends one session, or every one when ws is nil, and says how many ended.
func (a *webAuth) end(ws *webSession) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	if ws == nil {
		n := len(a.sessions)
		clear(a.sessions)
		if a.st != nil {
			stored, err := a.st.DeleteWebSessions(context.Background())
			if err != nil {
				a.warnf("web sessions not removed from the store", err)
			}
			n = max(n, stored)
		}
		return n
	}
	if _, ok := a.sessions[ws.hash]; !ok {
		return 0
	}
	delete(a.sessions, ws.hash)
	a.forget(ws)
	return 1
}

// cookieName is the session cookie's name. Browsers share cookies across a host's ports,
// so it carries the daemon's port: two daemons on one machine do not sign each other out.
func (s *Server) cookieName() string {
	if port := s.listenPort(); port != "" {
		return "shepherd_" + port
	}
	return "shepherd"
}

// listenPort is the port the daemon is listening on, "" before it is.
func (s *Server) listenPort() string {
	if a := s.addr.Load(); a != nil {
		if _, port, err := net.SplitHostPort(*a); err == nil {
			return port
		}
	}
	return ""
}

// listen binds the configured address. Sessions outlive a restart, but a cookie is named
// for its port, so when the port is left to the system (0) the port of the newest live
// session is tried first; if it is taken, any free port serves and those sessions wait
// out their expiry.
func (s *Server) listen(addr string) (net.Listener, error) {
	if host, port, err := net.SplitHostPort(addr); err == nil && port == "0" {
		if last := s.web.lastPort(); last != "" && last != "0" {
			if ln, err := net.Listen("tcp", net.JoinHostPort(host, last)); err == nil {
				return ln, nil
			}
		}
	}
	return net.Listen("tcp", addr)
}

// allowedHost says whether a request's Host header names this machine's loopback: a
// rebound DNS name, or anything else, is refused before it reaches a handler.
func (s *Server) allowedHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	// The address the daemon is pinned to (daemon.listen, as it bound).
	if a := s.addr.Load(); a != nil {
		if h, _, err := net.SplitHostPort(*a); err == nil && strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// sameOrigin says whether a request's Origin is the origin it was sent to.
func sameOrigin(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Origin"), "http://"+r.Host)
}

// browserCheck refuses a cookie request that did not come from the daemon's own pages:
// another origin's Origin, a cross-site fetch, and a write without Origin or without
// the session's CSRF token.
func browserCheck(r *http.Request, ws *webSession) error {
	if o := r.Header.Get("Origin"); o != "" && !sameOrigin(r) {
		return fmt.Errorf("a request from %s cannot use this daemon's session", o)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return errors.New("a request from another site cannot use this daemon's session")
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	if r.Header.Get("Origin") == "" {
		return errors.New("a browser request that changes something must carry its Origin")
	}
	if got := r.Header.Get(api.CSRFHeader); subtle.ConstantTimeCompare([]byte(got), []byte(ws.csrf)) != 1 {
		return fmt.Errorf("missing or wrong %s header", api.CSRFHeader)
	}
	return nil
}

// webPrincipal is the browser session a request's cookie names, once it passes the
// browser checks. No cookie, or a dead session, is no principal; a live session from the
// wrong origin is an error.
func (s *Server) webPrincipal(r *http.Request) (principal, error) {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return principal{}, nil
	}
	ws, ok := s.web.session(c.Value)
	if !ok {
		return principal{}, nil
	}
	if err := browserCheck(r, ws); err != nil {
		return principal{}, err
	}
	return principal{Role: RoleWeb, Human: true, web: ws}, nil
}

// browserGate checks every request's Host, sets the headers every response carries, and
// sends a request to the sign-in exchange, the API (behind auth) or the web UI.
func (s *Server) browserGate(apiHandler http.Handler) http.Handler {
	ui := webui.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if !s.allowedHost(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, fmt.Errorf("host %q is not this daemon's; reach it at 127.0.0.1 or localhost", r.Host))
			return
		}
		switch {
		case r.URL.Path == api.PathWebSignin && r.Method == http.MethodGet:
			s.webExchange(w, r)
		case r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/"):
			h.Set("Cache-Control", "no-store")
			apiHandler.ServeHTTP(w, r)
		default:
			ui.ServeHTTP(w, r)
		}
	})
}

// webSignin mints a one-time sign-in link (POST /v1/web/signin, operator only).
func (s *Server) webSignin(w http.ResponseWriter, r *http.Request) {
	code, exp, err := s.web.mintCode()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("web sign-in link issued", "expires", exp.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, api.WebSignin{Path: api.PathWebSignin + "?code=" + url.QueryEscape(code), Expires: exp.UTC()})
}

// webExchange spends a sign-in link's code for a session cookie, and sends the browser on
// to the UI (GET /v1/web/signin?code=, no token). A link opened from another site is
// refused, so no page can sign the person in to a session it chose.
func (s *Server) webExchange(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", webui.CSP)
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" && site != "same-origin" {
		signinPage(w, http.StatusForbidden, "A sign-in link only works when you open it yourself, not from another page.")
		return
	}
	if !s.web.spend(r.URL.Query().Get("code")) {
		s.Log.Warn("web sign-in refused: the link is unknown, used or expired")
		signinPage(w, http.StatusUnauthorized, "This sign-in link has expired or was already used. Run shepherd web for a new one.")
		return
	}
	// A browser signing in again leaves its old session behind: end it.
	if c, err := r.Cookie(s.cookieName()); err == nil {
		if old, ok := s.web.session(c.Value); ok {
			s.web.end(old)
		}
	}
	val, ws, err := s.web.start(s.listenPort())
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: val, Path: "/v1", MaxAge: int(webSessionTTL / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	s.Log.Info("web session started", "expires", ws.expires.UTC().Format(time.RFC3339))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func signinPage(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="color-scheme" content="light dark"><link rel="icon" href="data:,"><title>Shepherd: sign in</title></head>
<body><h1>Not signed in</h1><p>%s</p></body></html>
`, msg)
}

// webSessionInfo answers GET /v1/web/session: who the caller is, and for a browser its
// CSRF token, which it sends on every write.
func (s *Server) webSessionInfo(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	out := api.WebSession{Role: p.Role}
	if p.web != nil {
		out.CSRF, out.Expires = p.web.csrf, p.web.expires.UTC()
	}
	writeJSON(w, http.StatusOK, out)
}

// webSignout ends the caller's own browser session and clears its cookie.
func (s *Server) webSignout(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	if p.web == nil {
		writeError(w, http.StatusBadRequest, errors.New("not a browser session; end them all with DELETE "+api.PathWebSessions))
		return
	}
	n := s.web.end(p.web)
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/v1", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	s.Log.Info("web session ended")
	writeJSON(w, http.StatusOK, api.WebSignedOut{Ended: n})
}

// webEndAll ends every browser session (DELETE /v1/web/sessions, operator only).
func (s *Server) webEndAll(w http.ResponseWriter, r *http.Request) {
	n := s.web.end(nil)
	s.Log.Info("web sessions ended", "count", n)
	writeJSON(w, http.StatusOK, api.WebSignedOut{Ended: n})
}
