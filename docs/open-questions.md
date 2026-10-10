# Open questions

These are the maintainer's calls; an agent should bring each one with the trade-offs and a
recommendation, not a bare menu. Numbers are stable: a decided question keeps its number
and records the decision, so links to it stay right.

1. **Scope of a first version.** *Decided 2026-10-01:* the Fold and dispatch together,
   proven on ubixcore, then a host product built on it. See [v1.md](v1.md). (design.md §6 had proposed the
   standards pack first; it now comes after v1.)
2. **Where the human talks to it.** *Decided 2026-10-01:* the terminal first, a web page
   later, and a hosted `shepherd.ubixsys.com` with logins as a future enterprise feature.
   Discord and Slack remain candidates after that.
3. **How agents talk to it.** MCP server exposing Shepherd's tools to each agent is the
   obvious fit for Claude and increasingly for others; a REST API for CI jobs. v1 proposes
   both (operator and worker MCP tool sets over one HTTP API); not yet confirmed.
4. **Language and stack.** *Decided 2026-10-01:* a Go core (one binary per OS: daemon,
   CLI, MCP server, HTTP API) and a TypeScript/React web UI later. Chosen for Windows,
   macOS and Linux from one build and a path to a GUI. Contracts in JSON Schema; webhook
   intake stays in uBixOps.
5. **Who runs the agents.** *Decided 2026-10-01:* Shepherd starts them (dispatch is in
   v1), on the machine it runs on. Containers on the k3s cluster and hosted agents are
   still open.
6. **Secrets.** Provider API keys and tokens belong in uBixVault, never in this repo or in
   prompts. How does Shepherd get scoped credentials to each agent? v1 uses the provider
   CLIs' own logins; Vault leasing is later.
7. **Relationship to uBixOps.** A separate service, or a later uBixOps capability? v1
   proposes a separate service that uBixOps forwards GitLab webhooks to once Shepherd runs
   somewhere reachable.
8. **Open source and licence.** Like the rest of uBixSys, presumably public; confirm
   licence and when it gets a page on `ubixsys-web` (the family convention). Now tied to
   question 12.
9. **uBixFlock.** Whether and when an in-house agent runtime is worth building. Not before
   Shepherd works with third-party agents.
10. **Standards pack format.** Markdown with structured front matter, or schema-first,
    rendered to `CLAUDE.md` / `AGENTS.md` / `GEMINI.md`. And where the uBix pack lives:
    in uBixCore beside the standards it summarises, or here.
11. **Routing policy.** The first routing table ([design.md §3.11](design.md#311-routing-the-right-agent-for-the-work)):
    which task kinds exist, which provider and model each starts on, how many gate failures
    before escalating, and whether a cost budget per task is a hard stop or a warning.
12. **Open core and the enterprise edition.** "Framework-grade, product-free, open
    source" and "an enterprise edition people unlock at `shepherd.ubixsys.com`" can both
    hold under an open-core model (the core free; hosted, multi-user and team features
    paid). Which features sit on which side, and the licence that allows it. Not needed
    for v1.
13. **Adapter protocol.** Drive each provider's CLI directly (JSONL streams, version-pinned)
    or act as an ACP client so one adapter covers many agents. The roadmap starts with the
    CLIs and spikes ACP later ([roadmap.md §5](roadmap.md#5-nice-to-haves)).
14. **Repo autonomy profiles.** The research found merge, tag and deploy rights differ per
    repo (who merges, who tags, who deploys, what needs a plan first). The first profiles
    need the maintainer's confirmation, since they encode what agents may do unasked.
15. **Positioning against GitLab Duo Agent Platform**, which already runs Claude Code and
    Codex inside GitLab: complement it, ignore it, or adapt to it as one more provider.
16. **Which generic packs ship with v1.** The roadmap proposes Go (for the non-uBixCore
    pilot) alongside the uBixCore pack; Node/TypeScript is the next most useful to outside
    users.
17. **The terminal and Shepherd's conversation.** *Decided 2026-10-09:* the front desk
    lives in the daemon, with one conversation per workspace, kept across daemon restarts.
    Terminal chat and the browser are thin clients. The daemon wakes the desk only when a
    client is attached; otherwise it keeps a digest for the next attach, controlled by
    `desk.wake`. This keeps the conversation durable and shared between clients without
    spending on unattended wake-ups. The daemon desk and event stream are built, in review.
    Moving terminal chat onto the daemon desk is also decided, not started. See
    [roadmap.md](roadmap.md#0-status-and-next-work-2026-10-09).
    ([design.md §3.15](design.md#315-shepherds-conversation-orchestrating-agent-systems)
    and [§3.16](design.md#316-the-terminal-one-thread-many-feeds)).
18. **Remote access to a daemon.** *Decided 2026-10-09:* SSH tunnel first. `shepherd
    --host NAME` or `SHEPHERD_HOST` selects a client-side entry in
    `~/.shepherd/hosts.yaml`; the client reads the remote `daemon.json` over SSH and opens
    its own tunnel. The daemon stays loopback-only, avoiding an exposed network listener.
    Direct TCP with TLS is a possible later transport: a generated certificate whose
    fingerprint the client pins, with listening on a LAN interface off by default. The
    SSH-first transport is built, in review.
19. **Agent credential scope and isolation.** *Decided 2026-10-09:* use operator, worker
    and desk tokens now; the desk may answer a decision only in a turn a person started.
    This gives clients distinct capabilities without making the daemon's host a prerequisite
    for an OS sandbox. It is not process isolation: an agent running as the same OS user
    can read the operator token. Separate-account or OS-sandbox isolation is later work.
    Scoped tokens are built, in review.
20. **Web UI delivery.** *Decided 2026-10-09:* keep the shipped v1 board, lane page,
    decisions and run log, then build browser chat, lane story, cost and quality, config
    editor, desktop notifications, and scope and lease map, in that order. A browser client
    already provides the v1 pages with polling. Daemon serving, a one-time sign-in link and
    cookie, `shepherd web`, and missing API fields are not started. The client can be built
    and tested independently; daemon hosting and authentication must come first. See
    [roadmap.md](roadmap.md#0-status-and-next-work-2026-10-09).
21. **Branch model, releases and supported git.** *Decided 2026-10-09:* work lands on
    `dev` by merge request with required human sign-off; green `dev` is fast-forwarded to
    `main` automatically; releases are tagged on `main`; GitHub is the public home and
    release publisher. The first release is `v0.1.0-beta.1`. Human review preserves the
    person's authority over changes, while fast-forward promotion gives releases a
    traceable, gated source. Git 2.31 is the minimum because older versions silently ignore
    the push-block configuration; the daemon enforces this floor at start. The branch and
    release workflow and git check are shipped.
22. **OpenCode and local models.** *Decided 2026-10-09:* an OpenCode adapter belongs in
    Shepherd only for low-risk lanes, with Shepherd's own gate deciding success. Local
    OpenCode models are adequate for small mechanical edits and routing, but not for writing
    tests in an unfamiliar typed codebase. In the observed sample, the best model passed
    five of six small tasks; in a strict typed codebase, none of nine raw drafts passed the
    repository gate. Keeping this adapter out of higher-risk work lets the gate, not the
    model's confidence, determine success. The adapter is not started.
