import { useEffect, useMemo, useState } from 'react'
import { api, history } from '../api/client'
import type { LaneRecord, LaneView, RequestView, RunView } from '../api/types'
import { EventGlyph, Glyph } from '../components/Glyph'
import { MrBadge } from '../components/MrBadge'
import { clockTime, duration, runCost, shortSha, since } from '../format'
import { boardItems, GROUP_NAMES } from '../model/board'
import { ending } from '../model/history'
import { aboutLane, type Refs } from '../model/feed'
import { GROUP_MARKS } from '../model/groups'
import { useNow } from '../hooks'
import { href } from '../router'
import { useLive, useSnapshot } from '../state/context'
import { laneSource } from '../conversation/api'
import { Conversation } from '../conversation/Conversation'
import type { ConversationSource } from '../conversation/types'
import '../styles/conversation.css'

/** A run's state as the feed event that reports it, for its glyph. */
export const RUN_EVENT: Record<RunView['state'], string> = {
  running: 'run_started',
  succeeded: 'run_passed',
  failed: 'run_failed',
  interrupted: 'run_interrupted',
  stopped: 'run_interrupted',
}

interface LaneData {
  runs: RunView[]
  decisionRuns: Map<number, number>
  requests: RequestView[]
}

type Tab = 'overview' | 'conversation'

