# Roadmap: the MVP, what follows, and the nice-to-haves

**Status:** The original feature list is a proposal (2026-10-01). The status and next-work
sequence below record the decisions made on 2026-10-09. Implementation status is checked
against the default branch and the review branches visible on 2026-10-09. It builds on the
decided scope in [v1.md](v1.md) and comes from three pieces of research done the same day:
how the work runs today on uBixCore and the products built on it (coordination logs, agent
working agreements, two months of git history), the current landscape of multi-agent
tools, and which providers and non-LLM tools suit each kind of work. Model names and prices
are a snapshot; the design keeps them behind aliases for that reason.

## 0. Status and next work (2026-10-09)

Status labels are exact: **shipped** means on the default branch, **built, in review**
means committed on a branch but not merged, and **decided** means not started.

| Work | Status | Current position |
|---|---|---|
| Web UI: board, lane page, decisions and run log, using polling | **shipped** | The browser client exists under `web/`; it is not served by the daemon. |
| Minimum git 2.31 check at daemon start | **shipped** | The binary enforces the floor because older git versions silently ignore the push-block configuration. |
| Dev-to-main release flow, required human sign-off, GitHub release publisher | **shipped** | Work lands on `dev`; green `dev` is fast-forwarded to `main`; tags on `main` publish through GitHub. |
| Remote CLI using a named host and SSH tunnel | **built, in review** | The daemon remains loopback-only; the client reads remote runtime details over SSH and opens its tunnel. |
| Daemon-owned front desk, workspace conversation, wake policy and event stream | **built, in review** | Clients will attach to the daemon's conversation; no client attached means it keeps a digest rather than waking the desk. |
| Scoped operator, worker and desk tokens | **built, in review** | The desk can answer a decision only in a turn started by a person. Same-OS-user agents can still read the operator token. |
| Terminal chat as a thin client of the daemon desk, available with `--host` | **decided** | The existing terminal chat still owns its front-desk run; moving it is next after client support. |
| Daemon web serving, one-time sign-in link and cookie, `shepherd web`, missing API fields | **decided** | Not started. The needed fields include lane IDs on feed items, closed lanes in the list, pipeline URLs, commits per run and fix-attempt counts. |
| OpenCode adapter, restricted to low-risk lanes | **decided** | Shepherd's own gate, not the model, decides success. |
| Direct TCP with TLS | **decided** | A possible second transport if SSH tunnelling proves insufficient; it is off by default and would pin a generated certificate's fingerprint. |
| Separate-account or OS-sandbox isolation for agents | **decided** | Later work; scoped tokens do not isolate processes running as the same OS user. |

The first release is `v0.1.0-beta.1`. The development and release branch model is
decided, and its sign-off, promotion and publication workflow is shipped. The supported
git floor is 2.31 and is enforced at daemon start.

### Next work, in order

| Order | Work | Depends on |
|---|---|---|
| 1 | Merge the three finished implementation branches, then install Shepherd. | Human review and merge of the completed branches. |
| 2 | Add a client lane: agents should prefer `SHEPHERD_URL` and `SHEPHERD_TOKEN`; the pre-push hook continues to use the operator token. Add desk and event-stream client methods. | The remote daemon and daemon desk/event-stream work being merged. |
| 3 | Make terminal chat a thin client of the daemon's desk, and allow `chat` with `--host`. | The client lane and the daemon desk API. |
| 4 | Serve the built web app from the daemon and add one-time sign-in with a cookie. Add `shepherd web`. | The daemon's client authentication and web-serving work. |
| 5 | Build the web pages in this order: browser chat, lane story page, cost and quality page, config editor, desktop notifications, scope and lease map. | Daemon web serving, sign-in and the missing API fields. |
| 6 | Add direct TLS transport only if remote-access needs remain after SSH tunnels. | Experience with the SSH-first transport and a concrete need for a second transport. |
| 7 | Isolate agents from the operator token with a separate OS account or an OS sandbox. | A suitable platform-specific isolation mechanism; scoped tokens alone do not provide it. |

