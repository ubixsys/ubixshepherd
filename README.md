<img src="docs/brand/logo.svg" alt="uBixShepherd" width="228">

# uBixShepherd

**One voice to direct a whole swarm of AI agents.**

uBixShepherd is a master control agent: a single point of communication between one human
and many AI agents (Claude, Gemini, and whatever comes next). You talk to Shepherd; Shepherd
hands out the work, keeps the agents from stepping on each other, holds the decisions that
are yours for you, and reports back in one thread.

> Status: **beta**. The first release is `v0.1.0-beta.1`. Shepherd's interfaces (the CLI,
> the config file, the HTTP API, the MCP tools and the hook protocol) may still change
> between betas, and every such change is called out in [CHANGELOG.md](CHANGELOG.md); see
> [docs/VERSIONING.md](docs/VERSIONING.md). Lanes, scope leases, the pre-push hook, tag
> reservations, agent runs with Claude Code, Copilot and Cursor, decisions, routed
> requests, GitLab polling and `shepherd chat` work today. GitLab is the only forge for
> lanes, macOS and Linux are tested while Windows builds untested, and workspace-wide
> leases, typed cross-repo work orders and triage are not built yet. The web UI in `web/`
> is a preview the daemon does not serve. The changelog's "Known limits" has the full list,
> and [docs/v1.md](docs/v1.md) and [docs/roadmap.md](docs/roadmap.md) the plan.

