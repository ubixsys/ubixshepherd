# CLAUDE.md

Guidance for Claude Code (or any AI session) working in this repository.

## Where things stand

uBixShepherd is in **active build**. M1 is complete; M2 is partly implemented, with
workspace-wide leases and established real use still outstanding. M3 proofs and M5 dispatch
are partly implemented; M4's MCP tools and terminal front desk are implemented; M6 has not
started. The code includes per-repo scope leases, lane origins, tag reservations and import;
Claude Code, Copilot and Cursor dispatch with configured permission modes and repo-level
push and merge autonomy; per-run costs, quota holds and routed requests; GitLab polling,
merge closure and failed-pipeline handoffs; and `shepherd chat`. GitHub mirror proofs through
publication, typed cross-repo work orders and triage, and the end-to-end cross-repo delivery
criterion are not implemented. The web UI is a preview in `web/`, not yet served by the
daemon.
v1's scope and stack were decided on 2026-10-01: a **Go** core (daemon, CLI, MCP server, HTTP
API in one binary for Windows, macOS and Linux), the Fold and dispatch together, GitLab and
GitHub, running over a workspace of repos, terminal first with a TypeScript web UI later.
It must work on anyone's repos and is **aimed at uBixCore**: uBixCore support lives in a
pack, never in the core (see `design.md` §3.12). The docs:

- [README.md](README.md): what it is, in one screen.
- [docs/vision.md](docs/vision.md): purpose, capabilities, and what it is not.
- [docs/design.md](docs/design.md): **the core design** (proposed). The two rules that
  shape everything: "the shepherd is not a sheep" (a deterministic control plane, LLMs only
  at the edges) and "enforce at the boundaries every provider must cross".
- [docs/v1.md](docs/v1.md): **what v1 builds**: the decided calls, then the proposed
  shape, Fold, dispatch, cutover from `AGENTS-COORD.md`, and milestones M1 to M6.
- [docs/roadmap.md](docs/roadmap.md): the MVP feature list per milestone, then next steps,
  nice-to-haves, the landscape and risks, from the 2026-10-01 research (proposal).
- [docs/naming.md](docs/naming.md): the name, the family vocabulary (uBixFlock, Fold,
  Crook, Pasture), and rejected names. Use this vocabulary consistently.
- [docs/origins.md](docs/origins.md): the practices around uBixCore that Shepherd
  formalises. **Read before designing anything**, and read the source docs it points to.
- [docs/open-questions.md](docs/open-questions.md): the calls, decided and open (numbers
  are stable; a decided question keeps its number).
- [docs/pitch.md](docs/pitch.md): elevator pitch, one-liner, tagline (the README quotes
  the elevator pitch; keep the two identical).

## Code

Go, one binary (`cmd/shepherd`), packages under `internal/`. `make check` is the gate
(gofmt, vet, tests, `core-boundary`); `make build` gives `bin/shepherd`, `make cross` the six
release targets, `make dist` their release archives and `SHA256SUMS`. GitLab CI runs
`public-boundary`, `go-check` and `go-cross`, then `promote-to-main` on `dev`; GitHub
Actions runs `make check` on Linux, macOS and (informational) Windows, and publishes releases.

- The daemon (`internal/daemon`) owns the store; the CLI is a client of the HTTP API
  (`internal/api`, `internal/client`) like every other client. Don't let a command open the
  store directly.
- `internal/store` is an interface; `store/sqlite` uses a pure-Go driver so `CGO_ENABLED=0`
  cross-compiles. Schema changes are appended migrations, never edits.
- Text that stores or shows agent output goes through `internal/redact`.
- `internal/fold` owns lanes, per-repo scope leases, lane origins, tag reservations and
  import. It drives the git CLI through `internal/git`, never a git library, so hooks and
  config behave as for people. Its tests build real repos with a bare origin; keep them
  that way.
- `shepherd mcp` (`internal/cli/mcp.go`) maps each MCP tool onto a CLI command and runs
  it with output captured. Add a tool by adding a command first, then its mapping.