### Known gaps and limits

- Desk wake-ups are not per workspace, and queued desk turns do not survive a restart.
- Copilot and Cursor adapters' handling of the run token is unverified.
- Windows builds, but has not been tested.
- GitHub as a lane forge is not built.
- Workspace-wide leases, typed cross-repo work orders and triage are not built.
- A remote client's `SIGKILL` can orphan its SSH tunnel.
- A daemon started inside an agent run inherits that run's environment; a fix is built, in review.
- An agent running as the daemon's OS user can read the operator token. Stronger isolation is later work.

## 1. Lessons from practice, and what they mean for Shepherd

| What running a swarm by hand shows | So Shepherd should |
|---|---|
| Volume is high and much of it is rework: hundreds of commits a month per repo, peaks of 80 a day, dozens of framework tags a month, and fixes making up a quarter to a third of all work. | Be built for throughput and rework, not for occasional tasks. First-try gate pass rate is the metric that matters. |
| Most framework lanes are started from a product's session. The usual shape is a chain: framework MR → tag → host pin-bump MR. | Model **dependent work orders across repos** from the start, not single-repo tasks. |
| What agents may do unasked differs per repo: in one they merge after approval, in another only the human merges, in a third nothing is committed until the human has tested it. | Carry an **autonomy profile per repo** (who merges, who tags, who deploys, what needs a plan first). One global policy would over-ask or under-ask everywhere. |
| The costliest rework is **"done" that was true of an earlier state**: a stale lint cache, a stale application cache, a pod list that looked clean while a scheduled job failed every few minutes. | Run the gate itself (already in v1) and extend proof past `merged`: **`verified-on:<env>`** with evidence such as an endpoint probe, a failed-jobs check, or a browser/E2E run. |
| Product work is not done at merge: a feature is accepted after a real browser pass on a dev environment. | Add a deliverable kind: **a short test script for the human**, and treat their acceptance as a held decision like any other. |
| Topology accidents: stacked branches auto-merged work meant to be held; a merge seconds after a push killed the queued pipeline; tag races; tags cut before the merge landed. | Check the base branch, refuse merges before the head pipeline has run, reserve tags, verify the tag contains the merge. All deterministic. |
| Shared-file churn: a session can burn five rebases on one shared status file. | Leases (in v1), and **Shepherd-owned indexes**: files like `status.md` are generated from Shepherd's records, not hand-edited by every lane. |
| The human is the relay: "203 merged", approve then re-run the sign-off job, design fixes typed into a design tool, browser results. | Events instead of relaying (v1 polls the forges), and **human-relayed providers**: a work order can target a tool with no repo access, such as a design tool; Shepherd drafts the prompt and tracks the result, the human carries it across. |
| Owner queues already exist as hand-kept files, ordered by what each item unblocks, with an effort estimate. | Make the decision queue that same shape: what it unblocks, effort, options, recommendation. |
| Secrets leak through agent output and tooling, not only through code. | **Redact at Shepherd's report boundary** (known secret shapes and Vault paths never stored or shown), and keep Vault leasing on the roadmap. |
| Agents commit under the human's name, and many teams keep agent trailers out of commits. | Shepherd's outcome record is the only place agent attribution lives. Keep it complete. |
| Small teams run few CI runners; one MR can occupy the runner the next lane needs. | The concurrency limit counts **CI capacity**, not just local CPU. |
| Security and optimisation happen only reactively, inside feature lanes. | Add **scheduled audit work** after the MVP (see §4). |
| Working agreements that work: keep going; stop only for what is the human's; explain trade-offs before asking them to choose. | These are the policy the decision queue enforces, and the format every held decision uses. |

## 2. The kinds of work, and what proves each one done

