package api

import "time"

// The browser's way in. The daemon serves the web UI at / on its own address, and a
// browser reaches the API with a session cookie instead of a token:
//
//	POST   /v1/web/signin         operator: a one-time sign-in link, WebSignin
//	GET    /v1/web/signin?code=C  the browser opens the link: the code is spent, a session
//	                              cookie is set, and it is sent on to /
//	GET    /v1/web/session        the browser's session: WebSession (its CSRF token)
//	POST   /v1/web/signout        the browser ends its own session
//	DELETE /v1/web/sessions       operator: end every browser session, WebSignedOut
//
// A browser session has the web role: everything the front desk may do, the front desk
// conversation (api/desk.go), and answering decisions as the person. It cannot change
// the daemon's setup (workspaces, scopes, hooks, imports, settings, shutdown) or mint
// sign-in links; the shepherd command does those with the operator token.
//
// A request with the cookie must come from the daemon's own origin: its Origin header,
// when present, must be http://<the Host it was sent to>, and Sec-Fetch-Site, when
// present, same-origin (or none, for a link the person opened). A request that changes
// anything (not GET or HEAD) must also carry Origin and CSRFHeader with the session's
// CSRF token. The daemon never sends CORS headers. Every request, with a token or not,
// must name a loopback host (localhost, 127.0.0.0/8, ::1) in its Host header, so a DNS
// name rebound to 127.0.0.1 cannot reach the API from a web page.
const (
	PathWebSignin   = "/v1/web/signin"
	PathWebSession  = "/v1/web/session"
	PathWebSignout  = "/v1/web/signout"
	PathWebSessions = "/v1/web/sessions"
)

// CSRFHeader carries a browser session's CSRF token on every request that changes
// something.
const CSRFHeader = "X-Shepherd-CSRF"

// WebSignin answers POST /v1/web/signin. Path is the link without its scheme and host,
// for the client to put on the address it reaches the daemon at; it works once, until
// Expires.
type WebSignin struct {
	Path    string    `json:"path"`
	Expires time.Time `json:"expires"`
}

// WebSession answers GET /v1/web/session. CSRF is set for a browser session only.
type WebSession struct {
	Role    string    `json:"role"`
	CSRF    string    `json:"csrf,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
}

// WebSignedOut answers POST /v1/web/signout and DELETE /v1/web/sessions: how many
// sessions ended.
type WebSignedOut struct {
	Ended int `json:"ended"`
}
