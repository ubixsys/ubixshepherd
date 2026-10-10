# Design: what Shepherd does, and why it makes agents deterministic

**Status:** Proposed (2026-10-01). Parts of it are now built, and the status column of
[v1.md](v1.md)'s milestones says which; that does not make the rest decided. v1's scope
and stack are decided and live in [v1.md](v1.md); the rest is the thinking to argue with.

## 1. Start from how the work actually happens today

A developer running many agents already has a swarm. It just has no shepherd, so the
developer is the shepherd. From the uBixCore sessions this design grew out of
([origins.md](origins.md)):

| What happens | What it costs |
|---|---|
| Several Claude sessions run at once across a framework, the products built on it, a docs site and a deployment repo, each in its own worktree and lane. | Every session must remember to read `AGENTS-COORD.md`, register, and claim. Claims are honour-system. |
| The human relays state by hand: "203 merged", "is it still going?", pasting CI logs into the chat. | The human is the message bus. The agent idles until they type. |
| The same rules live in a workspace-level `CLAUDE.md`, each repo's `CLAUDE.md`, the standards, and per-user agent memory. | Copies drift. The workspace file already says it: "a copy of these rules is a copy that goes stale." |
| Rules exist only because a mistake happened once: push ≠ merge, local green ≠ pipeline green, "roadmap says Build" ≠ "there is no code", a release that forgot to update its docs site, stacked branches that merged an MR meant to be held back, two lanes taking the same tag (v0.39, v0.43). | Hours lost to each one. Each lesson lives as prose an agent may or may not read next time. |
| A Gemini reviewer runs in CI; everything else is Claude. | Gemini never sees `CLAUDE.md` or Claude's memory. A second provider starts with none of the standards. |
| Sessions run out of context and continue from a summary. | The hand-off is per-provider and per-session; nothing owns it. |
| Some calls are the human's alone (money, pricing, published promises, destructive production actions, approving merges). | Each session works out for itself what is the human's; some ask too much, some too little. |

The agents are good at the work. The overhead is all **coordination, verification and
repeating the rules**, and that is the part Shepherd takes over.

## 2. The uBixCore philosophy, applied to agents

uBixCore does not make user input trustworthy. It makes the **boundary** strict: a
`Payload` validates on the way in, a `DataType` wraps a scalar so an invalid value cannot
exist, an `Enum` closes the set of states, a `Repository` is the only way to the data. The
middle of the program is then predictable, even though its inputs are not.

Agents are the same. An LLM's output cannot be made deterministic. **Everything around it
can be**:

| uBixCore | Shepherd |
|---|---|
| Typed contracts at the boundary (`Payload`, DTO, `DataType`) | Typed **work orders** in, typed **reports** out. No free-text "done". |
| `Enum` closes a set of states | Every task has a closed **state machine**. A task is `merged` only with a merge SHA as proof. |
| The machine gate (phpcs, phpstan, tests) enforces the checkable subset of the standards | Shepherd enforces the checkable subset of the **agent rules**: claims, branch base, tag reservation, reserved decisions. |
| "Push clean, don't MR-loop-clean": a finding class that turns out mechanical is promoted into the gate | A correction an agent keeps needing is promoted from prose into a Shepherd rule. Prose is where rules start, not where they live. |
| Hosts inherit the framework baseline (`<rule ref="Ubix"/>`, `phpstan.neon`, the skeleton) and extend it, never fork it | Repos inherit a **standards pack**; Shepherd renders it for each provider. One source, many outputs, drift-checked. |
| The framework/product boundary test | Shepherd has no idea what product it is working on. The uBix rules are a pack it loads, not code inside it. |
| Secrets come from uBixVault, never `.env` in an image | Agents get scoped, short-lived credentials from uBixVault through Shepherd, never keys in prompts. |
| Decisions are recorded with dispositions (`Fixed:` / `Dismissed:` / `Deferred:`) | Every human decision and every agent outcome is an append-only, machine-readable record. |

The organising rule follows from that:

> **The shepherd is not a sheep.** Shepherd's control plane is ordinary, deterministic
> code: state machines, rules, schemas, webhooks. LLMs do the work; Shepherd decides who
> works on what, checks the result, and knows the state. Where Shepherd itself uses a model
> (summarising, drafting a brief), the output is advisory and goes through the same typed
> boundary as any agent's.

A second rule makes it hold across providers:

