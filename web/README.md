# Shepherd web UI

A browser client of the Shepherd daemon's HTTP API, like the CLI and `shepherd chat`. It
shows the board (lanes grouped by what they need from you, with the terminal dock's rules),
a lane's runs and timeline, the open decisions, any run's log, and the front desk's chat,
live. What it changes is a decision's answer and what you say to the front desk, only from
your click or your Enter.

Vite, React 19 and TypeScript (strict). No router, no state library, no CSS framework.

## Use it

The daemon serves the built app on its own address. Sign a browser in with:

```sh
shepherd web          # opens a one-time sign-in link in your browser
shepherd web --print  # prints the link instead (a shared machine, or a tunnel)
shepherd web --sign-out-all
```

The link works once, for a minute. Opening it gives the browser a session cookie and
sends it to the board; the browser never sees the operator token. The session lasts 12
hours, or until you sign out, run `shepherd web --sign-out-all`, or the daemon restarts.

From another machine, use an SSH tunnel to the daemon's loopback port. Pin the port in
the daemon's config (`daemon.listen: 127.0.0.1:7400`), then:

```sh
ssh -L 7400:127.0.0.1:7400 HOST      # leave it open
ssh HOST shepherd web --print        # open the printed link here
```

## Build and develop it

You need Node 22 or later. `make build` at the repo root builds the app and embeds it in
the binary (`internal/webui`); without npm, or with `NO_WEB=1`, the binary builds without
it and serves a page saying how to build it. For development, run the dev server against
a running daemon (`shepherd daemon start`, or any `shepherd` command, which starts one):

```sh
cd web
npm install
npm run dev      # http://localhost:5178
npm run build    # the static app in web/dist
npm run check    # typecheck, lint and tests
```

## How it talks to the daemon

The app calls the API by root-relative URL (`/v1/lanes`, `/v1/feed`, ...), so it works
unchanged wherever it is served. It never sees a token.

Served by the daemon, the browser holds a session cookie (`HttpOnly`, `SameSite=Strict`,
for `/v1` only). Every request a browser makes must name a loopback host, and must not
come from another origin or site; a request that changes something also sends the
session's CSRF token, which the client reads once from `GET /v1/web/session`, in
`X-Shepherd-CSRF`. The daemon grants no CORS. Pages forbid framing and load nothing from
another origin (a strict Content-Security-Policy). A browser session can do what the
front desk can, talk to the front desk and answer decisions; the daemon's setup
(workspaces, scopes, hooks, imports, settings, shutdown) stays with the `shepherd`
command.

In development, `daemon-proxy.ts` is a dev-server middleware that forwards `/v1` to the
daemon. It reads the daemon's address and token from its runtime file, `daemon.json` in
`SHEPHERD_HOME` (`~/.shepherd` by default), on every request, so a restarted daemon is
followed, and adds the token to the forwarded request only. Nothing prints or logs it.
Since it adds the operator's token, it makes the daemon's browser checks itself first (a
loopback host, nothing from another origin or site, and an Origin on every write), and
forwards none of the browser's cookies or origin headers.

### Endpoints for the web

| Endpoint | What |
|---|---|
| `GET /v1/web/session` | The caller's role; for a browser, its CSRF token and expiry. A 401 means not signed in |
| `POST /v1/web/signout` | End this browser's session |
| `/v1/desk/*` | The front desk conversation (see `internal/api/desk.go`), open to a browser session |
| `GET /v1/feed/stream`, `GET /v1/desk/stream` | Server-sent events, for `EventSource` on the same origin |
| `POST /v1/web/signin`, `DELETE /v1/web/sessions` | Operator only: what `shepherd web` uses |

### The chat page

`#/chat` is a thin client of the daemon-hosted front desk, the same conversation `shepherd
chat` shows. It reads the thread from `GET /v1/desk/history`, then follows
`/v1/desk/stream` with an `EventSource`. An `EventSource` sends no headers, so the stream
resumes by `?after=<seq>` of the last stored event; after a drop the page reopens it with a
backoff (0.5 s doubling to 15 s), asks `/v1/desk/status` why when it fails (a 401 stops
the retries and shows the sign-in hint, `shepherd web`), and shows the connection state in
the page head. A `partial` piece carries no id: pieces grow one message, and the stored
`assistant` event replaces them, as in the terminal.

- Enter sends (`POST /v1/desk/turn`, with the CSRF header like every write), Shift+Enter
  adds a line. The box keeps your words if the daemon refuses: a full queue (429), a
  signed-out session (401) and a refusal (403) each say so.
- Interrupt shows while a turn runs. New conversation asks first; the daemon rotates the
  session.
- Events from the feed (a run ending, quota, a decision asked, and so on) appear as
  compact rows between messages with the board's glyphs. An open decision shows inline with
  its options and is answered through the same `Live.answer` call as the Decisions page.
- Everything a model or agent wrote is rendered as text or a small markdown subset
  (paragraphs, headings, lists, fenced code, `**bold**`, `` `code` ``, and links) built as
  elements: no `innerHTML`, no raw HTML, and links only for `http` and `https`, with
  `rel="noopener noreferrer"`.

Updates elsewhere come from polling: the feed with its cursor every 1.5 s (10 s while the tab is
hidden), which also triggers a reload of the lists; the lists themselves every 5 s (30 s
hidden). A server-sent event stream will replace the feed poll.

## Layout

| Path | What |
|---|---|
| `src/api/` | The API's JSON types (from `internal/api` and `internal/store`) and the client |
| `src/state/live.ts` | The one live view of the daemon: lists, the feed window, polling, what you have seen |
| `src/model/` | Pure logic: the board's grouping (`board.ts`, the dock's rules), event marks, feed refs, the log parser |
| `src/state/desk.ts` | The front desk thread: history, the event stream, resume, sends |
| `src/pages/` | Board, Lane, Decisions, Log, Chat |
| `src/styles/` | `theme.css` (tokens, light and dark) and `app.css` |

## Keys

`j`/`k` move on the board, `Enter` opens, `g b` and `g d` go to the board and decisions,
`c` marks everything done as seen, `/` searches a run log (`Enter`, `n`, `N` step through
matches), `t` expands tool calls, `f` follows the log, `?` lists the keys.