/** `conversation` replaces the daemon's reader; tests give it a fake. */
export function LanePage({ id, conversation }: { id: number; conversation?: (laneId: number) => ConversationSource }) {
  const [tab, setTab] = useState<Tab>('overview')
  const source = useMemo(() => (conversation ?? laneSource)(id), [conversation, id])
  const snap = useSnapshot()
  const live = useLive()
  const now = useNow()
  const [data, setData] = useState<LaneData | null>(null)
  const [error, setError] = useState<string | null>(null)
  // The daemon lists open lanes only; a closed lane is read from its history record, or
  // failing that (an older daemon) known from its runs.
  const listed = snap.lanes.find((l) => l.id === id)
  const [record, setRecord] = useState<LaneRecord | null>(null)
  const unlisted = listed === undefined
  const feedId = snap.feed.at(-1)?.id
  useEffect(() => {
    if (!unlisted) return
    let current = true
    history.lane(id).then(
      (rs) => current && setRecord(rs[0] ?? null),
      () => {},
    )
    return () => {
      current = false
    }
  }, [id, unlisted, feedId])
  const lane: LaneView | undefined = listed ?? record ?? (data?.runs[0] ? closedLane(id, data.runs[0]) : undefined)
  const known = listed ?? record

  // Reload the lane's runs and what the feed's refs point at whenever the feed moves.
  const feedLast = snap.feed.at(-1)?.id ?? 0
  useEffect(() => {
    let current = true
    Promise.all([api.runs(id, 200), api.decisions(''), api.requests('')]).then(
      ([runs, ds, requests]) => {
        if (!current) return
        setData({ runs, decisionRuns: new Map(ds.map((d) => [d.id, d.run_id])), requests })
        setError(null)
      },
      (e: unknown) => current && setError(e instanceof Error ? e.message : String(e)),
    )
    return () => {
      current = false
    }
  }, [id, feedLast])

  // Opening a lane is looking at it: what finished there is no longer news.
  const latest = data?.runs[0]
  useEffect(() => {
    if (latest?.ended) live.markSeen(`run:${latest.id}`)
    if (lane?.closed) live.markSeen(`lane:${lane.id}`)
  }, [live, latest?.id, latest?.ended, lane?.id, lane?.closed])

  const timeline = useMemo(() => {
    if (!lane || !data) return []
    const refs: Refs = { runs: data.runs, decisionRuns: data.decisionRuns, requests: data.requests }
    return snap.feed.filter((it) => aboutLane(it, lane, refs)).reverse()
  }, [snap.feed, lane, data])

  const group = useMemo(() => {
    if (!lane) return undefined
    return boardItems({ ...snap, lanes: [lane], decisions: [], requests: [] })[0]?.group
  }, [snap, lane])

  if (!lane) {
    const known = snap.ready && data
    return (
      <p className="empty">
        {known ? <>No open lane {id}, and no runs to show for it. </> : 'Loading…'}
        {known && <a href={href.board()}>Back to the board</a>}
      </p>
    )
  }

  const runs = data?.runs ?? []
  const decisions = snap.decisions.filter((d) => d.lane === lane.name && d.repo === lane.repo)

  return (
    <div className="page lane">
      <header className="page-head lane-head">
        <h1>
          {group && <Glyph glyph={GROUP_MARKS[group].glyph} tone={GROUP_MARKS[group].tone} label={GROUP_NAMES[group]} />}
          {lane.name}
        </h1>
        <p className="page-sub lane-sub">
          <span>{lane.repo}</span>
          <span>
            <code>{lane.branch}</code>
            {lane.base && <> onto <code>{lane.base}</code></>}
          </span>
          {known && <span>{lane.state === 'closed' ? `closed ${since(lane.closed, now)} ago` : `opened ${since(lane.created, now)} ago`}</span>}
          {record && lane.state === 'closed' && <span title={ending(record).title}>{ending(record).text}</span>}
          {group && <span>{GROUP_NAMES[group]}</span>}
        </p>
      </header>

      {!known && (
        <p className="lane-note faint">
          This lane is closed or not listed by the daemon, so its scope and merge request are not shown; its runs and
          timeline are.
        </p>
      )}

      {decisions.length > 0 && (
        <p className="lane-decisions">
          <Glyph glyph="?" tone="warn" label="decision waiting" />
          <span>
            {decisions.length === 1 ? 'A decision waits on you: ' : `${decisions.length} decisions wait on you: `}
            {decisions.map((d, i) => (
              <span key={d.id}>
                {i > 0 && ', '}
                <a href={href.decisions(d.id)}>{d.question.length > 80 ? d.question.slice(0, 79) + '…' : d.question}</a>
              </span>
            ))}
          </span>
        </p>
      )}

      <div className="lane-tabs" role="tablist" aria-label="Lane views">
        {(['overview', 'conversation'] as const).map((t) => (
          <button
            key={t}
            type="button"
            role="tab"
            id={`lane-tab-${t}`}
            className="lane-tab"
            aria-selected={tab === t}
            aria-controls="lane-panel"
            onClick={() => setTab(t)}
          >
            {t === 'overview' ? 'Overview' : 'Conversation'}
          </button>
        ))}
      </div>

      {tab === 'conversation' && (
        <div id="lane-panel" role="tabpanel" aria-labelledby="lane-tab-conversation">
          <Conversation source={source} refreshKey={feedLast} now={now} />
        </div>
      )}

      <div className="lane-grid" id={tab === 'overview' ? 'lane-panel' : undefined} role={tab === 'overview' ? 'tabpanel' : undefined} aria-labelledby={tab === 'overview' ? 'lane-tab-overview' : undefined} hidden={tab !== 'overview'}>
        <section className="lane-main" aria-labelledby="runs-h">
          <h2 id="runs-h" className="section-h">Runs</h2>
          {error && <p className="tone-bad">Could not load the runs: {error}</p>}
          {data && runs.length === 0 && <p className="faint">No agent has run in this lane yet.</p>}
          {runs.length > 0 && <RunsTable runs={runs} now={now} />}

          <h2 className="section-h">Timeline</h2>
          {timeline.length === 0 ? (
            <p className="faint">Nothing in the recent feed about this lane.</p>
          ) : (
            <ol className="timeline">
              {timeline.map((it) => (
                <li key={it.id}>
                  <EventGlyph event={it.event} />
                  <time dateTime={it.created} title={it.created}>
                    {clockTime(it.created, now)}
                  </time>
                  <span className="timeline-text">{it.text}</span>
                </li>
              ))}
            </ol>
          )}
        </section>

        <aside className="lane-facts" aria-label="Lane details">
          <Forge lane={lane} />
          <dl>
            <dt>Scope</dt>
            <dd>
              <ul className="scope">
                {lane.scope.map((s) => (
                  <li key={s}>
                    <code>{s}</code>
                  </li>
                ))}
              </ul>
            </dd>
            <dt>Commits</dt>
            <dd>
              {runs.some((r) => r.commits > 0) ? (
                <ul className="commits">
                  {runs.filter((r) => r.commits > 0).map((r) => (
                    <li key={r.id}>
                      <code>{shortSha(r.start_sha)}..{shortSha(r.end_sha)}</code> {r.commits} by run {r.id}
                    </li>
                  ))}
                </ul>
              ) : (
                <span className="faint">None from Shepherd's runs</span>
              )}
            </dd>
            <dt>Worktree</dt>
            <dd>
              <code className="path">{lane.worktree}</code>
            </dd>
            <dt>Opened by</dt>
            <dd>{origin(lane)}</dd>
          </dl>
        </aside>
      </div>
    </div>
  )
}

