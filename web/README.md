# Shepherd web UI

A browser client of the Shepherd daemon's HTTP API, like the CLI and `shepherd chat`. It
shows the board (lanes grouped by what they need from you, with the terminal dock's rules),
a lane's runs and timeline, the open decisions, and any run's log, live. The one thing it
changes is a decision's answer, and only from your click.

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

Updates come from polling: the feed with its cursor every 1.5 s (10 s while the tab is
hidden), which also triggers a reload of the lists; the lists themselves every 5 s (30 s
hidden). A server-sent event stream will replace the feed poll.

## Layout

| Path | What |
|---|---|
| `src/api/` | The API's JSON types (from `internal/api` and `internal/store`) and the client |
| `src/state/live.ts` | The one live view of the daemon: lists, the feed window, polling, what you have seen |
| `src/model/` | Pure logic: the board's grouping (`board.ts`, the dock's rules), event marks, feed refs, the log parser |
| `src/pages/` | Board, Lane, Decisions, Log |
| `src/styles/` | `theme.css` (tokens, light and dark) and `app.css` |

## Keys

`j`/`k` move on the board, `Enter` opens, `g b` and `g d` go to the board and decisions,
`c` marks everything done as seen, `/` searches a run log (`Enter`, `n`, `N` step through
matches), `t` expands tool calls, `f` follows the log, `?` lists the keys.
