# The front desk's session

The daemon's front desk is one Claude Code session per workspace, resumed headless on
every turn: the person's messages, and Shepherd's wake-ups for the swarm's events. Each
turn rereads the whole session, so a long day of use grows slower and dearer turn by turn,
mostly from long tool results (run logs, request lists) that stay in the context for good.
Two things keep it small: the session is **rotated** once it grows past a threshold, and
what each tool returns to the desk is **capped**.

## Rotation

At the end of a turn, Shepherd checks two measures:

| Measure | From | Setting | Default |
|---|---|---|---|
| Context size | The input and cache tokens of the turn's last model call, as Claude Code reports them | `desk.rotate_tokens` | 150000; 0 never rotates on size |
| Session cost | The session's total cost, as Claude Code reports it | `desk.rotate_cost` (dollars) | unset: never rotates on cost |

When either is passed, the daemon ends the session and the feed gets one `desk_rotated`
item saying why. The next turn starts a new session seeded with a summary. Nothing else
changes for the person: the thread, its history and the streams go on as they were, and
the turns already queued, and the swarm's events waiting for the desk, run in the new
session in their order.

A rotation never happens mid-turn: a turn that was interrupted, failed, or cut short by a
new conversation or the daemon stopping is not measured, and the session is checked
again after the next turn.

Starting a new conversation on purpose (`POST /v1/desk/new`, which a client offers as a
new conversation) takes the same path: its first turn is a new session seeded with the
same summary. Only the very first session of a workspace, with no earlier turn to
continue, starts without one.

Claude Code's own prompt, the brief and the tools come to over 20000 tokens before
anything is said, so `desk.rotate_tokens` is at least 50000.

## The summary

The summary is built by Shepherd from its own records when the new session starts, not
written by a model, and every part of it passes through the same redaction as agent
output. It holds, in this order:

1. Decisions waiting for the person, with options and recommendation.
2. Requests between lanes not yet replied to.
3. Runs in flight, with each one's task and its latest report line.
4. Open lanes, with scope, state and how they were opened.
5. Standing rules: the desk's model, `desk.wake`, and for each repo its base branch,
   autonomy (push, merge, tag, deploy, plan first), permission mode, gate and brief.
6. Decisions the person already answered (the newest five).
7. The last finished runs (five), with outcome, commits and cost.
8. Spend today, by source, against the daily budget.
9. Optionally, a notes paragraph: with `desk.summary_model` set, that model is asked once,
   with no tools and no saved session, for a short paragraph on what the person is after
   and what is still open, from the summary itself. Its cost counts as the desk's. If
   it fails or takes too long, the summary goes without it.
10. The last turns of the thread, verbatim: the message that started each and the desk's
    reply (`desk.summary_turns`, default 6), newest first while they fit.

Each item is cut to a line, each list to twelve items, and the whole summary to
`desk.summary_chars` (default 6000): the records first, then the quoted turns in what room
is left. The summary goes to the new session on standard input before the turn's own
message, never on the command line, and is never stored in the thread. The new session's
brief says that it continues an earlier conversation, what the summary is, and not to
re-derive the facts in it.

What the summary never holds: credentials or tokens (the desk's own token is in its
environment, never in its messages, and anything shaped like a credential in the records
is replaced by `[REDACTED]`), agent output beyond a run's latest report line, tool
results, and the old session's transcript.

## What tools return to the desk

The desk's operator tools run `shepherd mcp --scoped --max-output N`, with N from
`desk.tool_output_chars` (default 4000, at least 500). A tool's output longer than that
keeps its start and its end, with a line in between saying how much was cut and how to
read more: for `run_status`, the run's whole log, redacted, is a file the desk can read
in parts with its Read tool.
MCP clients other than the desk get no cap of their own (only the general one that keeps
the start and end of a very long log).

## Settings

```yaml
desk:
  rotate_tokens: 150000   # 0: never rotate on size; else at least 50000
  # rotate_cost: 5        # dollars for one session; unset: never rotate on cost
  summary_model: ""       # a model for the notes paragraph; empty leaves it out
  summary_chars: 6000     # the summary's cap; at least 1000
  summary_turns: 6        # turns quoted; 0 to 50
  tool_output_chars: 4000 # each tool's output to the desk; at least 500
```

A change applies from the next turn after the daemon reloads its config.