- `internal/dispatch` starts agents (`lane run`). Adapters for Claude Code, Copilot and
  Cursor use the repo's `agent.permission_mode` (default `auto`), `agent.model` and push/merge
  autonomy. `desk.model` and `/model` choose the front desk's model.
  Agents can push when `autonomy.push: agent`; on GitLab, `autonomy.merge: agent` lets
  them arm merge-when-pipeline-succeeds, still subject to forge rules. `lane ship` is an
  explicit Shepherd ship for repos set to `push: shepherd`, after the repo gate passes;
  Shepherd never merges. The runner records cost per run even when a CLI reports a session
  total, holds an agent that is out of quota until reset, redacts output into
  `~/.shepherd/runs/`, and records outcomes. Its tests use a fake agent script; a real run
  of each CLI is a manual check before changing an adapter. Each adapter also says how its
  CLI names a session (chosen up front, created first, or printed in the output), how to
  resume it headless, and how to attach to it.
- Worker tools (`shepherd mcp --worker`, `internal/cli/decision.go`) are for agents
  Shepherd starts: the run comes from `SHEPHERD_RUN`. A decision's answer is delivered by
  continuing the asking run's session (`dispatch.Runner.Answer`), at once or when the run
  ends. Never let an agent answer a decision: `decision_answer` takes the person's words.
- Routing between lanes is `internal/dispatch/route.go`: deterministic rules start the
  target agent or explain why they cannot. Requests
  addressed to the person become decisions. `shepherd request close <id> [--why]`
  (`runRequest` in `internal/cli/decision.go`), the `request_close` MCP tool and the desk's
  tools close a stale request through `Runner.CloseRequest`, which refuses one already
  replied to, failed or closed. Anything needing routing judgment waits for the front desk; `Route` runs
  whenever a run ends and when a quota hold lifts.
- `shepherd chat` is `internal/chat`: a Bubble Tea model over the daemon's feed, with a
  front desk (`ClaudeDesk`) run headless and resumed per turn. The thread prints inline to
  terminal scrollback, replays history on start, and has a searchable Ctrl-O transcript and
  markdown rendering. Its attention-sorted dock shows counts, lane/run state and MR
  badges, and supports answering decisions in place. Colors adapt to the terminal and
  `NO_COLOR`. The desk gets operator tools and reads only (`--disallowedTools Edit Write
  Bash`); its standing instruction is `DeskBrief`. Test the model with fakes
  (`chat_test.go`); the desk needs a real check.
- `internal/forge` reads a forge through its CLI (`glab api`), never a stored token;
  `internal/watch` polls it for open lanes and acts on changes (`Fold.CloseMerged` on a
  merge commit, the agent continued on a failed pipeline, capped at `MaxFixTries`). Lane
  views include MR and pipeline state. The feed maps events to a closed set of kinds,
  including gate, commit and run outcomes. Test the watcher with a fake forge; check the
  forge reader against a real MR read-only.
- `internal/config` validates repo profiles, including `agent.permission_mode` and the
  `autonomy.push` and `autonomy.merge` choices. `internal/daemon` reloads config on SIGHUP
  (`shepherd daemon reload` on supported systems), stops agents left by a crashed daemon,
  rotates its log and logs routine reads at debug.
- Commands autostart the daemon (`internal/cli/daemon.go`); `internal/service` registers it
  with launchd or systemd. Tests leave `Env.Autostart` false; set `SHEPHERD_NO_AUTOSTART=1`
  and `SHEPHERD_HOME` to a temp dir when running the binary by hand.
- `core-boundary` fails if `cmd/` or `internal/` names a product. Product knowledge goes in
  a pack.
- **Minimum git is 2.31** (`git.MinVersion` in `internal/git/version.go`): the push block
  (`internal/dispatch/pushblock.go`) uses `GIT_CONFIG_COUNT`, which older git silently
  ignores, and `rev-parse --path-format` is also 2.31. The daemon checks it at start
  (`daemonRun`). Ubuntu 20.04 (2.25) and Debian 11 (2.30) are too old; Ubuntu 22.04 (2.34)
  passes, and CI runs the suite on it. Before using a newer git feature in code or tests
  (`worktree add --orphan` is 2.42, `git init -b` 2.28, `--show-current` 2.22), check it
  against that floor; raising the floor means changing the constant, the README and CI.
