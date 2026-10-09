# Changelog

All notable changes to uBixShepherd are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/) as described in
[docs/VERSIONING.md](docs/VERSIONING.md). While Shepherd is in beta, a change to a public
interface (the CLI, the config file schema, the HTTP API, the MCP tools or the hook
protocol) is listed under **Interface changes** in the release that makes it.

## [Unreleased]

### Added

- **Scoped tokens.** Besides the operator token in `daemon.json`, the daemon mints a
  worker token for each agent run, passed to the agent as `SHEPHERD_TOKEN` with the API
  address in `SHEPHERD_URL`, and revoked when the run ends or the daemon restarts. A
  worker token may only report, ask the person, ask another lane, reserve and release
  its own lane's tags, read its own run and run the pre-push check. A desk role, for a
  front desk the daemon runs, gets the operator tools but may answer a decision only in
  a turn the person started. See `docs/design.md` §3.17.
- **The front desk in the daemon.** The daemon runs a front desk of its own, one
  conversation per workspace kept in the store, that clients follow instead of starting
  their own: `POST /v1/desk/turn`, `GET /v1/desk/stream` (server-sent events, resumable
  by sequence number), `GET /v1/desk/history`, `GET /v1/desk/status`,
  `POST /v1/desk/interrupt` and `POST /v1/desk/new`. Swarm events wake it on its own
  as the new `desk.wake` setting says (`attached`, the default, `always` or `never`),
  as one digest when nobody was attached. A turn it takes on its own cannot answer a
  decision. Each turn's own cost is recorded as `desk` spend. `shepherd chat` keeps its
  own desk for now. See `docs/design.md` §3.18.

### Interface changes

- The HTTP API answers 403 to a scoped token calling an endpoint its role does not
  allow. The operator token is unchanged and may call every endpoint.
- `shepherd worker` commands and `shepherd mcp`, when started with `SHEPHERD_TOKEN`
  set, call the daemon at `SHEPHERD_URL` with that token and no longer read
  `daemon.json`. A worker command with no run token refuses instead of finding its run
  from the lane.

### Known limits

- **Scoped tokens are not isolation.** They stop accidents and tool misuse, but an
  agent running as the same OS user as the daemon can read `daemon.json` and obtain
  the operator token. Real isolation needs agents under a separate account or an OS
  sandbox, planned for later.

## [0.1.0-beta.1] - 2026-10-09

The first public beta of uBixShepherd: one local daemon coordinates agent work across a
workspace of Git repositories. Work happens in scoped lanes, agents run in those
worktrees, and the front desk brings decisions and activity into one terminal
conversation. This is a beta: public interfaces may change between releases. See
https://github.com/ubixsys/ubixshepherd/blob/main/docs/VERSIONING.md.

### Added

- **One binary and daemon.** `shepherd` provides the daemon, CLI, HTTP API and MCP
  server. It builds for Linux, macOS and Windows on amd64 and arm64. Commands start the
  daemon when needed. `shepherd daemon install` registers a login service with launchd
  on macOS or systemd on Linux. The daemon listens on loopback, stores state in SQLite
  under `~/.shepherd` or `$SHEPHERD_HOME`, and stops agents left behind after a crash.
  On systems with SIGHUP, `shepherd daemon reload` reloads configuration. The daemon
  writes and rotates its own log.
- **Workspaces and repo profiles.** `shepherd init` registers a directory of
  repositories and lets you choose which it manages. Profiles in
  `~/.shepherd/config.yaml` set the base branch, branch model, gate, shared paths,
  worktree setup, agent permission mode, and push, merge and tag autonomy. Defaults
  keep merge, tag and deploy decisions with the person.
- **Lanes and per-repo scope leases.** `shepherd lane open` creates a branch from a
  fresh fetch of the base branch, a worktree and a declared scope. Overlapping scopes
  in the same repository are refused, naming the lane and paths in conflict. Lane
  records retain who opened them and through which surface. `lane close`, `lane scope`,
  `lane review`, `fold gc` and `fold retire` support lane cleanup and review without
  removing unfinished work.
- **Pre-push and release-tag checks.** When it can safely install its hook,
  `lane open` adds the pre-push check; `shepherd hook` can install, remove or report on
  it, and does not replace another hook. For lane pushes the hook checks the branch,
  scope and configured commit-message rules. With `tags: reserved`, it also checks that
  pushed release tags were reserved and include the lane's merge.
- **Lane import and origin records.** `shepherd fold import` can turn a coordination
  file's lane table and matching worktrees into Shepherd lanes, reporting what it
  cannot import. Lane records capture their origin, such as the CLI, MCP, front desk,
  import or a followed release. With `coord_file` configured, Shepherd maintains a
  generated lane and tag-reservation view in that file.
- **Tag reservations.** `shepherd tag reserve` allocates release versions against
  remote tags and live reservations. With `tags: reserved`, the hook refuses an
  unreserved release tag and checks that a lane's tag contains its merge.
- **Agent runs.** `shepherd lane run` starts Claude Code, GitHub Copilot CLI or Cursor's
  `cursor-agent` headless in a lane, under daemon supervision. Prompts are sent on
  standard input, not exposed in process command lines. Each lane keeps its agent
  session for later runs; `run continue`, `run attach`, `run logs`, `run stop` and
  `run show` manage runs. Agent output is redacted before it is stored or shown, and
  `daemon.max_runs` limits concurrent runs per machine.
