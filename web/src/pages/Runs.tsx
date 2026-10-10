import { history } from '../api/client'
import type { RunRecord } from '../api/types'
import { EventGlyph } from '../components/Glyph'
import { RangeFilter } from '../components/RangeFilter'
import { clockTime, duration, runCost, usd } from '../format'
import { useNow } from '../hooks'
import { DEFAULT_RANGE, mergeHistories, RANGES, RANGE_PHRASE, rangeStart } from '../model/history'
import { go, href, type Range } from '../router'
import { useHistory } from '../state/history'

/** A run's state as the feed event that reports it, for its glyph. */
const STATE_EVENT: Record<RunRecord['state'], string> = {
  running: 'run_started',
  succeeded: 'run_passed',
  failed: 'run_failed',
  interrupted: 'run_interrupted',
  stopped: 'run_interrupted',
}

/** Every run in the range, with the agent and lane filters and the range's spend. */
export function RunsPage({ range = DEFAULT_RANGE, agent = '', lane = 0 }: { range?: Range; agent?: string; lane?: number }) {
  const now = useNow()
  const { data, error, loading } = useHistory(`${range}:${agent}:${lane}`, async (ids) => {
    const since = rangeStart(range, Date.now()).toISOString()
    return mergeHistories(await Promise.all(ids.map((id) => history.runs(id, since, agent, lane))))
  })
  const label = RANGES.find((r) => r.id === range)?.label ?? ''
  const nav = (o: { range?: Range; agent?: string; lane?: number }) =>
    href.runs({ range, agent: agent || undefined, lane: lane || undefined, ...o })

  const cost = data ? runCost({ cost_usd: data.cost_usd, credits: data.credits }) || usd(0) : ''
  const lanes = data?.lanes ?? []
  const agents = data?.agents ?? []
  // A filter the range does not hold stays a choice, so the select shows what is applied.
  const agentChoices = agent && !agents.includes(agent) ? [agent, ...agents] : agents
  const laneKnown = lane === 0 || lanes.some((l) => l.id === lane)

  return (
    <div className="page page-wide runs-page">
      <header className="page-head">
        <h1>Runs</h1>
        <p className="page-sub runs-total" aria-live="polite">
          {data ? (
            <>
              <strong>{cost}</strong> spent {RANGE_PHRASE[range]}
              <span className="faint">
                {' '}
                across {data.count} {data.count === 1 ? 'run' : 'runs'}
                {(agent || lane > 0) && ' matching the filters'}
              </span>
            </>
          ) : (
            <span className="faint">{error ? 'Could not load runs.' : 'Loading…'}</span>
          )}
        </p>
      </header>

      <div className="filters">
        <RangeFilter range={range} href={(r) => nav({ range: r })} />
        <label>
          <span className="faint">Agent</span>
          <select value={agent} onChange={(e) => go(nav({ agent: e.target.value || undefined }))}>
            <option value="">All agents</option>
            {agentChoices.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span className="faint">Lane</span>
          <select value={lane} onChange={(e) => go(nav({ lane: Number(e.target.value) || undefined }))}>
            <option value={0}>All lanes</option>
            {!laneKnown && <option value={lane}>lane {lane}</option>}
            {lanes.map((l) => (
              <option key={l.id} value={l.id}>
                {l.repo} / {l.name}
              </option>
            ))}
          </select>
        </label>
        {(agent || lane > 0) && (
          <a className="link-button" href={href.runs({ range })}>
            Clear filters
          </a>
        )}
      </div>

      {error && <p className="tone-bad">Could not load runs: {error}</p>}
      {data && data.runs.length === 0 && (
        <p className="faint">{agent || lane > 0 ? 'No run matches these filters' : 'No run started'} in {label.toLowerCase()}.</p>
      )}
      {data && data.runs.length > 0 && (
        <table className="runs history-runs">
          <thead>
            <tr>
              <th scope="col"><span className="visually-hidden">State</span></th>
              <th scope="col">Run</th>
              <th scope="col">Agent</th>
              <th scope="col">Lane</th>
              <th scope="col">Outcome</th>
              <th scope="col" className="num">Commits</th>
              <th scope="col" className="num">Cost</th>
              <th scope="col">Started</th>
              <th scope="col" className="num">Took</th>
              <th scope="col">Task</th>
            </tr>
          </thead>
          <tbody>
            {data.runs.map((r) => (
              <tr key={r.id}>
                <td><EventGlyph event={STATE_EVENT[r.state]} /></td>
                <td><a href={href.log(r.id)} title="Open the log">{r.id}</a></td>
                <td>{r.agent}</td>
                <td className="run-lane">
                  <a href={href.lane(r.lane_id)} title="Open the lane">
                    <span className="faint">{r.repo} /</span> {r.lane}
                  </a>
                </td>
                <td>{r.state}</td>
                <td className="num">{r.commits}</td>
                <td className="num">{runCost(r)}</td>
                <td className="faint" title={r.started}>{clockTime(r.started, now)}</td>
                <td className="num">{duration(r.started, r.ended, now)}</td>
                <td className="run-task" title={r.task}>{r.task}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {data && data.count > data.runs.length && (
        <p className="faint">
          Showing the newest {data.runs.length} of {data.count} runs; the total covers all of them.
        </p>
      )}
      {loading && data && <p className="visually-hidden">Updating…</p>}
    </div>
  )
}
