# Projects: groups of repos with a shared brief, budget and front door

**Status:** Proposed. Nothing here is built or decided; it is a design to argue with. It adds
one concept above the repo and below the workspace, and reuses what exists: repo profiles,
`follows`, routed requests and decisions.

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

- **Members are git repos only.** Each is already a workspace repo with a profile. Build
  caches, scratch folders and unrelated working copies stay outside Shepherd. When a repo
  carries its own recipe for them (a `setup` command, a lockfile), a fresh worktree
  reproduces them from it, as today.
- **A project holds three things:** a brief, a budget, and intake rules for requests from
  other projects.
- **A project adds no new way to act.** Lanes, leases, runs, decisions and autonomy stay
  per repo. A project only layers context on top, rolls costs up, and decides which
  requests may start work.
- **Core stays product-free.** "uBixCore is a framework project" is the person's config,
  not code. The core never names a product.

## Config sketch

Projects sit beside `repos:` in the existing config file. Repo profiles are unchanged
except that a repo may name its project (or the project lists it; one of the two, see
[open questions](#open-questions)).

```yaml
projects:
  ubixcore:
    repos: [ubixcore, ubixcore-ops]
    brief_max_age: 30d            # show the brief as stale past this; default 30d
    budget:
      amount: 20.00               # dollars per period; default: daemon.budget
      period: day                 # day (default) or month
      hold: automatic             # warn | automatic (default) | all
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
    budget: {amount: 10.00, hold: automatic}
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

**Proposed default:** each project gets the daemon's budget (`daemon.budget`, $20 a day
today) as its own, with the same warning at 80%. The machine-wide figure stays the ceiling.
So with no config at all, a project behaves like the workspace does now, and nothing new
needs choosing.

| `hold` | Warn at 80% | Hold Shepherd's own runs (fixes, routed requests, follow lanes) at 100% | Hold runs the person starts at 100% |
|---|---|---|---|
| `warn` | yes | no | no |
| `automatic` (default) | yes | yes | no |
| `all` | yes | yes | yes, until the person raises the budget or overrides |

**Recommendation: `automatic` as the default, `all` opt-in.** The suggestion was "soft by
default, optional hard cap". Pure soft has one real weakness: the spend that runs away is
the unattended kind, a failed pipeline fixed again and again, or a request chain, and a
warning nobody is watching does nothing about it. Holding only what Shepherd starts on
its own is soft toward the person (they are never blocked from work they typed) and hard
toward the loop. It is also the behaviour of `daemon.budget` today, so it adds no second
meaning of "budget". `warn` exists for people who want numbers without a hold, and `all`
for a project on a metered account.

A held run says which budget held it and what lifts it, like a quota hold does. Rollups
(per project, per repo, per day or month) go into the dock's spend line and
`shepherd project show`. Runs in a repo outside any project count toward the workspace
total only.

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
| Budget | `daemon.budget`, daily, holds Shepherd's own runs. | Per-project budgets under it, same semantics, `hold` chooses how hard. |
| Leases and scope | Per repo; workspace-wide still outstanding. | Unchanged. Cross-project writes are never direct, so none are needed. |
| Config | `daemon`, `desk`, `defaults`, `repos`. | Adds `projects`, validated like the rest, reloaded on SIGHUP. |

Changes by package, for whoever builds it: `internal/config` (the `projects` section and
its validation), `internal/store` (brief versions and approvals, a request's project and
typed fields, appended migrations), `internal/dispatch` (spend rollup and hold, intake
matching in the router), `internal/cli` and the MCP mapping (`project` commands first, then
tools), the front desk's `DeskBrief` and the dock. The core-boundary check is unaffected:
nothing in it names a product.

## Open questions

1. **Can a repo be in two projects?** Proposed: no, one project per repo. A shared repo
   (a docs site, a deployment repo) is then its own project, or sits in none. Two
   projects would make the brief ambiguous and spend double-counted, and the rule is easy
   to relax later and hard to tighten. The cost: a repo that really serves two groups
   has no good home.
2. **Is the workspace budget the sum?** Proposed: no. The workspace figure stays its own
   ceiling and projects draw under it; a sum of project budgets above it is allowed and
   the lower one holds first. Computing the workspace budget as the sum would silently
   raise it whenever someone adds a project.
3. **One brief per project, but one desk conversation per workspace.** Which brief does
   the desk see? Proposed: all of them in a short index, with the full text of the project
   in whichever lane or repo the turn concerns. Needs a size budget so a dozen projects
   do not crowd the desk's context.
4. **Does a repo name its project, or the project list its repos?** Proposed: the project
   lists them, as in the sketch, so membership is read in one place. A repo's own profile
   cannot then claim a project that does not want it.
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