- Keep dependencies few: the standard library first (the CLI is `flag`, not a framework).

## Releases

GitHub is the public home and where releases are published: the internal forge
push-mirrors protected branches and tags to it, GitHub's default branch is `main`, and
`.github/workflows/release.yml` builds, signs (keyless cosign) and publishes a GitHub
Release for each `v*` tag, with notes from its `CHANGELOG.md` section. It refuses a tag
that is not on `main`. Work lands on `dev` by merge request; a green `dev` is
fast-forwarded to `main` by a job on the internal forge (`bin/promote.sh`: never forced,
refuses a diverged `main`); `main` is the protected release branch. A release is a
changelog section landed on `dev` and promoted, then a tag cut on `main` and pushed to the
internal forge, never to GitHub. Read [docs/RELEASING.md](docs/RELEASING.md)
before cutting one, and [docs/VERSIONING.md](docs/VERSIONING.md) for what the number
promises. Agents never tag or publish a release unasked.

## This repo is public

It is meant to be mirrored to GitHub, and mirroring copies whole branches with their
history, so **anything committed here is public**. Write every doc and commit message for a
stranger:

- No private product names, internal hostnames, private MR numbers, incident details, or
  anyone's personal working notes. State the lesson in general terms instead.
- Maintainers keep the private evidence and research behind these docs in a separate
  private repository that is never mirrored. If you have access to it, that is where
  specifics go.
- The `public-boundary` CI job fails if the tree matches the pattern in the
  `PUBLIC_BOUNDARY_PATTERN` CI variable (set in the project settings, deliberately not in
  the repo). Public uBix projects (uBixCore, uBixVault, Replikate, UbixOS, ubixsys.com) are
  fine to name.

## Ground rules

- **The maintainer decides scope, stack and naming.** Bring open questions with the
  trade-offs and a recommendation, then a choice. Don't pick a stack and start building
  unasked.
- **Framework-grade, product-free.** Like all uBix tools, Shepherd must be useful verbatim
  to an unrelated company. No product's logic in the core.
- **No secrets in the repo.** Credentials live in uBixVault.
- **Agents never approve their own work**, here or in any repo Shepherd coordinates.
- Work lands on `dev`, the integration branch; `main` is the release branch and GitHub's
  default, moved only by promotion and never pushed to by hand. Work on a branch (e.g.
  `docs/<topic>`) and land it on `dev` by MR, with Conventional Commit style messages (`docs: ...`). Check for `AGENTS-COORD.md` before
  branching; none exists yet.
- Related public repos: `ubixcore` (the framework, uBixOps, CI tooling), `ubixsys-web`
  (where uBix projects get a docs page once released).

## Keeping the docs coherent

- The docs cross-reference each other: the README's "Read next" table indexes `docs/`,
  `design.md` §7 feeds the numbered list in `open-questions.md`, and that list links
  back into design sections by anchor (e.g. `design.md#6-a-phased-path-proposal`). When
  you add, renumber or retitle something, update the links that point at it.
- `design.md` is marked **Proposed**. Don't present anything in it as decided; a decision
  moves out of `open-questions.md` only when the maintainer makes it.
- House prose style: plain sentences, colons and commas instead of em dashes (the docs
  contain none), tables for comparisons.

## Good first step for a new session

Read all of `docs/` (design.md, then v1.md last), then the uBixCore standards that
`origins.md` cites. Check the current status in `v1.md` and `docs/roadmap.md` before
choosing implementation work: workspace-wide leases, GitHub mirror proofs, typed cross-repo
work orders and other milestone gaps remain. M6 has not started. The milestones and
everything below v1.md's "Decided" table are proposals: confirm them with the maintainer
before writing code.