The principle for every kind: **a model does the work, a non-LLM tool or a human decides
whether it is done.** Routes are aliases (`claude:strongest`, …) mapped to models in config;
the mapping as researched today is in [the appendix](#appendix-route-aliases-on-2026-10-01).

| Kind | Starting route → escalation | Ground truth (the gate) | Done means | Human gate |
|---|---|---|---|---|
| **code** | `claude:standard` → `claude:strongest` → `openai:strongest`; large changes start at `claude:strongest` | phpstan max, phpcs, phpunit; tsc, eslint, vitest; `go vet`, golangci-lint, `go test -race` | Gate green on the branch head, diff inside the lease, new tests fail when the change is reverted | Per repo profile |
| **test** | `claude:standard` → `claude:strongest` | Mutation testing (Infection, Stryker, gremlins), coverage | Tests pass on head and fail on the reverted or mutated target; mutation score does not drop | Rarely |
| **plan** | `claude:strongest` | Schema, Shepherd's own overlap check | Every step is a valid work order with kind, scope and gate; scopes do not collide | **Always** |
| **architect** | `claude:frontier`, second opinion from `openai:strongest` | ADR template (MADR), deptrac, dependency-cruiser, go-arch-lint | ADR complete; layer rules written as config that passes on today's code | **Always** |
| **review** | A provider **other than the author's, at least as strong** | Typed findings (file, line, severity, claim) | Every finding points at a real line in the diff; a blocking finding carries a failing check or a reproduction | Blocking findings only |
| **docs** | `gemini:fast` → `claude:standard` | markdownlint, Vale, lychee, codespell, the site build | Linters clean, links resolve, snippets run, versions match the release tag | Public sites: always |
| **graphics** | `openai:image` (raster), `recraft:vector` (SVG) → `gemini:image`; UI design via Claude Design (human-relayed) | Dimensions, format, byte weight, svgo, WCAG contrast, axe/Lighthouse, licence and prompt recorded | Asset meets its spec | **Always** (taste) |
| **optimize** | `claude:strongest` → `openai:strongest` | benchstat, phpbench, xhprof/SPX, pprof, k6 thresholds, Lighthouse budgets, `EXPLAIN` | Before/after on the same hardware, several runs, statistically significant, functional gate still green | Sometimes |
| **security audit** | Scanners first → `claude:strongest` triages → `openai:strongest` verifies adversarially; on refusal, fall back to another provider | Semgrep, **Psalm taint analysis for PHP** (CodeQL has no PHP), gosec, govulncheck, gitleaks, Trivy/Grype + SBOM, `composer audit`, `npm audit`, hadolint, kubescape | Each finding has scanner evidence or a proof of concept, confirmed by a second provider | **Always** (risk acceptance) |
| **migration** | `claude:standard` → `claude:strongest` | up → down → up on a scratch MariaDB, Rector for PHP upgrades, `helm diff` | Reversible, idempotent, data-preserving on a fixture copy; tests green | **Always** for production |
| **release** | `claude:fast` → `claude:standard` | Tag and changelog checks, helm lint, image scan, signing | Tag contains the merge, changelog entry, artefacts built, **ubixsys-web updated in the same pass** | **Always** |
| **research** | `claude:standard` with web → `gemini:pro` | Citation and link check | Every claim cited, every URL resolves, quotes appear in the source | Rarely |

Why these choices, briefly:

- **Cross-provider review is backed by evidence, with a catch.** On 1,000 real PRs each
  model found more bugs in the other's code than in its own, and models miss the bug classes
  they tend to write. But a weaker reviewer made a stronger author's code *worse* in one
  study. Hence "other provider, at least as strong". A review finding is advice; it blocks
  only with a check or a human behind it.
- **LLM security auditors over-report** (60 to 69% of samples flagged vulnerable in a 2026
  benchmark). Scanners first, then triage, then an adversarial second provider, and nothing
  survives without evidence.
- **"Optimised" is most often noise.** The gate is statistics on repeated runs, not a
  model's claim.
- **A test that never fails proves nothing.** Mutation or revert checks are the gate.
- **Effort is a cheaper lever than model.** Routes should carry an effort level
  (`claude:strongest@high`) as well as a model.

## 3. The MVP (v1), milestone by milestone

v1's scope is decided ([v1.md](v1.md)); this is the feature list inside it, with what the
research added marked ★.

**M1 Skeleton.** ★ **Workspaces**: one daemon per machine over `~/git`, opt-in repos, the CLI resolving workspace, repo or lane from where it runs. Go module; `shepherd` builds for Windows, macOS and Linux in CI; daemon,
SQLite store, HTTP API, config loader; `shepherd status`.
- ★ **Repo profiles in config from day one**: base branch, branch model (framework trunk +
  tags, or host `dev` → `staging` → `main`), gate command, shared paths, autonomy (who
  merges, tags, deploys; plan-first or not).
- ★ **Redaction** at every boundary that stores or shows agent output.

**M2 Fold.** Built: per-repo scope leases, lane origin records, worktree open/close/gc,
tag reservation, pre-push hook, generated `AGENTS-COORD.md`, and `fold import`.
Outstanding: workspace-wide leases for the CI runner, deployment repo and docs site.
- ★ **In-agent scope check**: a Claude Code `PreToolUse` hook (and Gemini's equivalent) asks
  Shepherd before a write outside the lane, earlier than the pre-push hook.
- ★ **Per-lane port and env injection**, so parallel dev servers do not collide.
- ★ **Shepherd-owned indexes**: generated status files replace hand-edited shared ones.

**M3 Proofs.** Built: GitLab polling, lane MR and pipeline state, merge-based lane closure
on forge merge-commit proof, checks that reserved tags contain the merge, failed-pipeline
handoff, and the gate runner used before Shepherd ships. The proposed merge discipline
below is not complete.
- ★ **Merge discipline**: base-branch check, `git merge-tree` against the target before an
  MR opens, no merge before the head pipeline has run, rebase remaining lanes in order after
  each merge. (Agent PRs conflict 28% of the time overall, **42% between different agents**
  against 20% within one; a multi-provider swarm needs this more, not less.)
- ★ **Tag proof (built)**: the tag commit contains the merge.
- ★ **`verified-on:<env>`** with pluggable evidence: HTTP probe, failed-Jobs check, E2E run.

**M4 MCP.** Shipped: operator and worker tool sets, plus `shepherd chat` as a terminal
front desk that reads the daemon feed. The daemon-owned desk and event stream are built,
in review. Moving terminal chat onto that desk is decided, not started.
- ★ **Stateless tools with explicit lane and task handles**, matching where the MCP spec is
  heading; `ask` is a plain tool call now, MCP Tasks/elicitation later.
- ★ **The decision queue** in the owner-queue shape: decisions carry options and a
  recommendation. Browser acceptance and the test script for the human live here.

**M5 Dispatch.** Partly built: adapters for Claude Code, Copilot, Cursor and OpenCode, deterministic
request routing, held decisions, outcome records, per-run cost accounting, daily budgets,
quota holds, configurable permission modes, agent push/merge autonomy and opt-in
Shepherd shipping. Typed work orders, cross-repo orders and triage are still outstanding.
- ★ **Adapter contract**, version-pinned with contract tests: parse each CLI's JSONL stream
  into one event model; final report constrained by schema (`--json-schema`,
  `--output-schema`); explicit permission modes, never blanket "bypass" by default. The
  current modes include the person's own mode (`auto` by default).
- ★ **Price table and budget control**: run costs are recorded per run, including the
  increment when an adapter reports a session total. The daily budget holds runs Shepherd
  would start on its own; it does not stop an already running task.
- ★ **Dependent and cross-repo work orders** (framework → tag → host pin bump). The release half is in: a repo `follows` another, and the followed repo's published release opens a lane and an agent run in it. Next: one work order spanning both repos, typed at the front desk.
- ★ **Diff-size cap per work order**, and a cross-provider review **before** the task
  reaches the human, to protect his review time.
- ★ **Concurrency limit that counts CI runners.** The current limit counts agent runs per
  machine, not CI capacity.
- Work kinds in the MVP: **code, test, review, docs, release**. These are most of the
  volume and have the strongest gates. Plan and architect run too, always held for him.

**M6 a host product, and a repo that is not uBixCore.** A host's profile; then `ubixvault` or
`replikate` on the generic Go pack.
- ★ **Packs and `shepherd init`**: the uBixCore pack, the generic Go pack, and the no-pack
  default; a product-noun check on Shepherd's own core.

Across M3 to M5:
- ★ **Two forges from the start**: GitLab primary, GitHub in the mirror role (mirror sync,
  tag-triggered workflow runs, `workflow_dispatch` when a tag triggered nothing, published
  artefact check). GitHub mirror and publication proofs are not implemented.
- ★ **Triage** for prompts with no front desk in front of them, routed to the person's
  cheapest allowed service.
- ★ **Shepherd's conversation** ([design.md §3.15](design.md#315-shepherds-conversation-orchestrating-agent-systems),
  the maintainer's direction): lanes keep their agent's session (continue, attach);
  launched agents get `ask_shepherd`, `ask_human` and `report`; Shepherd routes between
  sessions; events continue sessions instead of the human.
- ★ **The terminal** ([design.md §3.16](design.md#316-the-terminal-one-thread-many-feeds),
  revised 2026-10-08): shipped thread in terminal scrollback, recent history on start,
  searchable Ctrl-O transcript, markdown, typed events from a closed set, attention-sorted
  dock with counts and MR badges, in-place decision answers, and agent attach. External
  notifications and a richer board remain future work.
- ★ **VS Code works as the front desk** from M4 through MCP (VS Code agent mode, Claude
  Code extension); nothing extra to build.

**MVP exit criteria.** A cross-repo order typed at the front desk (a ubixcore change that a
host feature needs) ends with: ubixcore MR merged and tagged, the host's pin-bump MR merged,
`verified-on:dev`, with no relaying by the human, no tag or lease collision, and a
cross-provider review on both MRs. And one release of a non-uBixCore repo (ubixvault or
replikate) goes merged → tagged → mirrored → published with Shepherd checking every step. Measured over two weeks: human messages per merged MR
down, stale worktrees zero, tag collisions zero.

## 4. Further proposals after the decided sequence

These remain proposals, not the immediate work order: GitHub as a primary lane forge;
a VS Code extension; a standards pack rendered to `AGENTS.md`; scheduled audits; server
deployment; Vault credential leasing; release and pin-bump playbooks; more work kinds;
a learning loop; and additional agent adapters. The OpenCode adapter is no longer just a
general proposal: it is decided for low-risk lanes, as recorded above and in
[open-questions.md](open-questions.md).

## 5. Nice-to-haves

- **Adapters through ACP** (the Agent Client Protocol from Zed): one client-side adapter for
  Claude Code, Gemini CLI, Codex and others, with permission prompts arriving as structured
  events. Worth a spike; depends on adapter maturity.
- **Containers as an alternative Pasture** to worktrees: no dependency reinstall per
  worktree, stronger isolation.
- **Remote providers**: Codex cloud, Jules API, Cursor Cloud Agents API.
- **Best-of-n across providers** for chosen kinds; **code-health as a routing input** (one
  2026 paper routes to cheaper tiers on clean code with good results).
- **Signed, hash-chained audit log.**
- **Ticket intake** from GitLab issues, Linear, Jira; **Discord or Slack** for the decision
  queue on the phone.
- **Local models** behind LiteLLM for cheap kinds, kept off the critical path.
- **More generic packs** (Python, plain PHP, Rust) and a way for others to publish packs.
- **JetBrains and Zed** through ACP, once the VS Code extension has proven the client shape.
- **Enterprise edition** at `shepherd.ubixsys.com`: SSO/SCIM, roles, org budgets, audit
  export, dashboards, self-hosted or air-gapped. These are what teams pay for elsewhere.

## 6. The landscape, and where Shepherd stands

Nobody found combines Shepherd's core: **proof-carrying state tied to real merge SHAs with
the gate re-run by the control plane, leases on shared paths, tag reservation, task-level
routing across harnesses driven by measured outcomes, GitLab-first, self-hosted, one binary
on three operating systems.** Most tools are GitHub-centric, a desktop GUI, or tmux-only.

Study before building:

- **Bernstein** (Python, Apache-2.0): the nearest twin in thinking ("no model in the
  coordination loop"), with worktrees, gates, a signed audit log and many CLI adapters. No
  GitLab or MCP. Borrow ideas, especially adapters and audit.
- **GitLab Duo Agent Platform** (GA 2026-01): runs Claude Code and Codex inside GitLab.
  Shepherd's position against it: local and self-hosted, no licence tier, coordination and
  proof rather than agent hosting.
- **GitHub Agent HQ**: the "any vendor's agent with an enterprise control plane" story, on
  GitHub.
- **builderz Mission Control, Emdash**: multi-provider dispatch, dashboards, cost tracking,
  MCP. Check for overlap before building the UI.
- **Gas Town / Beads**: git-backed durable task state, and a cautionary tale (about $100 an
  hour, auto-merged broken tests, a corrupted main). Exactly what proofs and leases prevent.

**Do not build another session manager.** Worktree and tmux session UX is commodity. The
value is the Fold, the proofs and the routing evidence.

## 7. Risks to plan for

- **The human becomes the bottleneck.** Industry data: AI PRs wait longer, are about 2.5
  times larger, and merge far less often. Diff caps, cross-provider review first, and batched
  decisions are the defence.
- **More agents is not always faster.** Practitioners report diminishing returns past two
  to four parallel agents on one codebase. Start the concurrency limit low and raise it on
  evidence.
- **Cost blow-ups.** The budget kill switch is MVP, not a nice-to-have.
- **Provider refusals** on security work (safety classifiers). Treat a refusal as a routing
  outcome and fall back.
- **Free-tier Gemini trains on what it is sent** and fails on rate limits. Provider choice
  depends on repo and data class, which belongs in the repo profile.
- **CLI flags change often.** Version-pin adapters and test them against each CLI release.

## Appendix: route aliases on 2026-10-01

Confidence: high on the principles and on list prices; medium on the ranking between
providers (benchmarks are noisy and several models shipped days ago); low on exact scores.
Expect this table to change within a quarter.

| Alias | Model today | $ per 1M tokens in / out |
|---|---|---|
| `claude:frontier` | Claude Fable 5.1 | 10 / 50 |
| `claude:strongest` | Claude Opus 5.5 | 4 / 20 |
| `claude:standard` | Claude Sonnet 5.5 | 2 / 10 |
| `claude:fast` | Claude Haiku 4.5 | 1 / 5 |
| `openai:strongest` | GPT-6 Astra | 10 / 50 |
| `openai:codex` | GPT-6.1 Sol (Codex CLI default) | 2 / 10 |
| `gemini:pro` | Gemini 3.1 Pro (preview); Gemini 4 Argon when generally available | 2 / 12 |
| `gemini:fast` | Gemini 3.8 Flash (price doubles 2027-01-01) | 0.75 / 3.75 |
| `openai:image` | gpt-image-2.5 | per image |
| `gemini:image` | Gemini 3.1 Flash Image | about $0.045 an image |
| `recraft:vector` | Recraft V4.1 (true SVG) | about $0.035 an image |

Batch tiers cut about half; cached input up to 90%.
