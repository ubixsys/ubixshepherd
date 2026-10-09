# Changelog

All notable changes to uBixShepherd are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/) as described in
[docs/VERSIONING.md](docs/VERSIONING.md). While Shepherd is in beta, a change to a public
interface (the CLI, the config file schema, the HTTP API, the MCP tools or the hook
protocol) is listed under **Interface changes** in the release that makes it.

## [Unreleased]

### Added

- **Close a request.** `shepherd request close <id> [--why TEXT]` and the `request_close`
  tool (also available to the front desk) close a stale request without a reply and keep
  the reason. A request already replied to, failed or closed is refused.
- **Model settings.** `desk.model` sets the front desk's model, `/model` shows, sets or
  resets it in the chat, and `agent.model` sets a model per agent for a repo.
- **`daemon.log_level`** chooses what the daemon logs, applied on reload.

### Changed

- A `go install` build reports the module version Go recorded instead of `dev`.
- The daemon backs off from a forge host that cannot be reached.
- `autonomy.push: agent` works: the pre-push hook tells the daemon which remote is being
  pushed to, and an agent's push is allowed only to the lane repo's `origin`.

## [0.1.0-beta.1] - 2026-10-09

The first public release: a beta. Shepherd runs as one daemon per machine over a
workspace of git repos, keeps parallel agent work apart in lanes, starts and follows
Claude Code, Copilot and Cursor in them, and brings you what is yours to decide in one
thread. Interfaces may still change between betas; see
[docs/VERSIONING.md](docs/VERSIONING.md).

### Added

- **One binary, one daemon.** `shepherd` is the daemon, the CLI, the MCP server and the
  HTTP API, built for Linux, macOS and Windows on amd64 and arm64. Commands start the
  daemon when it is not running; `shepherd daemon install` registers it with launchd
  (macOS) or systemd (Linux). The daemon listens on loopback only, keeps its state in a
  SQLite store under `~/.shepherd` (or `$SHEPHERD_HOME`), reloads its config on SIGHUP
  where the OS supports it, rotates its log, and stops agents left behind by a crash.
- **Workspaces and repo profiles.** `shepherd init` registers a directory of repos and
  lets you choose which Shepherd manages. Each repo gets a profile in
  `~/.shepherd/config.yaml`: base branch, branch model, gate command, shared paths,
  worktree setup, agent permission mode, and push, merge and tag autonomy, over cautious
  defaults (a person merges, tags and deploys).
- **Lanes, the Fold.** `shepherd lane open` gives a stream of work its own branch, cut
  from a fresh fetch of the base, its own worktree and a declared scope. Scopes are
  per-repo leases: an overlapping scope is refused, naming the lane that holds it. Lanes
  record who opened them and through which surface. `lane close`, `lane scope`,
  `lane review`, `fold gc` and `fold retire` cover the rest of a lane's life, and refuse
  to remove work that is not finished.
- **The pre-push hook.** A push from a lane may only update the lane's branch with
  changes inside its scope. `lane open` installs the hook when that is safe;
  `shepherd hook` installs, removes and checks it, and never replaces another hook.
- **Moving a hand-coordinated repo across.** `shepherd fold import` turns a lane table in
  a coordination file and its worktrees into lanes, reporting what it would not import
  and why. With `coord_file` set, Shepherd keeps a generated table of its lanes and tag
  reservations at the top of that file for sessions not yet on Shepherd.
- **Tag reservations.** `shepherd tag reserve` hands out release versions one at a
  time against the remote's tags and every live reservation. With `tags: reserved`, the
  hook refuses an unreserved release tag, and one cut before its lane's merge.
- **Agent runs.** `shepherd lane run` starts Claude Code, GitHub Copilot CLI or Cursor's
  `cursor-agent` headless in a lane, owned by the daemon. A lane keeps its agent's
  session, so the next run continues it; `run continue`, `run attach`, `run logs`,
  `run stop` and `run show` work on any run. Output is redacted before it is stored or
  shown. `daemon.max_runs` caps concurrent runs per machine.
- **Autonomy per repo.** Agents run with the repo's `agent.permission_mode` (default
  `auto`). `autonomy.push: agent` lets agents push their own lane; on GitLab,
  `autonomy.merge: agent` lets them arm merge-when-pipeline-succeeds. With
  `autonomy.push: shepherd`, Shepherd runs the repo's gate itself when a run ends with
  commits in scope, then pushes and opens or updates the merge request; `lane ship` does
  the same on request. Commit messages are checked against the profile's `forbid`
  patterns first. Shepherd never merges.
- **Decisions and requests.** Agents Shepherd starts get worker tools: `ask_human`
  holds a decision for you with options and a recommendation, `ask_shepherd` sends a
  question, hand-off or review to another lane, and `report` records progress, done or
  blocked. Answers and replies continue the asking agent's conversation. Deterministic
  rules route requests between lanes or say why they cannot; a review goes to a different
  provider in a fresh session. `shepherd decision` and `shepherd request` list and act
  on them.
- **Cost and quota.** Every run records its attributable cost (dollars for Claude Code,
  credits for Copilot at a configurable price, nothing for Cursor, which reports none).
  `daemon.budget` holds the runs Shepherd would start on its own once the day's spend
  reaches it. An agent that is out of quota is held until its limit resets.
- **Watching GitLab.** For each open lane, the daemon polls its merge request through
  your own `glab` login, so Shepherd stores no forge credentials. A merge closes the
  lane on the forge's merge commit as proof (squash merges included); a failed pipeline
  sends the failed jobs' logs back to the lane's agent to fix, at most twice per merge
  request.
- **Following a dependency's releases.** A `follows` entry in a repo's profile opens a
  lane and an agent run when a repo it depends on publishes a new release.
- **Adopting outside conversations.** `shepherd session` imports Claude Code sessions
  you ran by hand in a managed repo, so you can reopen or ask them, and so questions to
  their lane can reach them read-only.
- **MCP.** `shepherd mcp` serves the operator tools over stdio for Claude Code or any
  MCP client; each tool runs the CLI command of the same name.
- **`shepherd chat`.** One terminal conversation with Shepherd's front desk, a Claude
  Code session that can use the operator tools and read files but not edit them. The
  thread prints to terminal scrollback with history replayed on start, a searchable
  transcript, markdown rendering, an attention-sorted dock of lanes, runs and merge
  requests, and decisions answered in place.

### Known limits

- **GitLab is the only forge for lanes.** Merge request polling, merge closure,
  pipeline handoffs and merge arming are GitLab only, through `glab`. GitHub's role in
  this release is the public mirror where releases are published; GitHub mirror and
  publication proofs are not built.
- **macOS and Linux are tested; Windows builds but is untested.** `daemon install` has
  no Windows service manager yet, and SIGHUP reload is not available there.
- **Not built yet:** workspace-wide leases (scopes are leased per repo), typed and
  cross-repo work orders, triage, `verified-on:<env>` proofs, and the merge discipline
  checks proposed in [docs/roadmap.md](docs/roadmap.md).
- **The web UI is a preview.** Its sources are in `web/` and it runs from a dev server
  against a running daemon; the daemon does not serve it.
- **`push: agent` relies on the pre-push hook, which an agent could bypass**
  (`git push --no-verify`, or editing the hook). The forge's own branch protection and
  merge rules are the boundary an agent cannot step around. The scope check is a guard
  against mistakes, not a security control.
- **No independent security review.** See
  [docs/VERSIONING.md](docs/VERSIONING.md#what-the-version-does-not-communicate).
