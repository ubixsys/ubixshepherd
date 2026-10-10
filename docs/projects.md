# Projects: groups of repos with a shared brief, budget and front door

**Status:** Proposed. Nothing here is built. The maintainer approved the design on
2026-10-10 as the basis for the [implementation plan](#implementation-plan), and decided
four calls the same day: one project per repo, how budgets nest and cap, that a project
lists its repos, and a `caches` field (see [Decided](#decided-on-2026-10-10)). The other
[open questions](#open-questions) stay open until the maintainer decides them, and the plan
says which must be settled before which phase. It adds one concept above the repo and
below the workspace, and reuses what exists: repo profiles, `follows`, routed requests and
decisions.

## Decided on 2026-10-10

| Call | Decision |
|---|---|
| One project per repo | A repo is in at most one project. Sharing goes through cross-project requests and `follows`, never dual membership. |
| Budgets | The workspace budget caps the sum of the project budgets. The front desk's spend is its own line against the workspace ceiling. Every cap is `soft` or `hard`; unmarked is `hard`. |
| Membership | A project lists its repos (`repos: [...]`). A repo in no project behaves as today; a repo in two projects is a config error. |
| Caches | A project may list local build caches that are not repos, each made by a recipe in a real repo. Shepherd tells agents where they are and how to rebuild them. |

The sections below state each in place.

## Why

A workspace ([design.md §3.14](design.md#314-workspaces-one-shepherd-over-many-repos)) is a
directory of repos, and today a repo is the only unit with its own rules. People do not
think that way. They think "uBixCore" or "the shop", a handful of repos that share a goal,
a focus this month, a bill, and a way of taking requests. Three gaps follow:

| Gap today | What it costs |
|---|---|
| A repo's `brief` is its standing rules; nothing says what the group is working toward now. | The front desk and each agent rediscover the focus, or work on a stale one. |
| Spend is a single daily figure for the whole machine (`daemon.budget`). | A runaway in one group holds everyone's automatic runs, and nobody can see what a group costs. |
| A repo can follow another repo's releases and can ask a named lane for help. Nothing says how one group asks another for a change. | A consumer's agent either edits a shared framework directly (wrong) or the person relays by hand. |

## What a project is

A **project** is a named group of repos in one workspace. It is configuration plus a little
state in Shepherd's store; it is not a directory, a pack or a product.

- **Members are git repos only, and each is in one project** (decided 2026-10-10). Each
  is already a workspace repo with a profile, and the project lists it. A repo in no
  project behaves as today; a repo listed in two projects is a config error. Sharing
  goes through cross-project requests and `follows`, never through dual membership. A
  possible later extension: a project may list the projects it consumes, for visibility
  only; it would change no routing and no budget.
- **Build caches are not members.** Scratch folders and unrelated working copies stay
  outside Shepherd. When a repo carries its own recipe for them (a `setup` command, a
  lockfile), a fresh worktree reproduces them from it, as today. A cache shared by the
  project's repos can be declared under `caches` (see [Caches](#caches)); that is
  information for agents, not membership.
- **A project holds three things:** a brief, a budget, and intake rules for requests from
  other projects, plus optional cache declarations.
- **A project adds no new way to act.** Lanes, leases, runs, decisions and autonomy stay
  per repo. A project only layers context on top, rolls costs up, and decides which
  requests may start work.
- **Core stays product-free.** "uBixCore is a framework project" is the person's config,
  not code. The core never names a product.

## Config sketch

Projects sit beside `repos:` in the existing config file. Repo profiles are unchanged
except that the project lists its repos (decided 2026-10-10), so a repo's own profile
never claims a project.

```yaml
projects:
  ubixcore:
    repos: [ubixcore, ubixcore-ops]
    brief_max_age: 30d            # show the brief as stale past this; default 30d
    budget:
      amount: 20.00               # dollars per period; default: daemon.budget
      period: day                 # day (default) or month
      cap: hard                   # soft | hard (default hard)
    caches:                       # optional: local build caches that are not repos
      - path: ~/build/llvm-cache
        made_by: ubixcore:tools/build-llvm.sh   # <repo>:<recipe path>, a real repo's recipe
        note: "Toolchain objects; rebuild, do not hand-patch."
    intake:                       # rules for requests from other projects, first match wins
      - from: "*"
        kind: bug
        evidence: [failing_test]  # a request must carry these typed fields
        touches: [internal, tests]
        decide: auto              # open a lane, start an agent
      - from: "*"
        kind: [feature, bug]
        decide: hold              # becomes a decision for the person
      - from: "*"
        kind: notice
        decide: refuse
        why: "changes flow out from this project, not in"
    # no rule matches: hold

  acme-shop:
    repos: [shop-api, shop-web]
    budget: {amount: 10, cap: soft}
    intake:
      - from: ubixcore
        kind: notice              # "this changed, here is what breaks"
        decide: auto

repos:
  shop-api:
    follows:                      # unchanged: a release of ubixcore opens a lane here
      - repo: ubixcore
        min_bump: minor
        task: "Bump the ubixcore pin, run the gate, fix what breaks."
```

The brief is not in this file. It is text with an author, an approval and a date, so it
lives in the store and is edited through the front desk (see below).

## The brief

Today each repo has a `brief` string (`config.Profile.Brief`) added to every agent's
brief in that repo. A project brief is the layer above it: **the goal and the current
focus**, a few lines to a page.

**Layering,** outermost first: workspace pack rules, then the project brief, then the
repo's `brief`. Every agent Shepherd starts in a member repo, and the front desk, gets the
project brief in its standing instruction. A repo's brief wins on a conflict, since it is
the more specific; a conflict is a note for the person, not something an agent resolves.

**Who writes it.** The front desk drafts the brief, and drafts updates when the work has
drifted from it (lanes closed, a milestone met, the focus mentioned in conversation). A
draft is only a draft: **the person approves it**, and only the approved text is given to
agents. This is the same rule as decisions: agents never approve, `decision_answer` takes
the person's words, and a brief approval does too. An agent can suggest a change through
`ask_shepherd`; it cannot edit the brief.

**Staleness is visible.** Each approved brief records `approved_at`. Shepherd shows its age
in `shepherd project show`, in the dock next to the project's name, and in the brief
itself as it is given to agents ("approved 41 days ago"), so a stale focus is a stated
fact to the agent too. Past `brief_max_age` the front desk drafts a refresh and the dock
marks the project; nothing is blocked. Age is wall-clock days since approval, not a guess
at relevance.

## Spend

Run costs are already recorded per run, and a run belongs to a lane and a lane to a repo,
so a project's spend is a sum with no new measurement. The existing machinery
(`internal/dispatch/budget.go`) is the model: a daily dollar figure, a warning at 80%, a
hold on runs Shepherd would start on its own, and never a stop on a run the person started.

**Default:** each project gets the daemon's budget (`daemon.budget`, $20 a day today) as
its own, with the same warning at 80%. With no config at all, a project behaves like the
workspace does now, and nothing new needs choosing. Amounts are left to configuration.

**How budgets nest** (decided 2026-10-10):

- The workspace budget caps the sum of the project budgets. Adding a project does not
  raise it; a configuration whose project budgets sum above the workspace figure is
  allowed, and the workspace ceiling holds first.
- **The front desk's spend is its own line** against the workspace ceiling. It is not
  charged to any project, so a talkative desk cannot starve a project and a project
  cannot hide the desk's cost.
- A run in a repo outside any project counts toward the workspace total only.

**Every cap is marked `soft` or `hard`** (decided 2026-10-10):

| `cap` | At 80% | At 100% |
|---|---|---|
| `soft` | Warns. | Warns again. Never stops work: it only reports. |
| `hard` (default when unmarked) | Warns. | Holds only runs Shepherd starts itself (fixes, routed requests, follow lanes). Never holds a run the person starts. |

This replaces the earlier `hold: warn | automatic | all` design. `hard` is the behaviour
of `daemon.budget` today, so it adds no second meaning of "budget": it is soft toward the
person, who is never blocked from work they typed, and hard toward the unattended loop (a
failed pipeline fixed again and again, a request chain), which is the spend that runs
away. `soft` is for people who want numbers without a hold. The same marking applies to
the workspace ceiling and to the desk's line.

A held run says which budget held it and what lifts it, like a quota hold does. Rollups
(per project, per repo, the desk, per day or month) go into the dock's spend line and
`shepherd project show`.

## Caches

A project may declare **local build caches that are not repos**: a toolchain build, a
vendored object store, anything slow to make and shared by the project's repos. A cache is
information, not a member.

```yaml
caches:
  - path: ~/build/llvm-cache
    made_by: ubixcore:tools/build-llvm.sh
    note: "Toolchain objects; rebuild, do not hand-patch."
```

| Field | Meaning |
|---|---|
| `path` | Where the cache lives on this machine. |
| `made_by` | `<repo>:<recipe path>`: the recipe in a real repo that makes it, so the cache is always reproducible. The repo must exist in the workspace. |
| `note` | A line for agents. |

What Shepherd does: tells every agent in the project where each cache is and how to
rebuild it (the recipe path and its repo), and may show its size and age in `shepherd
project show`. What it never does: open lanes for a cache, commit to it, or count it as a
member. Agents should rebuild a cache from its recipe rather than hand-patch it. A
temporary cache is removed from config when it is no longer needed.

## Feeds between repos

Two repos relate in one of two ways, and Shepherd should not pretend they are one.

**A release feeds a consumer: `follows`.** A repo that depends on another's tagged release
already says so (`Profile.Follows`): the followed repo's published release opens a lane in
the consumer and hands it to an agent, with `min_bump` and `after` deciding which releases
count. This stays exactly as it is, inside a project or across projects (the followed repo
is named in the workspace, not in the project). It is a **pin upgrade the consumer
configures**: a consumer that wants to upgrade when it chooses sets `min_bump: major`, or
unfollows and asks by hand. One small addition worth having, listed under the open
questions: a follow whose lane opens but waits for the person to start it.

**Everything else is a request, not a release.** When one repo's work affects another's
and there is no tag, Shepherd does not invent one. The agent sends a request, as it does
today with `ask_shepherd`, to the other repo's lane. Inside a project that is the existing
routing: deterministic rules start the target agent or explain why they cannot, anything
needing judgment waits for the front desk, a request addressed to the person becomes a
decision, and chains are capped at `MaxDepth`. A request addressed to a repo with no open
lane is `needs_routing` for the desk, as an unrouteable request is today.

## Cross-project work

The example: uBixCore is a shared framework project, and several product monorepos consume
it and contribute to it.

**A consumer does not edit the framework.** An agent in `shop-api` that needs a framework
change is not given the framework's repo to change. Scope leases and the lane's worktree
already keep an agent inside its own repo; the new part is the sanctioned way out, a
**request addressed to a project**.

### The envelope

A cross-project request is a routed request with a project as its target and a few typed
fields, so intake rules have something deterministic to match:

| Field | Meaning |
|---|---|
| `to` | The receiving project (not a lane: the receiver picks the repo and lane). |
| `kind` | `bug`, `feature` or `notice`. `notice` flows from framework to consumers. |
| `summary`, `detail` | What and why, redacted like any agent output. |
| `evidence` | A closed set of attachments the rules can require: `failing_test`, `repro`, `log`. |
| `touches` | What the change reaches: `internal`, `tests`, `docs`, `public_api`. Declared by the asker, checked after the fact (below). |
| `version` | The framework version the consumer is pinned to, read from the consumer's lock, not typed by the agent. |

The state, depth cap and reply path are the existing ones; `to`, the three new kinds and
the typed fields are the proposal.

### The receiver's rules decide

The receiving project's `intake` list (config sketch above) is checked in order, first
match wins, and no match means `hold`:

| Decision | What happens | Example |
|---|---|---|
| `auto` | Shepherd opens a lane in the repo the rule or the desk names and starts an agent with the request as its work order. The asker is continued with the reply when the lane's run ends. | A bug with a failing test attached. |
| `hold` | The request becomes a decision for the person, with the options accept (opens the lane), refuse (with a reason the asker receives) or ask for more. | Anything changing the public API; any feature. |
| `refuse` | The asker is told why at once. | A kind the project does not take. |

Rules match only on typed fields and the sender, never on the prose, so a rule is a plain
condition, not a judgment. Because `touches` and `evidence` come from an agent, an `auto`
lane is opened with **a narrow scope** (the paths the rule's `touches` allows), and the
existing lease and gate machinery does the checking: a change outside that scope is a
lease the agent has to ask for, which becomes a decision, and `lane ship` still runs the
repo's gate. An agent that mislabels a request gets a lane it cannot widen without the
person.

The decision to merge, tag and release is untouched. The receiving repo's `autonomy`
applies as for any lane: an `auto` request can produce a merge request, never a merge the
repo's rules do not already allow, and **releases still go through human tags** (tag
reservations and `autonomy.tag: human`).

### Consumers pin and upgrade when they choose

The framework's fix lands, is released by a person's tag, and reaches a consumer through
that consumer's own pin. Shepherd does not move a pin: the consumer's `follows` lane does,
on the consumer's `min_bump` and `after`, and the consumer's gate decides if it holds. A
request's reply says which framework version carries the fix once there is one, so the
consumer's agent can wait on a follow lane instead of vendoring a patch.

### Change flows back the other way

A framework change that may break consumers is sent the same way in reverse: a `notice`
request addressed to each consuming project ("this changed, here is what breaks, here is
the migration"). Consumers are found from the `follows` of repos in the workspace, so
nobody keeps a second list. Each consumer's intake rules decide, typically `auto` (open a
lane to adapt, behind the consumer's own gate) or `hold`. A `notice` before a release is
advice; the release itself is still the human's tag.

### Relation to typed cross-repo work orders

CLAUDE.md and [v1.md](v1.md) list **typed cross-repo work orders and triage** as not
implemented, and [design.md §3.3](design.md#33-typed-work-orders) describes the work order
schema. This proposal is not that, and is meant to fit under it:

- A **work order** is what an agent is given to do in one repo: goal, scope, gate,
  deliverables.
- A **cross-project request** is how one party asks another to take on work. It is an
  envelope, and it exists before any work order does.
- An accepted request (`auto`, or `hold` then approved) is what **produces** a work order
  in the receiving repo. Until typed work orders exist, the request's fields become the
  lane's task and scope as a prompt does today; when they do, the same fields fill the
  schema, and the request needs no change.
- "Triage" is the intake rules: deterministic matching first, with the front desk only
  for requests that need routing judgment, as in [v1.md](v1.md#dispatch-in-v1).

So the end-to-end cross-repo delivery criterion (a framework change, its tag, a host's
pin bump, with no relaying) is the request plus the release plus the `follows` lane,
and this document supplies the first and last pieces of that chain.

## How it maps onto what exists

| Piece | Today | With projects |
|---|---|---|
| Repo `brief` | A string in the repo's profile, added to each agent's brief. | Unchanged; the project brief sits above it. |
| `follows` | A release of a followed repo opens a lane and starts an agent. | Unchanged; the way a released dependency feeds a consumer, in or across projects. |
| Requests (`internal/dispatch/route.go`) | `question`, `handoff`, `review`, `person`, addressed to a lane; depth capped. | Adds a project as a target, three kinds and typed fields; the same state machine, depth cap and reply path. |
| Decisions | `ask_human`, routed `person` requests, quota holds. | A held request is a decision; so is a brief approval and a widened scope. |
| Autonomy | Per repo: `push`, `merge`, `tag`, `deploy`. | Unchanged. A project grants no autonomy; intake `auto` only starts work. |
| Budget | `daemon.budget`, daily, holds Shepherd's own runs. | Per-project budgets under it, a separate desk line, each cap `soft` or `hard` (default `hard`, today's semantics). |
| Leases and scope | Per repo; workspace-wide still outstanding. | Unchanged. Cross-project writes are never direct, so none are needed. |
| Config | `daemon`, `desk`, `defaults`, `repos`. | Adds `projects` (with `repos`, `budget`, `caches`, `intake`), validated like the rest, reloaded on SIGHUP. |

Changes by package, for whoever builds it: `internal/config` (the `projects` section and
its validation), `internal/store` (brief versions and approvals, a request's project and
typed fields, appended migrations), `internal/dispatch` (spend rollup and hold, intake
matching in the router), `internal/cli` and the MCP mapping (`project` commands first, then
tools), the front desk's `DeskBrief` and the dock. The core-boundary check is unaffected:
nothing in it names a product.

## Open questions

1. **Decided 2026-10-10: one project per repo.** A shared repo (a docs site, a deployment
   repo) is its own project, or sits in none. Sharing goes through cross-project requests
   and `follows`. A possible later extension: a project lists the projects it consumes,
   for visibility.
2. **Decided 2026-10-10: the workspace budget caps the sum of the project budgets,** and
   the front desk's spend is its own line against the workspace ceiling. Every cap is
   `soft` or `hard`; see [Spend](#spend).
3. **One brief per project, but one desk conversation per workspace.** Which brief does
   the desk see? Proposed: all of them in a short index, with the full text of the project
   in whichever lane or repo the turn concerns. Needs a size budget so a dozen projects
   do not crowd the desk's context.
4. **Decided 2026-10-10: the project lists its repos,** so membership is read in one
   place and a repo's profile cannot claim a project that does not want it.
5. **Should a follow be able to wait for the person?** The upgrade-when-I-choose case
   wants a follow lane that opens but does not start an agent until told. Proposed:
   a `start: hold` on `Follow`, defaulting to today's behaviour. Alternative: raise
   `min_bump`.
6. **Where does a `notice` find its consumers?** Proposed: the `follows` graph. It will
   miss a consumer that pins by hand without following. Alternative: an explicit
   `consumers` list on the project, which duplicates `follows` and can drift.
7. **Depth and cost across projects.** A request chain between projects can run past the
   per-chain cap in sum. Proposed: the depth count carries across projects unchanged
   (`MaxDepth`), and a request's runs charge the receiving project, so a sender cannot
   spend another project's budget by asking.
8. **Is `touches` trustworthy enough to match on?** It is declared, so the narrow lane
   scope is the real control. If that proves too loose, the receiving project can set
   `decide: hold` for everything from a given sender and accept the load on the person.
9. **Vocabulary.** "Project" already appears loosely in design.md; if accepted, it
   needs a row in [naming.md](naming.md) and a numbered entry in
   [open-questions.md](open-questions.md). This document does not edit either.

## Implementation plan

Plan-first: this section is the plan, and no phase starts until the maintainer confirms
it. Each phase is one lane and one merge request that `make check` gates, small enough to
review alone and ordered so that every merge leaves a working Shepherd (a project with
nothing configured behaves as the workspace does today). Paths are the lane's scope.

### Assumptions to confirm first

Questions 1, 2 and 4 were decided on 2026-10-10 and the plan builds on those answers (one
project per repo and validated, the workspace budget caps the sum of the projects with the
desk on its own line, the project lists its repos), along with the `soft` or `hard` cap
and `caches`. The phases still use the proposed answers to the rest of the
[open questions](#open-questions): 3 must be decided before phase 6; 5 and 6 can wait for
their phases; 7 and 8 before phase 5.

### Phases

| # | Lane (suggested) | Scope | Depends on | Waits for open lanes |
|---|---|---|---|---|
| 1 | `feat/project-config` | `internal/config/**` | none (decisions 1, 2, 4 made) | **`feat/desk-rotation`** (holds `internal/config/**`) |
| 2 | `feat/project-store` | `internal/store/**` | none | none |
| 3 | `feat/project-spend` | `internal/dispatch/budget*.go`, `internal/store/**` | 1, 2 | `feat/desk-rotation`, through 1 |
| 4 | `feat/project-brief` | `internal/dispatch/runner.go`, `internal/dispatch/adapters.go`, `internal/api/**`, `internal/client/**`, `internal/store/**` | 1, 2 | `feat/desk-rotation`, through 1 |
| 5 | `feat/project-intake` | `internal/dispatch/route.go`, `internal/dispatch/runner.go`, `internal/api/**`, `internal/client/**`, `internal/cli/decision.go`, `internal/store/**` | 1, 2, 3 | `feat/desk-rotation`, through 1 |
| 6 | `feat/project-surfaces` | `internal/cli/project.go`, `internal/cli/mcp.go`, `internal/desk/**`, `internal/chat/**`, `internal/daemon/desk.go`, `internal/webui/**`, `web/src/**`, `docs/desk.md` | 3, 4, 5, decision 3 | **`feat/desk-rotation`** (holds `internal/desk/**`, `internal/daemon/desk.go`, `internal/cli/mcp.go`, `docs/desk.md`) |

Phase 2 can start now. Phases 1 and 6 are the ones the open `feat/desk-rotation` lane
blocks directly, and the rest inherit that through phase 1, since each needs the config
types. `fix/prepush-merge-base` (holds `internal/fold/hook*.go` and `internal/cli/hook*.go`)
overlaps none of them: no phase edits the hook or the fold. Phase 5 opens a lane through
the existing `Fold` calls and leaves the hook alone; if testing it shows the hook's scope
check must change, that change waits for `fix/prepush-merge-base` to land and goes in its
own lane.

Where a phase wants a file another lane holds, the plan moves it rather than asking for
the lease. Two moves are deliberate: the desk's brief injection (`internal/desk/claude.go`,
`internal/chat/desk.go`) and the MCP mapping (`internal/cli/mcp.go`) are in phase 6 and
not earlier, so phases 3 to 5 do not queue behind the desk lane's files.

### Phase 1: config

- **Adds:** a `projects` map in `Config` (`repos`, `brief_max_age`, `budget` with `amount`,
  `period` and `cap`, `caches` with `path`, `made_by` and `note`, `intake` rules as in the
  sketch); a `cap` on the workspace budget and the desk line too; defaults (budget from
  `daemon.budget`, `period: day`, `cap: hard`, `brief_max_age: 30d`, no caches);
  validation: members exist in `repos`, a repo is in at most one project, `cap` is `soft`
  or `hard`, `decide` values are in their set, an intake rule names a known project or
  `*`, durations parse, amounts are not negative, a cache's `made_by` is `<repo>:<path>`
  with a repo in the workspace and a non-empty path. Merge and reload on SIGHUP as the
  rest of config does. Extends `template.yaml`.
- **Tests:** table tests in the style of `config_test.go` for each rejection above and each
  default (including a repo in two projects, an unmarked cap becoming `hard`, a bad
  `cap`, a malformed or unknown-repo `made_by`); reload picks up a changed project;
  `template_test.go` still parses the template.
- **Gate:** `make check`.

### Phase 2: store and migration

- **Adds:** appended migrations only: `project_briefs` (project, text, state of draft or
  approved or superseded, drafted_by, approved_by, created_at, approved_at); a project
  column on `requests` and the typed fields (`kind` values, `evidence`, `touches`,
  `version`, `from_project`) as nullable columns so old rows read unchanged; a project
  column or lookup the spend rollup can use, kept in the store interface as methods, not
  as SQL elsewhere.
- **Tests:** `sqlite_test.go` migrates a database at the previous version and reads old
  requests and spend unchanged; one brief is approved at a time and an approval supersedes
  the last; a draft cannot be approved by the run that drafted it.
- **Gate:** `make check`.

### Phase 3: spend rollup and hold

- **Adds:** a per-project spend total per period (day or month) summed from run costs by
  repo membership; the front desk's spend as its own line against the workspace ceiling,
  charged to no project; the `soft` or `hard` behaviour in the existing budget check: both
  warn at 80%, `soft` only reports at 100%, `hard` holds only runs Shepherd starts itself
  at 100% and never a person's; the held-run message naming the budget and what lifts it;
  the workspace ceiling caps the sum of the projects and holds first when lower. `Spent`
  and `overBudget` keep their signatures for repos outside any project.
- **Tests:** extends `budget_test.go`: a `hard` project at 100% holds Shepherd-started runs
  and not a person's; a `soft` project at 100% holds nothing and warns; both warn at 80%;
  an unmarked cap behaves as `hard`; the desk's spend counts toward the workspace line and
  no project; a repo outside any project counts toward the workspace only; the workspace
  ceiling holds a project still under its own; month rollover. Uses fake runs and the fake
  agent script as the runner's tests do.
- **Gate:** `make check`.

### Phase 4: brief layering, age and caches

- **Adds:** the approved project brief given to every agent Shepherd starts in a member
  repo, layered between workspace rules and the repo `brief` where
  `internal/dispatch/runner.go` and `Brief` in `adapters.go` build the agent's text, with
  its approval age stated in it; draft, approve and read operations on the HTTP API and
  client (approve takes the person's words only, as `decision_answer` does); an `age` and a
  stale flag computed from `approved_at` and `brief_max_age`. Unapproved drafts are never
  given to an agent. The project's `caches` are stated in the same standing text (path,
  recipe and repo, note), so an agent knows to rebuild rather than patch; Shepherd opens no
  lane, commit or count for a cache. The text goes through `internal/redact` like other
  stored agent text.
- **Tests:** a run in a member repo gets the approved brief and not a draft; order is
  workspace, project, repo; a project's caches appear with their recipe and a project
  with none adds nothing; a repo in no project is unchanged; age and stale flag at the
  boundary; an agent run cannot approve (the API refuses a worker's token); API round
  trip in `api_test.go` and `client_test.go`.
- **Gate:** `make check`.

### Phase 5: cross-project requests and intake

- **Adds:** a project as a request target and the `bug`, `feature` and `notice` kinds with
  their typed fields, in `RequestHelp` and `dispatchRequest`; an intake matcher
  (first match, no match is `hold`, matching only typed fields and sender); `auto` opens a
  lane in the rule's repo with a scope limited to the paths `touches` allows and starts an
  agent; `hold` becomes a decision with accept, refuse and ask-for-more options; `refuse`
  replies at once; the consumer's pinned `version` read from its lock where the repo
  records one, else omitted; the depth cap carried across projects and the receiving
  project charged for the runs. `request close` keeps working on these.
- **Tests:** in the style of `route_test.go` with a fake agent: a bug with a failing test
  from another project opens a lane and starts a run; a feature, or anything touching
  `public_api`, becomes a decision; an unmatched request holds; a refused one replies with
  its reason; a request with a mislabeled `touches` cannot write outside its scope;
  `MaxDepth` still bites across projects; the receiver's budget is charged and the
  sender's is not; a request to a repo with no open lane is `needs_routing`.
- **Gate:** `make check`.

### Phase 6: CLI, MCP, desk, dock and web

- **Adds:** `shepherd project` commands first (`list`, `show` with brief, age, spend, caches
  (size and age where the path exists) and intake, `brief draft|approve`), then the MCP tools that map onto them, and the request
  tools' new fields; the desk's standing instruction and operator tools (`internal/desk`
  and `DeskBrief`), including the index of project briefs and the draft-a-refresh
  behaviour; the dock's project name, brief age and spend line; a project page and brief
  approval in `web/`.
- **Tests:** `mcp_test.go` for each tool mapped to its command; `chat` dock tests for the
  new lines and for collapsing on a small terminal; `web` runs `npm run check`
  (typecheck, lint, vitest) as its README does; the desk needs a real check by hand, as
  CLAUDE.md says, before the lane is merged.
- **Gate:** `make check`, and `npm run check` in `web/`.

### Docs follow-up (a later docs lane)

These are outside this lane's scope and are made only when the maintainer decides the
related calls, since a decided question moves out of `open-questions.md` only by the
maintainer:

| File | Entry |
|---|---|
| `docs/naming.md` | A **Project** row in the family vocabulary: a named group of repos in one workspace, holding a brief, a budget and intake rules; not a directory, a pack or a product. Add "intake" if the term sticks. |
| `docs/open-questions.md` | One numbered entry for the project concept (the approval and its date), plus one each for questions 1 to 8 above (1, 2 and 4 are decided, with their date, and the `caches` and `soft`/`hard` cap calls go with them) as they are recorded, keeping stable numbers and linking back here. |
| `docs/design.md` | A short §3.14 note pointing at this document; it stays **Proposed**. |
| `docs/v1.md` and `docs/roadmap.md` | Where the project concept lands among the milestones, and the status of each phase as it merges; the end-to-end cross-repo criterion's status changes only when phase 5 and a real run prove it. |
| `CLAUDE.md` | "Where things stand" and the Code section, once phases merge, not before. |
| `README.md` | The "Read next" table row for this document. |

### What each phase leaves alone

No phase edits the Fold, the pre-push hook, the forge reader, autonomy or release
handling. A project grants no autonomy and no phase moves a tag, a merge or a release off
the person: those stay under the repo's own `autonomy` and tag reservations.