Part of the **uBix** family of open-source systems tooling (uBixCore, uBixVault, uBixOps,
Replikate, UbixOS), published under [uBixSys](https://ubixsys.com).

## Install

**From a release (recommended).** Download the archive for your OS and architecture from
[GitHub Releases](https://github.com/ubixsys/ubixshepherd/releases), with `SHA256SUMS` and
`SHA256SUMS.sigstore.json`, verify it ([docs/RELEASING.md](docs/RELEASING.md#verifying-a-download)
has the commands), unpack it and put `shepherd` on your PATH, for example in
`~/.local/bin`.

**With Go** (see `go.mod` for the version):

```sh
go install github.com/ubixsys/ubixshepherd/cmd/shepherd@v0.1.0-beta.1
```

A `go install` build reports the module version Go recorded (`v0.1.0-beta.1` above); a build from a checkout with plain `go build` reports `dev`, and only builds made with `make` stamp the version.

**From source:** clone the repo and run `make install` (below).

Shepherd needs git, and the CLIs of the agents you want it to start (`claude`, `copilot`,
`cursor-agent`) and of your forge (`glab`), each logged in on this machine.

## Build and run

Needs Go (see `go.mod` for the version) and git.

```sh
make build                  # bin/shepherd for this machine
make check                  # gofmt, vet, tests, and the core boundary check
make cross                  # dist/ for Linux, macOS and Windows on amd64 and arm64
make install                # copy to ~/.local/bin/shepherd and restart a running daemon

bin/shepherd init ~/git     # finds the repos below ~/git; you choose which Shepherd manages
bin/shepherd status         # the daemon, its workspaces, and where you are
bin/shepherd where          # the workspace, repo and lane for this directory, with its profile
```

### Lanes

A lane is one stream of work in a repo: its own branch, cut from a fresh fetch of the
repo's base branch, its own worktree, and a declared scope.
Shepherd records who opened each lane and through which surface, where known.

```sh
cd ~/git/myrepo
shepherd lane open feat/login --scope 'src/auth/**' --scope docs/auth.md
cd ~/git/myrepo-worktrees/feat-login      # work here, or start an agent here
shepherd lane list                          # this repo's lanes; --all, or run at ~/git, for every repo
shepherd lane scope --add 'src/session/**'  # widen (refused if another lane holds it) or --remove
shepherd lane close                         # from inside the worktree, or: shepherd lane close feat/login
shepherd fold gc                            # worktrees that look finished, across the workspace
shepherd hook status                        # is the pre-push hook installed in this repo
```

A lane's scope is its lease: `lane open` refuses a scope that overlaps an open lane's,
naming the lane and the paths, judged against the repo's files plus globs for paths not
created yet. It reports any of the repo profile's `shared_paths` the lane takes. It
also refuses a name, branch or worktree path already in use. A repo profile's `setup`
runs in each new worktree (with `$SHEPHERD_REPO` set to the main checkout), for what a
fresh worktree needs before its gate can run, such as an `.env` or `vendor/`.

The **pre-push hook** enforces the scope where every agent has to pass: a push from a
lane may only update the lane's branch, with changes inside its scope. `lane open`
installs the hook when that is safe (no `pre-push` hook yet, hooks inside `.git`);
otherwise it says what to do. `shepherd hook install | uninstall | status` manage it by
hand; it never replaces another hook, and prints the line to add to one instead. Pushes
from outside a lane are left alone, and `git push --no-verify` skips the check once.

 `lane close` refuses a
worktree with uncommitted changes, and a branch git cannot see merged into the base. A
squash merge looks unmerged to git, so after one, `lane close --force` closes the lane and
keeps the branch. `fold gc` only lists; it removes nothing.

### The one conversation: `shepherd chat`

```sh
cd ~/git
shepherd chat
```

You talk to Shepherd's **front desk**: a Claude Code session Shepherd runs at the
workspace root, resumed turn by turn, with Shepherd's operator tools and read access to
files but no way to edit them. It delegates: it opens lanes, starts Claude Code, Copilot
or Cursor in them, follows up, routes requests between them, and brings you what is
yours. The thread prints inline to terminal scrollback, with recent history replayed when
chat starts and a searchable full transcript on Ctrl-O. Replies render markdown. An
attention-sorted dock shows counts, lane and run states, merge-request badges and decision
answers in place. Colors adapt to light or dark terminals, and `NO_COLOR` is respected.
The feed uses a closed set of event kinds for lane, run, commit, gate, decision, request,
merge-request, pipeline and other outcomes. The conversation is kept between `shepherd chat`
sessions.

| In the thread | |
|---|---|
| `/answer 4 2` | answer decision 4 yourself, by option number or in words |
| `/decisions` | what is waiting for you |
| `/log 22` | run 22's live output; Esc back to the thread |
| `/auto off` | keep swarm events from reaching the desk on their own |
| `/sessions` | your adopted conversations (also listed beside the thread) |
| `/attach 71ffa009` | step into that conversation in Claude Code; exit it to come back |
| `/ask 71ffa009 …` | ask that conversation a question; the answer lands in the thread |
| `/model` | show the desk's model and where it came from; `/model opus` sets it, `/model reset` goes back to `config.yaml` |
| `/new` | start a new conversation with the desk |

### Moving a repo onto Shepherd

A repo already coordinated by hand, with a lane table in a file like `AGENTS-COORD.md`
and worktrees per session, comes over in two steps:

```sh
shepherd fold import --repo myrepo           # what would become lanes, and why the rest would not
shepherd fold import --repo myrepo --apply   # do it
```

Each worktree whose branch matches a row's branch prefix becomes a lane with that row's
scope (the backticked paths in it; when a row names none, the directories the branch
changed, marked as inferred). Worktrees whose branch is already in the base are
reported as finished, worktrees no row claims as unregistered, rows with no worktree as
claims with nothing in flight, and overlapping scopes are listed: the old file allowed
them, Shepherd refuses new ones.

`shepherd lane review` judges every worktree, lane or not, and shows its evidence:
**finished** (every change in the base by content, or in a merged request), **live**
(uncommitted work, an open request, or a commit or conversation in the last two weeks),
or **unclear** (work in neither, nothing recent: yours to decide). It changes nothing.
`shepherd fold retire <worktree>` removes a finished one and keeps its branch; it refuses
anything else, and lanes.

While sessions not yet on Shepherd still read the file, set `coord_file: AGENTS-COORD.md`
in the repo's profile: Shepherd then keeps a generated table of its lanes and tag
reservations at the top of the file, between marker comments, and never touches the
rest. `shepherd fold view` shows it; `--write` writes it now.

### Tags

Two lanes in one repo can each take "the next version". `shepherd tag reserve` hands
versions out one at a time, past the highest release tag on the remote (read fresh) and
every live reservation:

```sh
shepherd tag reserve minor             # in a lane: v0.45.0 is yours
shepherd tag reserve patch --no-lane   # for a release cut outside any lane
shepherd tag list                      # reserved, pushed, verified
shepherd tag release v0.45.0           # give one back
```

With `tags: reserved` in a repo's profile, the pre-push hook enforces it on every push
from the repo, lane or not: a release tag must be reserved (from a lane, by that lane),
and once its lane has merged, the tag must contain the merge, so a release cannot be cut
before the work it is for. Agents reserve with their `tag_reserve` tool.

### What it costs

Every run records its attributable cost: Claude Code's cost in dollars, Copilot's
credits (priced at `daemon.credit_usd`, $0.04 by default, an estimate), nothing for
Cursor, which reports nothing. When a CLI reports a session total, Shepherd records the
increase for that run. The front desk's turns count too. `shepherd status`, the chat's
status line and `run show` show it.

`daemon.budget` (default $20 a day, 0 for no cap) holds the runs Shepherd would start on
its own, pipeline and gate fixes and routed requests, once the day's spend reaches it;
runs you or the desk start still go. A line in the thread warns at 80% and at 100%.

### Watching the forge

Every minute (`daemon.poll`, `off` to stop), Shepherd looks up the merge request for each
open lane's branch on GitLab, through your own `glab` login, so it keeps no forge
credentials. What changed lands in the thread, and some of it is acted on:

- **Merged:** the forge's merge (or squash) commit is the proof. The lane closes and its
  local branch goes, no `--force` needed after a squash merge. A worktree with
  uncommitted changes, or an agent still running, keeps the lane open and says why.
- **Pipeline failed:** the failed jobs' logs go back into the lane's agent conversation
  with "fix it and commit", at most twice per merge request; then it is yours. Who pushes
  the fix follows the repo profile: `agent`, `shepherd` after its gate, or you by default.
- **Opened, pipeline passed, canceled, closed without merging:** a line in the thread.

Agents run with the repo's `agent.permission_mode` (default `auto`). Repo profiles can
grant `autonomy.push: agent` so agents push their own lane, or `autonomy.merge: agent` so
they can arm their GitLab merge request to merge when its pipeline succeeds. Forge rules,
approvals and pipeline requirements still apply.

**Shepherd can push, for repos you opt in.** With `autonomy.push: shepherd` in a repo's
profile (and a `gate` set), when an agent's run ends with commits inside its scope,
Shepherd runs the gate itself in the lane. If it passes, Shepherd pushes the branch
(the scope hook still checks it) and opens the merge request, or updates the open one;
if it fails, the output goes back into the agent's conversation to fix, at most twice.
It waits while the agent is waiting on you or another lane, or said it was blocked. It
never merges. For repos configured with `autonomy.push: shepherd`, you can also ask Shepherd to ship
committed lane work with `shepherd lane ship`, which runs the repo gate before pushing and
opening or updating the merge request.
The default is `push: human`: agents commit, you push.

```yaml
repos:
  ubixshepherd:
    base_branch: dev
    gate: make check
    brief: No Co-authored-by or AI tool trailers in commit messages.
    forbid: ["(?i)co-authored-by"]
    autonomy:
      push: shepherd
```

`brief` adds the repo's own rules to every agent's brief; `forbid` holds patterns
commit messages must not match before Shepherd pushes (agent CLIs such as Copilot and
Cursor add attribution trailers by default). A match goes back to the agent to amend its
unpushed commits.

GitHub as a lane's forge comes later; GitHub's role in v1 is the release mirror.

### When a repo you depend on releases

A framework tags a version, and every app on it needs a branch that moves to it. A
`follows` entry in the app's profile has Shepherd do that: when the followed repo's
newest release tag appears on its remote and the tag's pipeline passes (so the package is
published), Shepherd opens a lane in the app and hands its agent the task, with the
tag's message and the commit subjects since the last release in the prompt.

```yaml
repos:
  my-app:
    follows:
      - repo: framework          # its name in the workspace
        min_bump: minor          # patch, minor (the default) or major
        after: published         # or tagged, for a repo with no pipeline on its tags
        lane: chore/{repo}-{major}.{minor}
        scope: [composer.json, composer.lock]
        agent: claude
        task: |
          Move the framework requirement in composer.json to ^{major}.{minor}, run
          composer update for it alone, run the gate, and commit.
```

`lane` and `task` take `{repo}`, `{tag}`, `{version}`, `{major}`, `{minor}`, `{patch}`
and `{previous}`. The first look only records the release the app is on, so turning a
follow on never starts work for releases that were already out. A release skipped by
`min_bump`, a tag whose pipeline is still running, and one whose pipeline failed are each
a line in the thread; a failed release waits until a run of its pipeline passes. Several
releases at once open one lane, for the newest. The run counts against the daily budget,
and pushing follows the app's `autonomy.push` like any other run.

### Conversations you had outside Shepherd

Claude Code sessions you ran by hand in a managed repo (or its worktrees) can be adopted:

```sh
shepherd session import            # every managed repo; --repo R for one
shepherd session list              # id, repo, title, dates, branches
shepherd session attach 71ffa009   # reopen it in Claude Code, in the directory it started in
shepherd session ask 71ffa009 "Is the webhook secret in Vault yet?"
```

Shepherd reads only each session's metadata and a short title; it never changes the
files. `session ask` continues the conversation headless with your question, able to
read but not to edit or run anything, and prints its answer; the desk can do the same
(`session_list`, `session_ask`) from `shepherd chat`. A session whose file changed in
the last five minutes may be open in a terminal, and is not asked.

When an agent asks a lane a question (`ask_shepherd`) and no agent of Shepherd's has
worked in that lane, the question goes to the adopted conversation that worked on the
lane's branch most recently, read-only, and its answer goes back to the agent.

### Agent runs

Shepherd can start an agent headless in a lane's worktree:

```sh
shepherd lane run feat/login --agent claude "Add rate limiting to the login handler, with tests"
shepherd lane run --agent copilot "..."     # from inside the lane's worktree
shepherd run list                            # this lane's runs; --all for every lane
shepherd run show 7                          # state, time, exit code, commits, files outside the scope
shepherd run logs -f 7                       # follow its output
shepherd run continue 7 "the lint job failed: fix it"   # same agent, same conversation
shepherd run attach 7                        # the agent's own CLI, resumed, with you at the keyboard
shepherd run stop 7
```

**A lane keeps its conversation.** Shepherd records each agent's session (Claude Code's
session id, Copilot's resume id, Cursor's chat), so the next `lane run` in a lane with the
same agent continues where it left off, with everything it already knows; `--new`
starts fresh. `run continue` sends a follow-up to a finished run's session, and
`run attach` opens that session interactively in the lane's worktree, where you work
with your own permissions and the push hook still checks the scope.

Agents: `claude` (Claude Code), `copilot` (GitHub Copilot CLI), `cursor` (Cursor's
`cursor-agent`), each with its own login on this machine. The daemon owns the run, so
closing the terminal does not stop it; `lane run` follows the output, and Ctrl-C only
detaches.

Every agent gets the same brief (its lane, branch, scope, the repo's gate) and the
permission mode and autonomy configured for that repo. By default agents cannot push;
when `autonomy.push: agent` is set, the run permits pushing the agent's own lane. The
pre-push hook still checks lane scope. The repo profile's `autonomy.merge` controls whether
the agent can arm its own GitLab merge request; it cannot merge another lane or approve a
request. One agent runs per lane; `daemon.max_runs` (default 4) caps them per machine.
Output is redacted and kept in `~/.shepherd/runs/`.

### Agents ask, you answer

Agents Shepherd starts get three tools of their own, and their brief says when to use
each: `ask_human` for anything that is yours to decide (money, published text, deleting
data, production, a scope or design call), `ask_shepherd` when they need another lane,
and `report` for progress, done or blocked. An agent that asks ends its turn; your
answer goes back into its conversation and it carries on.

```sh
shepherd decision list                 # what agents are waiting on you for, with options and a recommendation
shepherd decision answer 3 2           # pick option 2, or answer in words: "keep the old price"
shepherd run show 12                   # its reports and decisions, with the run's outcome
```

Claude Code and Copilot get the tools by flag; Claude Code sees only Shepherd's worker
tools, so an operator server you registered for yourself never reaches an agent. Cursor
reads MCP servers only from its config file, so it needs one step, once:
`shepherd agents setup cursor` adds a `shepherd-worker` entry to `~/.cursor/mcp.json` and
leaves the rest alone. Cursor does not pass its environment on to the MCP servers it
starts, so its worker finds its run from the lane's worktree; it reaches the daemon in
the default home (`~/.shepherd`), not one chosen with `SHEPHERD_HOME`.

**Agents talk to each other through Shepherd.** `ask_shepherd` sends a question, a
hand-off or a review request to another lane; the asking agent ends its turn, Shepherd
continues the target lane's conversation with it, and the reply (the target's `report`)
comes back into the asker's conversation. A review goes to a different provider than
the author, in a fresh session, and may not change files. A request Shepherd cannot route by rule (no lane named, or a lane with no agent yet)
explains why and waits for the front desk or you. Requests addressed to the person become
decisions; a routed request starts the target agent when its lane is free, or says what is
holding it. A request that has gone stale is closed without a reply with
`shepherd request close <id> --why "..."`, which keeps the reason on the request; a target
agent already working on it is left to finish, but its reply is not carried back. A request
that has already been replied to, failed or closed is refused. An agent out of quota is held until the limit resets. Chains are capped at
three requests.

```sh
shepherd request list                  # requests between lanes, and their replies
shepherd request route 4 --lane docs/api --agent cursor   # route one Shepherd could not
shepherd request close 5 --why "no longer needed"          # close a stale one, keeping the reason
```

### From Claude Code (MCP)

`shepherd mcp` serves Shepherd's operator tools over MCP on stdio: `shepherd_status`,
`shepherd_where`, `lane_list`, `lane_open`, `lane_close`, `lane_ship`, `lane_run`, `run_list`,
`run_status`, `run_continue`, `run_stop`, `decision_list`, `decision_answer`,
`request_list`, `request_route`, `request_close` and `fold_gc`. (`shepherd mcp --worker` is the agents' own set, described above.) Each runs the CLI
command of the same name. Register it once, then start Claude Code at the workspace root
and ask in plain words ("open a lane in myrepo for the login fix, scoped to src/auth"):

```sh
claude mcp add --scope user shepherd -- ~/.local/bin/shepherd mcp   # or wherever the binary is
cd ~/git && claude
```

Any MCP client that can start a stdio server works the same way.

### The daemon

Any command starts the daemon in the background if it is not running, and says so. To
have it start at login and restart if it crashes, register it with the OS service manager
(launchd on macOS, systemd on Linux; Windows to come):

```sh
bin/shepherd daemon install     # writes and loads the LaunchAgent or user unit, with your PATH
bin/shepherd daemon status      # running or not, and whether it starts at login
bin/shepherd daemon stop        # stays stopped until the next login or `daemon start`
bin/shepherd daemon start | restart | uninstall
bin/shepherd daemon reload     # reload config on systems that support SIGHUP
bin/shepherd daemon             # in the foreground, for debugging; Ctrl-C stops it
```

Register the copy `make install` puts in `~/.local/bin`, not `bin/shepherd`: `make clean`
removes the latter, and `make install` restarts the daemon onto each new build.
`install` copies your current PATH into the service, because launchd and systemd start
programs with a bare one that would hide git and the agent CLIs. Set
`SHEPHERD_NO_AUTOSTART=1` to stop commands starting a daemon (scripts, CI).
On systems that support SIGHUP, `shepherd daemon reload` reloads the config without
restarting the daemon. The daemon rotates its log; successful routine reads are logged at
debug level. If it restarts after a crash, it stops leftover agent processes and marks
their runs interrupted.

Shepherd keeps its files in `~/.shepherd` on every OS (or `$SHEPHERD_HOME`): the config,
the SQLite store, the daemon's log, and the running daemon's address and access token. The daemon listens on
loopback only. On its first start it writes `~/.shepherd/config.yaml` with every setting
commented out, so the defaults apply until you change one; it never touches the file
again. A repo's profile comes from that file, over cautious defaults (a human merges, tags
and deploys; agents plan first):

```yaml
defaults:
  gate: make check
repos:
  ubixcore:                 # the repo's path relative to the workspace
    shared_paths: [README.md, .gitlab-ci.yml]
    autonomy: { tag: agent }
  my-app:
    base_branch: dev
    branch_model: promotion
    promotion: [dev, staging, main]
```

Other settings worth knowing: `daemon.log_level` (`debug`, `info`, `warn` or `error`,
applied on reload), `desk.model` (the front desk's model, overridden by `shepherd chat
--model` or `/model`), and `agent.model` per repo (a model per agent, used when `lane run`
names none).

## Branches and releases

Shepherd is developed on an internal forge that push-mirrors its protected branches and
tags to GitHub, so GitHub is the public home but not where work lands.

- **Work lands on `dev`** by merge request, through the gate (`make check` and the
  boundary checks).
- **`main` is the release branch and GitHub's default branch.** It is protected, and it
  moves only by fast-forward from a green `dev`: when the checks pass on a push to `dev`,
  a job on the internal forge promotes that commit to `main` (`bin/promote.sh`). It never
  forces, does nothing if `main` already has the commit, and refuses to promote when
  `main` has diverged from `dev`, which means someone wrote to `main` directly and it has
  to be reconciled by hand.
- **Releases are tags cut on `main`.** GitHub Actions builds, signs and publishes the
  release from the tag, and refuses a tag that is not on `main`. The steps are in
  [docs/RELEASING.md](docs/RELEASING.md).

## The pitch

> **uBixShepherd** is one place to run all my AI agents. Instead of juggling separate
> sessions of Claude, Gemini and whatever else, I talk to Shepherd, and it hands out the
> work, keeps the agents from stepping on each other, and reports back in one thread. It's
> mission control for an AI swarm: I give the direction, it keeps the flock together.

More versions (one-liner, technical) are in [docs/pitch.md](docs/pitch.md).

## Read next

| Doc | What it holds |
|---|---|
| [docs/vision.md](docs/vision.md) | What Shepherd is for, what it does, what it deliberately is not |
| [docs/design.md](docs/design.md) | **The core design**: how Shepherd applies uBixCore's typed-boundary philosophy to make multi-provider agent work deterministic |
| [docs/v1.md](docs/v1.md) | **What v1 builds**: decided scope and stack, the Fold and dispatch around uBixCore, milestones |
| [docs/roadmap.md](docs/roadmap.md) | The MVP feature list by milestone, what follows, nice-to-haves, and the research behind them |
| [docs/naming.md](docs/naming.md) | Why "Shepherd", the names we rejected, and the family vocabulary (Flock, Fold, Crook, Pasture) |
| [docs/origins.md](docs/origins.md) | The practices Shepherd grows out of, already running around uBixCore |
| [docs/pitch.md](docs/pitch.md) | Elevator pitches, ready to paste |
| [docs/open-questions.md](docs/open-questions.md) | Decisions not yet made |
| [CHANGELOG.md](CHANGELOG.md) | What each release changed, and its known limits |
| [docs/VERSIONING.md](docs/VERSIONING.md) | What the version number promises, and what it does not |
| [docs/RELEASING.md](docs/RELEASING.md) | How a release is cut and published, and how to verify a download |
| [CLAUDE.md](CLAUDE.md) | Hand-off notes for an AI session picking this up |

## Licence

BSD 3-Clause. See [LICENSE](LICENSE).