function RunsTable({ runs, now }: { runs: RunView[]; now: number }) {
  return (
    <table className="runs">
      <thead>
        <tr>
          <th scope="col"><span className="visually-hidden">Outcome</span></th>
          <th scope="col">Run</th>
          <th scope="col">Agent</th>
          <th scope="col">Model</th>
          <th scope="col">Outcome</th>
          <th scope="col" className="num">Took</th>
          <th scope="col" className="num">Cost</th>
          <th scope="col">Started</th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.id}>
            <td><EventGlyph event={RUN_EVENT[r.state]} /></td>
            <td><a href={href.log(r.id)}>{r.id}</a>{r.parent ? <span className="faint" title={`continues run ${r.parent}`}> ↰{r.parent}</span> : null}</td>
            <td>{r.agent}</td>
            <td className="faint">{r.model ?? ''}</td>
            <td title={r.error ?? undefined}>
              {r.state}
              {r.exit_code !== undefined && r.exit_code !== 0 && <span className="faint"> (exit {r.exit_code})</span>}
              {r.outside?.length ? <span className="tone-bad" title={r.outside.join(', ')}> outside scope</span> : null}
            </td>
            <td className="num">{duration(r.started, r.ended, now)}</td>
            <td className="num">{runCost(r)}</td>
            <td className="faint" title={r.started}>{clockTime(r.started, now)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function Forge({ lane }: { lane: LaneView }) {
  if (!lane.mr) return <p className="forge faint">No merge request yet.</p>
  const pipelineUrl = pipelineLink(lane)
  return (
    <p className="forge">
      <MrBadge lane={lane} />
      <span className="faint">{lane.mr_state}</span>
      {lane.pipeline ? (
        <span>
          {pipelineUrl ? <a href={pipelineUrl} target="_blank" rel="noreferrer">pipeline {lane.pipeline}</a> : `pipeline ${lane.pipeline}`}{' '}
          <span className="faint">{lane.pipeline_status}</span>
        </span>
      ) : (
        <span className="faint">no pipeline seen</span>
      )}
    </p>
  )
}

/** The daemon gives a pipeline's id but not its URL; on GitLab it sits beside the MR's. */
function pipelineLink(lane: LaneView): string | null {
  if (!lane.mr_url || !lane.pipeline) return null
  const m = /^(.*)\/-\/merge_requests\/\d+$/.exec(lane.mr_url)
  return m ? `${m[1]}/-/pipelines/${lane.pipeline}` : null
}

function closedLane(id: number, r: RunView): LaneView {
  return {
    id, repo_id: 0, name: r.lane, repo: r.repo, branch: r.lane, base: '', worktree: r.worktree,
    scope: [], state: 'closed', created: r.started, origin: {},
  }
}

function origin(l: LaneView): string {
  const o = l.origin
  if (!o.via) return 'unknown'
  const parts = [o.agent, o.run ? `run ${o.run}` : '', o.detail].filter(Boolean)
  return parts.length ? `${o.via} (${parts.join(', ')})` : o.via
}
