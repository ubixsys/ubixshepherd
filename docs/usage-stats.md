# Usage stats: tokens and context size

Shepherd records dollars per run and per day (`spend`). Usage stats add what the dollars
do not say: how many tokens each run and each front desk turn used, and above all how
large the context grew. They exist to answer one question with data: **is a 1M-context
model worth it, or does a smaller window do?**

```
shepherd stats                       # the last 7 days
shepherd stats --days 30
shepherd stats --from 2026-10-01 --to 2026-10-07 --json
shepherd run show <id>               # one run's usage line
```

The same numbers are `GET /v1/usage?from=DAY&to=DAY` (a `store.UsageStats`), the
`usage_stats` MCP tool, and the `usage` field of `GET /v1/runs/{id}`. Days are the
daemon's local days, like spend. The stats cover the whole daemon, every workspace.

## What is recorded

One record per agent run (written when the run ends, whatever its outcome) and one per
front desk turn (written when the turn ends).

| Field | Meaning |
|---|---|
| `model` | The model the CLI reported, else the one asked for, else empty (the agent's default). A Claude Code 1M variant reports as `...[1m]`, so it groups apart from the 200k one. |
| `context_window` | The window the model ran with, where the CLI says. Claude Code reports it per model; OpenCode's is the `opencode.context` limit Shepherd configured. |
| `requests` | Model requests the counts were summed over. 0 means the CLI reported no counts. |
| `input_tokens` | Prompt tokens that were not cached. |
| `cache_read_tokens` | Prompt tokens served from the cache. |
| `cache_creation_tokens` | Prompt tokens written to the cache. |
| `output_tokens` | Tokens the model wrote. |
| `peak_context` | The largest single request's prompt: input + cache read + cache creation, taken from each request. |
| `compactions` | Times the CLI summarised the conversation to make room (Claude Code's `compact_boundary`). |
| `cost_usd`, `credits` | What this run or turn added, as the run and spend records have it. |

What each agent reports:

| Agent | Tokens | Peak context | Window | Compactions |
|---|---|---|---|---|
| Claude Code | yes | yes | yes | yes |
| OpenCode | yes, per step | yes (the largest step's prompt) | the configured limit | no |
| Copilot | no | no | no | no |
| Cursor | no | no | no | no |

Copilot and Cursor leave a record with the model and cost only. They count in `RUNS` and
`COST` and not in `MEASURED`, and never in the peak or the over-200k count.

## How to read the output

```
Agent runs: 12 runs (10 with token counts), $14.20
  peak context 412k, 3 over 200k, 2 compaction(s), largest window 1M
  tokens: in 2.1k, cache read 18M, cache write 2.1M, out 310k
```

- **RUNS / TURNS** is every record. **MEASURED** is the records that carry token counts;
  everything else in the row (tokens, peak, over 200k) covers those only.
- **PEAK** is the largest peak of any record in the row. **>200K** counts the records
  whose peak went past 200,000 tokens: the runs or turns a 200k window could not have
  held without compacting.
- **WINDOW** is the largest window reported in the row.
- **CACHE-READ** is usually most of the tokens. It is summed over every request, so it
  counts the same prompt again each time it is re-read: it measures how much was
  processed, not how large the context was. Use PEAK for size.

### The 1M-or-smaller question

Look at the range that matches how you work (a week, a month) and at `>200K` and `PEAK`
by agent and model, by lane, and for the desk.

- **`>200K` is 0 everywhere, or nearly.** Nothing needed more than 200k. A 1M window
  bought nothing in that range: use the smaller model, or keep the 1M one only where
  its price is the same.
- **A few runs or lanes over 200k.** Look at them by lane. If they are one kind of work
  (a large refactor, a whole-repo review), give that work the 1M model by choosing
  `--model` for those runs, and keep the rest small.
- **Many over 200k, or compactions on small windows.** The smaller window is costing
  work: a compaction drops detail mid-run. A 1M window removes the compactions. Compare
  `COMPACT` on the 200k rows with the same work on the 1M rows.
- **Peaks of 1M-window runs cluster below 200k.** The window is bigger than the need.
  The model is being paid for headroom it does not use.
- **Peak near the window.** The run was close to compacting or running out: look at
  `COMPACT` and consider splitting the lane.

The front desk is its own line, and its peak is bounded by configuration: a desk session
is ended at the end of the turn once it passes `desk.rotate_tokens` (150000 by
default; see [desk.md](desk.md)), so a desk peak seldom goes far past that setting. With
the default, a desk with turns over 200k means the threshold was raised, not that the
work needs a big window. If you raised it to let the desk keep more history, the desk's
peak and `>200K` count are the measure of whether that was worth it.

## How it is counted, and where it falls short

- **Per invocation, not per session.** A continued run (`run continue`) and every desk
  turn after the first resume a session, and Claude Code reports the whole session's
  totals in its final result on a resume. Shepherd therefore sums the requests the
  invocation printed (each request once, though Claude Code prints it once per content
  block). On a new session it uses the result's own totals. Output tokens on a
  resumed session are as printed on each request and can read low.
- **The main conversation sets the peak.** A subagent's requests are in the token totals
  and its cost, but not in the peak: a subagent has its own context.
- **A run that failed or was stopped** is recorded with what it printed before it ended.
  A run that printed nothing parseable has `requests` 0.
- **The desk's notes call** (the summary's paragraph written by `desk.summary_model`)
  is in spend but has no usage record, and `shepherd chat`'s own local desk, which runs
  in the terminal rather than in the daemon, records spend but no usage yet.
- **Runs and turns before usage was kept** have no record. They show in spend, not here.
- **Costs** are the dollars the CLI reported; a local model reports 0, and Copilot's are
  credits.

## Where it lives

The `usage` table (an appended migration) holds one row per run or desk turn. The runner
reads usage line by line as the agent prints it (`dispatch.UsageReader`, one reader per
adapter), the desk reads it from its stream, and `store.RollupUsage` totals rows into
`store.UsageStats` for the API, the CLI and the MCP tool.