- **Permission modes and repo autonomy.** Agents use their repo's
  `agent.permission_mode`, defaulting to `auto`. Claude Code supports `auto`,
  `acceptEdits`, `default` and `bypassPermissions`; Copilot maps `auto` and
  `bypassPermissions` to allowing all tools and uses an allow list for the other modes.
  Cursor has no comparable permission modes and runs with `--force`. By default
  agents cannot push. `autonomy.push: agent` allows an agent to push its own lane to
  the repository's `origin`; `autonomy.merge: agent` lets it arm its own GitLab merge
  request to merge when the pipeline succeeds. With `autonomy.push: shepherd`,
  Shepherd checks scope and commit rules, runs the repo's gate itself, then pushes and
  opens or updates the merge request. `shepherd lane ship` does that for committed
  lane work on request. Shepherd never merges.
- **Decisions and requests.** Worker tools let agents use `ask_human` to hold a
  decision with options and a recommendation, `ask_shepherd` to ask a question, hand
  off work or request a review from another lane, and `report` to record progress,
  done or blocked. Answers and replies continue the asking agent's conversation.
  Deterministic rules route requests between lanes or explain why they cannot; reviews
  use a different provider in a fresh session. `shepherd decision` and
  `shepherd request` list and act on them.
- **Cost and quota tracking.** Each run records its attributable cost: dollars for
  Claude Code, Copilot credits converted at a configurable rate, and no reported cost
  for Cursor. `daemon.budget` holds runs Shepherd starts on its own after daily spend
  reaches the limit. An agent that reports a quota limit is held until its reset time,
  or retried after a pause when no reset time is available.
- **GitLab lane watching.** The daemon polls open lanes' merge requests through the
  local `glab` login and stores no forge credentials. A merge closes a lane using the
  forge's merge commit as proof, including squash merges. Failed pipeline job logs are
  sent back to the lane's agent to fix, at most twice per merge request. When a forge
  host is unreachable, the watcher backs off from one to fifteen minutes and resets
  the wait when the host responds.
- **Following dependency releases.** A repo profile's `follows` entry can open a lane
  and start an agent when a dependency publishes a release. It can wait for the
  release pipeline to pass, or follow tags directly.
- **Adopted conversations.** `shepherd session` imports Claude Code sessions run by
  hand in a managed repo, so they can be reopened or asked questions. Their answers
  are read-only, and a lane can route questions to an adopted conversation.
- **MCP tools.** `shepherd mcp` serves operator tools over stdio for Claude Code or
  another MCP client. Tools map to Shepherd CLI commands. Worker tools are for agents
  Shepherd starts.
- **The terminal front desk.** `shepherd chat` runs a Claude Code front desk with
  operator tools and read access to files, but no edit or shell tools. The conversation
  prints inline to terminal scrollback and replays recent history at startup. Ctrl-O
  opens the full searchable transcript in a pager, with markdown rendering in the
  thread. An attention-sorted dock shows lane and run state, merge-request badges and
  counts, and decisions can be answered in place.

### Interface changes

- Added `shepherd request close <id> [--why TEXT]` and the `request_close` MCP tool,
  also available to the front desk. They close a stale request without a reply and
  retain the reason. Requests already replied to, failed or closed are refused.
- Added `/model` in chat to show, set or reset the desk's model. The
  `shepherd chat --model` flag overrides it for that chat, `desk.model` configures the
  front desk, and per-repo `agent.model` selects a model per agent when a run does not
  specify one.
- Added `daemon.log_level` to select `debug`, `info`, `warn` or `error`; a configuration
  reload applies it immediately.

### Changed

- A `go install` build reports the module version recorded by Go instead of `dev`.
- `autonomy.push: agent` now permits a running agent to push only its own lane branch
  to the repo's `origin`; the hook checks the target remote as well as the branch,
  scope and configured commit rules.

### Known limits

- **GitLab is the only forge for lanes.** Merge request polling, merge closure,
  pipeline handoffs and merge arming use GitLab through `glab`. GitHub is the release
  mirror and publishing home; GitHub mirror and publication proofs are not built.
- **macOS and Linux are tested; Windows builds in CI but is untested.** Windows has
  no service-manager integration, and does not support SIGHUP reload.
- **Not built yet:** workspace-wide leases (scopes are per repository), typed and
  cross-repo work orders, triage, the OpenCode adapter, `verified-on:<env>` proofs,
  and the proposed merge-discipline checks. See
  https://github.com/ubixsys/ubixshepherd/blob/main/docs/roadmap.md.
- **The web UI is a preview.** Its sources are in `web/`; it runs from a development
  server against a running daemon, which does not serve it.
- **`autonomy.push: agent` uses a cooperative pre-push guard, not a security boundary.**
  An agent can bypass it with `git push --no-verify` or by editing the hook. Forge
  branch protection and merge rules remain the enforcement boundary.
- **No independent security review.** See
  https://github.com/ubixsys/ubixshepherd/blob/main/docs/VERSIONING.md#what-the-version-does-not-communicate.