> **Enforce at the boundaries every provider must cross.** Claude Code has hooks, others
> have different hooks, some have none. So Shepherd does not rely on any agent obeying a
> prompt. It enforces where every agent has to pass anyway: **git** (hooks, server-side push
> rules), the **MR** (sign-off, threads), **CI** (the gates), **Vault** (credentials), and
> Shepherd's own **API** (claims, reports). An agent that ignores its instructions still
> cannot push to a branch it does not hold, close a task without proof, or reach a secret it
> was not leased.

## 3. What Shepherd does

### 3.1 One thread for the human

The human talks to Shepherd in one place: a terminal, Discord, Slack, or a web page,
undecided. What reaches that thread:

- **Results**, already verified: "shop !405 merged at `3f9c1e07`, dev pipeline 6518 green."
- **Decisions that are yours**, each one held with the trade-offs and a recommendation
  (the "explain before asking to choose" rule, built in).
- **Blockers that need a hand**, such as an owner action in Stripe.
- **Nothing else.** "Still running" and "shall I continue?" are not messages.

### 3.2 Standards, compiled once and rendered per provider

A **standards pack** is the single source for how agents work in a family of repos: house
rules (keep going; stop only for what is the human's; tell the truth; verify state),
repo-specific facts (main branch, gate command, lane protocol), and pointers to the long
standards documents.

Shepherd renders it into whatever each provider reads: `CLAUDE.md`, `AGENTS.md`,
`GEMINI.md`, Cursor rules, a CI reviewer's guide. Like the Ubix phpcs standard, a repo
**inherits** the pack and adds its own section; it does not copy and edit. A gate check
fails when a rendered file differs from its source, so the copies cannot drift.

This alone gives a Gemini or Codex session the same rules a Claude session has, from the
first message.

### 3.3 Typed work orders

Every task an agent gets is a work order, not a chat message. It is the uBixCore
prompt template (`ai-coding-guidelines.md` §2) turned into a schema:

```yaml
id: wo-0142
repo: shop
goal: "Launch discount: 5% for 12 months on launch codes"
scope:                 # paths this task may change; anything else needs a claim
  - php/Shop/Service/Promotion/**
  - tests/Service/Promotion/**
  - docs/surfaces/payments/**
base: dev              # Shepherd checks the branch really starts here
gate: "php bin/ubix code:review"
deliverables: [code, tests, tds-update, roadmap-row]
reserved:              # stop and ask instead of deciding
  - pricing
  - published-copy
done_when: merged      # opened | merged | deployed:<env>
```

The same order goes to Claude, Gemini or a local model; each provider adapter turns it
into that provider's prompt plus the rendered standards.

The prompt never goes on an agent's command line. Anyone on the machine can read a
process's arguments with `ps`, and a `pkill -f` pattern can match an agent through the
words of its task and stop it. Shepherd writes the prompt to the agent's standard input
instead: Claude Code, Copilot and Cursor each read it there when given no prompt argument.
The same holds for the front desk's turns and for questions to an adopted conversation.

### 3.4 Typed reports and a closed task state machine

Agents report through Shepherd's API (exposed to them as MCP tools), and each report is a
state transition that has to carry its proof:

```
queued → claimed → working → gate-green → mr-open → awaiting-approval → merged → deployed
                       ↘ blocked (needs: decision | owner-action | other-task)
                       ↘ failed (with the output)
```

| Transition | Proof Shepherd checks itself |
|---|---|
| → `gate-green` | the gate ran on the current tree (commit SHA matches the branch head) |
| → `mr-open` | the MR exists and targets the right branch |
| → `merged` | GitLab says merged; merge SHA recorded |
| → `deployed:<env>` | the deploy job for that SHA succeeded |

"A push is not a merge" stops being a lesson an agent must remember: Shepherd will not
record `merged` without a merge SHA it fetched from GitLab itself.

### 3.5 The Fold: lanes and claims as a service

`AGENTS-COORD.md` becomes state that Shepherd owns:

- **Lanes** are registered by Shepherd when it hands out a work order, with a distinct
  branch prefix and worktree, created by Shepherd. No agent forgets to register, because
  no agent registers.
- **Every lane records its origin**: the surface it was opened through (the CLI, an
  agent's MCP client, the front desk, an import, a followed release) and, where the
  caller can say, the agent, its run and session, the calling process and the directory.
  A lane with no run still shows who opened it; a lane from before origins were recorded
  shows as unknown, never a guess.
- **Claims are leases.** Shared paths (root `README.md`, `CLAUDE.md`, `Routes.php`,
  `Dependencies.php`, `composer.lock`, `.gitlab-ci.yml`, roadmap rows) are leased to one
  lane at a time. A pre-push hook and a CI check refuse a change to a shared path the lane
  does not hold.
- **Reservations** for anything two lanes can race for: release tags (`v0.44.0`), migration
  timestamps, version numbers. The tag collisions of 2026-09 become impossible.
- **Conflict warnings before work starts**: two work orders whose scopes overlap are
  serialised or flagged at dispatch, not discovered at rebase.

The untracked live file can still be generated as a read-only view for humans.

### 3.6 Events, not relaying

Shepherd subscribes to what already happens: GitLab webhooks (through uBixOps, which
already receives them), pipeline results, approvals, mirror failures. When something
changes, Shepherd moves the task and wakes the agent that is waiting on it. The human
never has to type "203 merged" again.

### 3.7 Reserved decisions: a deterministic gate, with escalation only upward

What is "his" is a rule set, not a mood:

| Reserved class | How Shepherd detects it (examples) |
|---|---|
| money / pricing | paths like pricing templates, promotion services; Stripe live-mode calls |
| published promises | marketing pages, public docs, emails to users |
| destructive production | `Destructive:` migration marker, `kubectl delete` against prod, `data:reset` |
| merge sign-off | always; agents never approve, 👍, or resolve AI-review threads |
| product choice / no-default design | declared by the agent (the only class a model triggers) |

Detection is code (paths, markers, commands). A model may **add** a reason to stop; it
can never remove one. The held decision goes to the human thread with its options and a
recommendation; the answer is recorded and released back to the agent.

Reserved decisions are the line, so an agent's tools need not be narrower than the
person's. By default an agent Shepherd starts has the powers the person's own sessions
have (Claude Code's auto mode under their own settings; `agent.permission_mode` in a repo
profile narrows it to `acceptEdits` or `default`, or widens it to `bypassPermissions`),
plus Shepherd's coordination. What a repo lets agents do beyond that is per repo, stated
in every brief so no agent guesses:

| Setting | What the agent may do |
|---|---|
| `autonomy.push: human` (default) | commit only; its pushes are blocked for the run |
| `autonomy.push: shepherd` | commit only; Shepherd runs the gate and pushes when the run ends |
| `autonomy.push: agent` | push its own lane's branch and open the MR (GitLab push options) |
| `autonomy.merge: agent` | arm merge-when-pipeline-succeeds on its own lane's MR (GitLab) |

The boundaries still hold whichever is set: the pre-push hook checks the branch and scope,
and the forge's approvals, pipelines and threads decide whether a merge happens. No
setting lets an agent approve.

Under `autonomy.push: agent` the hook also tells the daemon which remote git is pushing
to, and a running agent's push goes ahead only to the lane repo's origin, by name or by
its URL. A push to any other remote is refused, and so is one from a hook that names no
remote (an older hook), since the daemon cannot tell where it goes. The hook is the only
check here: the run is not push-blocked, so it holds for an agent that cooperates, not
one that passes `--no-verify` or rewrites its origin.

### 3.8 Playbooks: "missed once already" becomes "cannot be missed"

A playbook is an event plus a condition that produces work orders, like a CI rule:

- A project tags a release **and** has a page on its docs site → work order: update the
  docs; if notable, a second one: the announcement post.
- A change under `.github/workflows/` → check the GitHub mirror synced; work order if it
  failed.
- A Vault secret written for an app → reminder that its web tier needs a rollout restart.
- New uBixCore tag → work order in each host: `composer update ubixsys/ubixcore`, commit the lock.

Each of these is a lesson today written in a `CLAUDE.md` or an agent's memory. As a playbook it
runs every time, for every provider.

### 3.9 Hand-offs and memory, provider-neutral

When a session ends or runs out of context, its last report plus Shepherd's own record of
the task (state, proofs, decisions, open claims) is the hand-off packet. The next agent, on
any provider, starts from that, not from a provider-specific summary. Durable lessons go to
the standards pack or a playbook, where every provider sees them, not to one user's
`~/.claude` memory.

### 3.10 The learning loop

Shepherd records every correction and every review finding by class (the
`Fixed:` / `Dismissed:` / `Deferred:` convention already makes findings machine-readable).
When a class repeats, Shepherd proposes promoting it: a gate check, a Fold rule, a playbook,
or a line in the pack. Promotion is a normal MR the human approves. That is uBixCore's
"push clean" principle, applied to agent behaviour.

### 3.11 Routing: the right agent for the work

A work order says what kind of work it is, not who does it. Shepherd picks the provider and
model from a **rules table**, so the same order routes the same way every time and the
choice can be read and argued with:

```yaml
rules:
  - match: { kind: code-change, size: large }
    route: claude:strongest
    escalate: []                       # already at the top
  - match: { kind: code-change }
    route: claude:standard
    escalate: [claude:strongest]       # after the gate fails twice
  - match: { kind: docs }
    route: gemini:fast
    escalate: [claude:standard]
  - match: { kind: review }
    route: { not_provider_of: author, min_tier: author } # another provider, at least as strong
```

- **Kind, not model, is the input.** The front desk may draft the `kind` from your prompt;
  like any model output it is advisory, schema-checked and shown back.
- **Review needs another provider, at least as strong.** Models catch more of each other's
  bugs than their own, but a weaker reviewer can make a stronger author's code worse
  ([roadmap.md §2](roadmap.md#2-the-kinds-of-work-and-what-proves-each-one-done)).
- **Escalation is a fixed ladder.** A task that fails the gate a set number of times moves
  up the list, never down, and never down on reserved work.
- **Names are aliases.** `claude:strongest` maps to a concrete model in config, because
  models and prices change every few months and the tool has to stay useful verbatim to an
  unrelated company.
- **Evidence changes the table.** Every task records provider, model, kind, first-try gate
  result, rework, time and cost ([v1.md](v1.md#dispatch-in-v1)). When a cheaper route does
  a kind of work as well, Shepherd proposes the change as an MR the human approves. Nothing
  re-routes itself.

**Turning a prompt into a work order** is itself routed. Plain patterns (`review !205`, an
MR link, `release <repo> minor`) become orders by rule, with no model. Otherwise the front
desk drafts the order; where there is no front desk (the CLI, later the web page, Discord or
a labelled issue), a `triage` task goes to the cheapest service the person has that their
repo's data policy allows, with schema-constrained output, temperature 0 and a cache by
prompt hash. Below a confidence threshold it asks rather than guesses. A small local
classifier, trained on outcome records, is an option for offline or air-gapped installs.

```yaml
  - match: { kind: triage }
    route: claude:fast                 # whichever cheap model this person has
    escalate: [claude:standard]
```

A model choosing the model would be more flexible on day one and impossible to predict or
audit. The table starts dumber and gets better from measured outcomes, which is the same
trade uBixCore makes everywhere else.

Until the table exists, models are plain settings. A run uses the model it is started
with, else the one the run it continues used, else the repo profile's `agent.model` for
its agent (a map such as `{claude: sonnet}`, a repo's entries over the defaults'), else
the agent CLI's own default. The front desk uses the chat's `--model` flag, else the
override set with `/model` in the chat, else `desk.model` in `config.yaml`, else Claude
Code's default.

### 3.12 Packs: useful to anyone, best with uBixCore

Shepherd is aimed at uBixCore and built for everyone. The core knows no product, framework
or stack. What it knows about a kind of repo comes from a **pack**: repo profile defaults
(branch model, shared paths, autonomy), gate commands, playbooks and a standards pack.

- **The uBixCore pack** is the flagship and the deepest: `code:review` as the gate,
  `ci:requireApproval` sign-off, uBixOps webhooks, uBixVault leasing, rules from uBixCore's
  own standards, and the framework → tag → host pin-bump chain as a built-in work order
  chain.
- **Generic packs** (Go, Node/TypeScript, Python, plain PHP) give anyone value on day one.
- **No pack at all** still gets lanes, worktrees and leases on the usual shared files
  (lockfiles, CI config, root README).
- `shepherd init` detects the stack and offers the right pack: a `composer.json` requiring
  `ubixsys/ubixcore` gets the uBixCore pack.

A CI check in this repo fails if core code names a product, the same framework boundary
check uBixCore runs on itself.

### 3.13 Forges: primary and mirror

A repo has one **primary** forge, where MRs, CI and sign-off happen, and any number of
**mirrors**. Both GitLab and GitHub implement one forge interface; which role each plays is
per repo. A common setup is GitLab-primary with a GitHub mirror, where the GitHub side
is not passive: a tag that reaches the mirror triggers the workflow that publishes the
image to `ghcr.io`. In the uBix repos each step there has broken once already (a tag arriving with its
workflow file and triggering nothing; a mirror token without `workflow` scope; a package
left private; a history purge that had to reach the mirror's tags too). So the mirror is
part of the proof chain:

```
merged → tagged → mirrored (GitHub has the merge and the tag)
       → published (workflow run succeeded, artefact pullable) → deployed
```

A GitHub-primary repo uses the same interface with pull requests, reviews and branch
protection in place of MRs and `require-approval`.

### 3.14 Workspaces: one Shepherd over many repos

Shepherd does not live in a repo. It runs **one daemon per machine** and works over a
**workspace**: a directory of repos, such as `~/git`. Repos are members of a workspace.

- **`shepherd init ~/git`** finds the git repos below it and detects a pack for each. Repos
  are **opt-in**: Shepherd suggests, the human ticks. A directory of projects usually holds
  scratch repos and deliberate duplicate working copies that should not be managed.
- **The CLI knows where it was run from**, the way git finds `.git`: from the workspace
  root it covers every repo and lane; from inside a repo it defaults to that repo; from a
  lane's worktree, to that lane. Nothing has to run from a particular path.
- **The front desk sits at the workspace root.** Every work order names its repo, so chains
  across repos (a framework change, its tag, the host's pin bump; a release and the docs site
  that describes it) are the normal case.
- **Workspace-level leases and reservations** for what repos share: CI runner capacity, a
  deployment repo several projects release through, a docs site every release updates.
- **Workspace rules.** Rules that span projects (check the coordination state before
  branching; a release updates its docs page) form a workspace-level pack, rendered into
  the workspace root's `CLAUDE.md` / `AGENTS.md` like any other standards pack.
- **Worktrees** default to `<workspace>/<repo>-worktrees/<lane>`, configurable per repo.
- **Clone on demand**: a work order for a member repo that is not cloned yet clones it into
  the workspace first.
- **State lives in Shepherd's store**, not in the workspace directory, which need not be a
  git repo. Per-repo views (the generated `AGENTS-COORD.md`) are still written into each
  repo during the cutover.

A machine can hold several workspaces. When Shepherd is hosted, a workspace becomes a team's
or an organisation's: the same concept, with members and logins.

### 3.15 Shepherd's conversation: orchestrating agent systems

**Status: proposal (2026-10-02), the maintainer's direction, to plan and evolve.**

An agent CLI like Claude Code already orchestrates its own subagents: the human holds one
conversation, the CLI spawns helpers the human never sees, and only their results come
back. Shepherd is that, **one level up**: the one conversation sits above whole agent
systems (Claude Code, Cursor, Copilot, Gemini), each working in its own lane, and they
talk to each other **through Shepherd**. Only what needs the human reaches the human.

- **The voice is an agent; the control plane is not.** The conversation the human holds is
  a front-desk session that Shepherd briefs and runs, on a model the human chooses. Under
  it, Shepherd's deterministic core decides and verifies: lanes, leases, rules, proofs,
  routing ("the shepherd is not a sheep"). To the human it is one conversation, called
  Shepherd.
- **Every lane keeps its agent's conversation.** Agent CLIs can resume a session by id, so
  a lane's agent can be continued (with a follow-up, an event, another agent's question)
  and attached to, rather than started once and forgotten.
- **Three ways out for every agent Shepherd starts**, and the brief says when to use each:

| When | The agent calls | What happens |
|---|---|---|
| It needs another lane: an answer, a change outside its scope, a review | `ask_shepherd` | Shepherd continues the right lane's session with it and returns the answer to the asker's session. The human sees one line. |
| It reaches something reserved for the human (§3.7): money, published copy, destructive actions, a scope or design call | `ask_human` | The question reaches the human's conversation with options and a recommendation; the agent waits. |
| Progress, done, blocked | `report` | Shepherd records it and checks the claim (gate, commits) before believing it. |

  Everything else, it keeps working without asking.
- **Cross-talk is typed, recorded and capped.** A hand-off, a question, a review or a
  notice, never free text into the void. Policy still applies (a hand-off cannot grant a
  scope another lane holds; reserved decisions only escalate up, to the human). Rounds
  per exchange, depth of a chain and budget per work order are limited, so agents cannot
  ping-pong.
- **Events continue sessions instead of the human.** "The MR merged", "the pipeline failed
  on lint", "the framework tagged v0.45, bump the pin": today the human types these into
  the right window. Shepherd sees them (§3.6, §3.13) and continues the right session.

Build order: session continuation for lanes (continue, attach); the three tools for
launched agents with the brief; routing between sessions; then the one conversation
itself, a front-desk session that both the human's messages and the swarm's events
continue (§3.16).

### 3.16 The terminal: one thread, many feeds

**Status: proposal (2026-10-08), revising the 2026-10-02 proposal, to plan and evolve.**
The human wants one terminal to work from, like an agent CLI, that also carries what every
other agent is doing. Two easy shapes both fail the brief: piping every agent's raw output
into one conversation turns the human back into the person watching five sessions, and
tabs to switch between are a terminal multiplexer with the human still the coordinator.

It builds on §3.1 (one thread for the human), §3.6 (events, not relaying), §3.7 (reserved
decisions) and §3.15. The proposal: **one thread in the terminal's own scrollback, a dock
that says what needs the human, and a board to drill in.**

**Why inline.** Agent CLIs have tried both ways. Tools that took over the whole screen and
scrolled it themselves lost what people use the terminal for: the terminal's own search,
selection, copying, multiplexer copy modes, and a record that stays after the program ends.
Several shipped that mode and then added ways back to native scrollback, or turned it off
again. Shepherd keeps the thread in scrollback and owns only a small region at the bottom.
An earlier version with a full-screen side panel was removed for this reason.

```
  › have copilot add tests for the parser
  ● Opened lane test/parser and started copilot (run 12).
  ✓ run 11 claude · feat/login · 2 commits · gate passed
  ✗ !207 fix/crash · pipeline failed (lint) · agent continued, try 1 of 3
  ◆ !214 feat/login merged · lane closed
  ? decision 9 · run 12 copilot · test/parser: OK to change the public API?
  ───────────────────────── live dock, redrawn in place ─────────────────────────
  1 needs you · 2 working · 1 to review · $4.10 of $20 today
  ? 9  copilot  test/parser   OK to change the public API?   waiting 3m   Tab to answer
  ✗ !207 claude fix/crash     pipeline failed, fixing
  ● 12 copilot  test/parser   running 4m
  › _
  Enter send · Tab answer · Ctrl-T board · Ctrl-O transcript · /help
```

Everything above the rule is ordinary terminal output. Everything below it is redrawn.

**Three layers, each with one job.**

| Layer | Where | Shows | Rules |
|---|---|---|---|
| The thread | Printed into the terminal's scrollback | The human's messages, the front desk's replies, and typed swarm events, one line each | Printed once and never rewritten. Wrapped to the width at the time, since scrollback cannot reflow. |
| The dock | A few lines at the bottom, redrawn | Counts by attention state, the items that need the human first, the desk's status and spend, the input | A hard height budget, a fraction of the terminal's rows, collapsing to one status line on small terminals. Never wraps, never reprints the thread. |
| The board | The alternate screen, opened and closed on demand | Every lane and run, grouped by attention, with filter, run logs, diffs, peek and attach | Leaving it returns to the scrollback exactly as it was. Events that arrive meanwhile print on return. |

**The thread carries events, not raw output.** A run started, committed, passed or failed
the gate, asked a question; a pipeline failed and the agent was continued; an MR merged; a
lane closed; the budget crossed a threshold. Each event kind has its own glyph and colour,
and the glyph alone carries the meaning, so the thread reads the same without colour. The
agent's raw output stays in the run log, one key away.

**The dock triages.** Attention is the scarce resource when several agents run at once:
they produce work faster than one person can review it. The dock orders what it shows by
what needs the human, not by when it started:

| Order | State | Example |
|---|---|---|
| 1 | Needs you | A reserved decision waiting, with how long it has waited; a request no rule could route |
| 2 | Broken | A failed pipeline or gate, with the fix attempts used |
| 3 | To review | An MR open and green, waiting for a person |
| 4 | Working | Runs in progress, with their age |
| 5 | Done, unseen | Finished since the human last looked |

A lane's MR shows as a badge coloured by its pipeline and merge state. Every state has a
glyph as well as a colour. The counts at the top of the dock also go to the terminal's
window title, so a tab shows that something is waiting.

**Decisions come to the human, and only the human answers them.** A reserved decision
(§3.7) prints in the thread with its options and recommendation, and appears first in the
dock. One key brings the oldest into focus; a number answers it, or the human types their
own words. The answer is always the person's keystroke: no agent, and not the front desk,
answers on their behalf.

**Signals outside the window.** When the terminal is not focused, a new decision, a failed
run or pipeline, or a request that needs routing raises a terminal notification, once per
item, after a short delay, and not at all if it was handled meanwhile. Where the terminal
supports it, a progress indicator on the tab shows that agents are working or that one has
failed. When the human comes back after a while, one line sums up what happened while they
were away, built from the feed by Shepherd, not written by a model; the desk adds detail
only when asked.

**Drill in, then attach.** From the board, any run's live output opens full screen, a
lane's diff opens in the human's pager, and attaching hands the terminal to that agent's
own CLI, resuming its session, until the human exits back. Shepherd keeps the record either
way. Stopping a run or closing a lane asks for confirmation; reading never does.

**Who the human talks to.** The human is the master coordinator; the **front desk** is
their voice: an ordinary agent session, hosted by the daemon (§3.18; the terminal
chat still hosts its own until it moves onto that one), and wired to Shepherd's operator tools, with read-only access to the workspace. It drafts work orders and
summarises. Shepherd does not run its own model for the conversation or for the dock: the
control plane and everything it displays stay deterministic code ("the shepherd is not a
sheep"), and the front desk can be swapped without changing anything else.

**Accessible and portable.** A plain mode prints the thread and a prompt with no live
region and no animation. Colours adapt to light and dark backgrounds and respect
`NO_COLOR`. Terminal features that are not universal (notifications, tab progress,
hyperlinks, clipboard) are used only where the terminal is recognised, with a plain
fallback.

**Another client of the HTTP API**, like the CLI and the web UI to come, so the same
thread, dock and board can later live in a browser or an editor extension. A standalone
dashboard client can also run in a multiplexer split beside the chat for people who want
the board always visible.

How it could grow, each step useful alone:

| Step | What | Status |
|---|---|---|
| 1 | The thread in scrollback, the front desk, the run log and transcript pagers, decisions answered by command | Built |
| 2 | Typed event lines, an attention-sorted dock with MR badges, the window title, decisions answered by key, adaptive colours | Proposed |
| 3 | Notifications, tab progress and the "while you were away" line | Proposed |
| 4 | The board: lanes and runs grouped by attention, filter, logs, diffs, peek, stop | Proposed |
| 5 | Attach to any run's agent session from the board, for every supported agent CLI | Proposed |

Some of these use terminal features that the current terminal UI library exposes only in
its next major version (synchronized output, tab progress, clipboard, distinct Shift+Enter).
Moving to it is a step of its own, after checking that printing above an inline view and
resizing behave as the thread needs.

### 3.17 The human/agent boundary: scoped tokens

**Status: built (2026-10-09) as a first step; isolation is planned for later.**

Some acts are the person's alone (§3.7): answering a decision, starting or stopping
agents, opening and closing lanes, changing settings. The daemon enforces that at its API,
the boundary every client crosses, with three kinds of token:

| Role | Who holds it | Lives | May call |
|---|---|---|---|
| Operator | The person and their own tools: the CLI, the terminal chat, their MCP server | `daemon.json`, replaced on each daemon start | Every endpoint |
| Worker | One agent Shepherd started, in its environment as `SHEPHERD_TOKEN` | Minted when its run starts; revoked when the run ends or the daemon restarts | What the worker tools need, for its own run: report, ask the person, ask another lane, reserve and release its lane's tags, read its run, the pre-push check |
| Desk | The front desk the daemon runs | Minted for the desk; revoked when the daemon stops | The operator tools, except answering a decision, which it may do only in a turn the person started |

A worker token used on another run's resources, or after its run ended, is refused. The
daemon keeps only a hash of each scoped token, in memory. Clients that run inside an agent
(the worker MCP server, and the operator MCP server when Shepherd starts it) use the token
they were given and never read `daemon.json`. For agent CLIs that start MCP servers
without passing their environment on, the run's token is also written to the lane
worktree's private git directory, never the work tree, and removed when the run ends.

**What this does not do.** Scoped tokens stop accidents and tool misuse: an agent's tools
cannot answer its own question, start another agent or close a lane, and a desk woken by an
event cannot answer for the person. They are not isolation. An agent runs as the same OS
user as the daemon, so it can read `daemon.json` and use the operator token. Real
isolation needs agents under a separate account or an OS sandbox; that is planned for
later.

### 3.18 The front desk in the daemon

**Status: built (2026-10-09); the terminal chat moves onto it next.**

The maintainer decided that the desk lives in the daemon, and that the terminal and the
browser are thin clients of one shared conversation. Before, `shepherd chat` started the
desk itself: one conversation per open terminal, and none at all when no terminal was
open, so the swarm's events waited for the person to come back.

- **One conversation per workspace**, run by the daemon one turn at a time, with a
  bounded queue. A turn is a headless Claude Code session resumed with the person's
  message, or with what Shepherd has to tell it, as §3.16's desk is, with the same
  read-only tools and operator tools. It keeps its own session, never the chat's: two
  processes must not resume one session.
- **Kept in the store.** Every event of the conversation (the person's message, a
  wake-up, the reply, a tool call, the cost, an error, a turn's start and end) has a
  sequence number, the turn it belongs to and that turn's origin, human or system. Clients
  follow it as server-sent events and resume from a sequence number after a dropped
  connection or a daemon restart; a restart closes the turn it cut short. Partial replies
  stream live and are not stored. Old events are trimmed past a cap.
- **Wake-ups move into the daemon.** The kinds that continued the chat's desk on their own
  (a run ended, an agent out of quota, a request needing routing) and a decision waiting
  now continue the daemon's desk, as `desk.wake` allows: while a client is attached and
  briefly after (`attached`, the default), `always`, or `never`. Events that arrive with
  nobody attached wait as one digest, newest first past a bound, for the next client.
- **A wake-up cannot answer for the person.** Each turn gets a desk token (§3.17) that may
  answer a decision only when the person started the turn.
- **Cost per turn.** Claude Code reports a session's total, so a turn records the
  difference from the last total, as spend from `desk`.

## 4. Where the efficiency comes from

| Today | With Shepherd |
|---|---|
| Human relays merges, pipelines and logs between sessions | Webhooks move tasks and wake agents |
| Each session rediscovers the rules, repo by repo, provider by provider | One pack, rendered and drift-checked |
| Collisions found at rebase or after a bad merge | Leases and reservations stop them at dispatch |
| "Done" claims checked by the human | Proof-carrying state transitions |
| Lessons re-learned after each new session | Playbooks and gate rules run every time |
| Each agent asks, or fails to ask, in its own way | One decision queue, one format, one place |
| Context loss means re-explaining | Hand-off packets from Shepherd's own record |
| A second provider starts from zero | Same work order, same pack, same gates |

The measure of success is simple: **messages from the human per merged MR** goes down,
and **rework per merged MR** (re-opened tasks, reverted merges, review findings of a
promoted class) goes toward zero.

## 5. What Shepherd is not

- **Not an agent.** It does not write product code. In-house agents are uBixFlock, later.
- **Not a reviewer of last resort.** Human sign-off stays a human click.
- **Not a replacement for CI.** It consumes gate results; the gates stay in each repo.
- **Not uBix-only.** The uBix rules are one pack. Another company brings its own.

## 6. A phased path (proposal)

> **Superseded for v1 (2026-10-01):** the maintainer chose to build the Fold and dispatch
> together first (phases 2 and 4 below, without Vault leasing), around ubixcore. See
> [v1.md](v1.md). The standards pack (phase 1) comes next after v1. The list below is kept
> as the original reasoning.

Each phase is useful alone, and the first two need no LLM inside Shepherd at all.

1. **Standards pack + renderer + drift check.** Pure files and a CLI. Immediately gives
   every provider the same rules, and retires the hand-copied `CLAUDE.md` sections.
2. **The Fold as a service + MCP tools**: lanes, leases, reservations, typed reports,
   the state machine; git hook and CI check for leases. Existing Claude sessions use it
   right away; the human view replaces `AGENTS-COORD.md`.
3. **Events and the one thread**: GitLab webhooks via uBixOps, proof checks, the human
   thread, the decision queue.
4. **Dispatch**: Shepherd starts agents (Claude Code headless, Gemini CLI, others) from
   work orders, in worktrees it creates, with credentials leased from uBixVault.
5. **Playbooks and the learning loop.**

## 7. Questions this design raises

These feed [open-questions.md](open-questions.md):

- **Stack.** *Decided 2026-10-01:* a **Go** core (one binary per OS: daemon, CLI, MCP
  server, HTTP API; schemas in JSON Schema so any language can speak the contracts) and a
  **TypeScript/React** web UI later, which the Go binary serves locally and which can be
  wrapped as a desktop app or hosted. Chosen for Windows, macOS and Linux support from one
  build and a path to a GUI. Webhook intake stays in uBixOps. See [v1.md](v1.md).
- **Agent adapters.** Driving each provider's CLI headless is the uniform path; provider
  SDKs give richer control but differ per provider. v1 drives the CLIs, and the adapters
  built so far are Claude Code, Copilot and Cursor; Gemini CLI, first proposed here, has
  none yet.
- **Pack format.** Markdown with structured front matter, or a schema-first format that
  renders to Markdown.
- **Where the human thread lives first.** *Decided 2026-10-01:* the terminal, then a web
  page; a hosted `shepherd.ubixsys.com` with logins is a future enterprise feature.
  Discord stays a candidate for later.
