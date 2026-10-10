import { Fragment, useEffect, useMemo, useState, type ReactNode } from 'react'
import { usage as loadUsage } from '../api/client'
import type { UsageGroup, UsageStats, UsageTotals } from '../api/types'
import { ContextChart } from '../components/ContextChart'
import { useNow } from '../hooks'
import { runCost, tokens, usd } from '../format'
import {
  chartSections, DEFAULT_SORT, DEFAULT_USAGE_RANGE, nextSort, sortGroups, USAGE_RANGES, usageDays,
  type Sort, type SortKey,
} from '../model/usage'
import { go, href, type UsageRange } from '../router'
import { useSnapshot } from '../state/context'
import '../styles/usage.css'

/** The money of a set of records: dollars, with Copilot's credits beside them; "$0.00" for none. */
const cost = (t: Pick<UsageTotals, 'cost_usd' | 'credits'>) => runCost(t) || usd(0)

const COLUMNS: { key: SortKey; label: string; title?: string; num?: boolean }[] = [
  { key: 'count', label: 'Runs', num: true, title: 'Records in the group; the ones with token counts are measured' },
  { key: 'input_tokens', label: 'Input', num: true, title: 'Prompt tokens that were not cached' },
  { key: 'cache_read_tokens', label: 'Cache read', num: true, title: 'Prompt tokens served from the cache, counted again on every request' },
  { key: 'cache_creation_tokens', label: 'Cache write', num: true, title: 'Prompt tokens written to the cache' },
  { key: 'output_tokens', label: 'Output', num: true },
  { key: 'peak_context', label: 'Peak context', num: true, title: 'The largest single request’s prompt' },
  { key: 'over_200k', label: '>200k', num: true, title: 'Records whose peak went past 200,000 tokens' },
  { key: 'compactions', label: 'Compactions', num: true, title: 'Times the CLI summarised the conversation to make room' },
  { key: 'cost_usd', label: 'Cost', num: true },
]

function ariaSort(sort: Sort, key: SortKey) {
  return sort.key !== key ? undefined : sort.desc ? ('descending' as const) : ('ascending' as const)
}

function GroupTable({
  caption, nameLabel, groups, sort, onSort, lane,
}: {
  caption: string
  nameLabel: string
  groups: UsageGroup[]
  sort: Sort
  onSort: (k: SortKey) => void
  lane?: boolean
}) {
  const rows = useMemo(() => sortGroups(groups, sort), [groups, sort])
  const head = (key: SortKey, label: string, title?: string, num?: boolean) => (
    <th scope="col" className={num ? 'num' : undefined} aria-sort={ariaSort(sort, key)} title={title}>
      <button type="button" className="sort-button" onClick={() => onSort(key)}>
        {label}
        <span aria-hidden="true">{sort.key === key ? (sort.desc ? ' ↓' : ' ↑') : ''}</span>
      </button>
    </th>
  )
  return (
    <section className="usage-group" aria-label={caption}>
      <h2>{caption}</h2>
      {rows.length === 0 ? (
        <p className="faint">Nothing recorded in this range.</p>
      ) : (
        <table className="runs usage-table">
          <thead>
            <tr>
              {head('name', nameLabel)}
              {COLUMNS.map((c) => <Fragment key={c.key}>{head(c.key, c.label, c.title, c.num)}</Fragment>)}
            </tr>
          </thead>
          <tbody>
            {rows.map((g) => (
              <tr key={g.key}>
                <td className="usage-name" title={g.key}>
                  {lane && g.repo ? <><span className="faint">{g.repo} /</span> {g.lane}</> : g.key}
                </td>
                <td className="num" title={g.measured === g.count ? undefined : `${g.measured} with token counts`}>
                  {g.count}
                  {g.measured < g.count && <span className="faint"> ({g.measured})</span>}
                </td>
                <td className="num">{g.measured ? tokens(g.input_tokens) : '–'}</td>
                <td className="num">{g.measured ? tokens(g.cache_read_tokens) : '–'}</td>
                <td className="num">{g.measured ? tokens(g.cache_creation_tokens) : '–'}</td>
                <td className="num">{g.measured ? tokens(g.output_tokens) : '–'}</td>
                <td className="num">
                  {g.measured ? tokens(g.peak_context) : '–'}
                  {g.context_window ? <span className="faint"> / {tokens(g.context_window)}</span> : null}
                </td>
                <td className={`num ${g.over_200k > 0 ? 'tone-warn' : ''}`}>{g.measured ? g.over_200k : '–'}</td>
                <td className="num">{g.measured ? g.compactions : '–'}</td>
                <td className="num">{cost(g)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

function Tile({ label, value, sub }: { label: string; value: string; sub?: ReactNode }) {
  return (
    <div className="tile">
      <dt>{label}</dt>
      <dd>
        <strong>{value}</strong>
        {sub && <span className="tile-sub">{sub}</span>}
      </dd>
    </div>
  )
}

function Tiles({ s }: { s: UsageStats }) {
  const over = s.runs.over_200k + s.desk.over_200k
  const measured = s.runs.measured + s.desk.measured
  const peak = Math.max(s.runs.peak_context, s.desk.peak_context)
  const share = s.total.cost_usd > 0 ? Math.round((s.desk.cost_usd / s.total.cost_usd) * 100) : 0
  return (
    <dl className="tiles">
      <Tile label="Total cost" value={cost(s.total)} sub={`${s.total.count} records`} />
      <Tile
        label="Desk vs agents"
        value={`${usd(s.desk.cost_usd)} / ${usd(s.runs.cost_usd)}`}
        sub={s.total.cost_usd > 0 ? `the desk is ${share}% of dollars` : 'front desk / agent runs'}
      />
      <Tile
        label="Runs and turns"
        value={`${s.runs.count} / ${s.desk.count}`}
        sub={`agent runs / desk turns, ${measured} measured`}
      />
      <Tile
        label="Over 200k"
        value={String(over)}
        sub={`of ${measured} measured; peak ${tokens(peak)}${over === 0 && measured > 0 ? ', a 200k window held all of it' : ''}`}
      />
    </dl>
  )
}

/** Tokens, context size and cost over a range of days, to tell whether work needs a 1M window. */
export function UsagePage({ range = DEFAULT_USAGE_RANGE, from, to }: { range?: UsageRange; from?: string; to?: string }) {
  const now = useNow(60_000)
  const snap = useSnapshot()
  const feedLast = snap.feed.at(-1)?.id ?? 0
  const days = usageDays(range, now, from, to)
  const key = `${days.from}:${days.to}`
  const [state, setState] = useState<{ key: string; data: UsageStats | null; error: string | null }>({ key, data: null, error: null })
  const [sort, setSort] = useState<Sort>(DEFAULT_SORT)

  // Again when the feed moves: a run ending is a new record.
  useEffect(() => {
    let current = true
    loadUsage(days.from, days.to).then(
      (data) => current && setState({ key, data, error: null }),
      (e: unknown) => current && setState((s) => ({ ...s, key, error: e instanceof Error ? e.message : String(e) })),
    )
    return () => {
      current = false
    }
    // days is named by key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, feedLast])

  // A new range's answer is not the old range's.
  const fresh = state.key === key
  const data = fresh ? state.data : null
  const error = fresh ? state.error : null
  const onSort = (k: SortKey) => setSort((s) => nextSort(s, k))
  const sections = useMemo(
    () => (data ? chartSections(data.by_model ?? [], data.desk_by_model ?? [], data.by_lane ?? []) : []),
    [data],
  )
  const custom = (f: string, t: string) => go(href.usage({ range: 'custom', from: f, to: t }))

  return (
    <div className="page page-wide usage-page">
      <header className="page-head">
        <h1>Usage</h1>
        <p className="page-sub" aria-live="polite">
          {data ? (
            <>
              Tokens and context size, {data.from === data.to ? data.from : `${data.from} to ${data.to}`}. Peak context is the
              measure of window need; cache read counts the same prompt again on every request.
            </>
          ) : (
            <span className="faint">{error ? 'Could not load usage.' : 'Loading…'}</span>
          )}
        </p>
      </header>

      <div className="filters">
        <nav className="range" aria-label="Date range">
          {USAGE_RANGES.map((r) => (
            <a
              key={r.id}
              href={href.usage(r.id === 'custom' ? { range: 'custom', from: days.from, to: days.to } : { range: r.id })}
              aria-current={r.id === range ? 'true' : undefined}
            >
              {r.label}
            </a>
          ))}
        </nav>
        {range === 'custom' && (
          <>
            <label>
              <span className="faint">From</span>
              <input type="date" value={days.from} max={days.to} onChange={(e) => e.target.value && custom(e.target.value, days.to)} />
            </label>
            <label>
              <span className="faint">To</span>
              <input type="date" value={days.to} min={days.from} onChange={(e) => e.target.value && custom(days.from, e.target.value)} />
            </label>
          </>
        )}
      </div>

      {error && <p className="tone-bad">Could not load usage: {error}</p>}
      {data && data.total.count === 0 && <p className="faint">No run or desk turn was recorded in this range.</p>}
      {data && data.total.count > 0 && (
        <>
          <Tiles s={data} />
          <section className="usage-group" aria-label="Peak context">
            <h2>Peak context</h2>
            {sections.length === 0 ? (
              <p className="faint">No record in this range carries token counts.</p>
            ) : (
              <ContextChart sections={sections} />
            )}
          </section>
          <GroupTable caption="By agent and model" nameLabel="Agent and model" groups={data.by_model ?? []} sort={sort} onSort={onSort} />
          <GroupTable caption="Front desk" nameLabel="Model" groups={data.desk_by_model ?? []} sort={sort} onSort={onSort} />
          <GroupTable caption="By lane" nameLabel="Lane" groups={data.by_lane ?? []} sort={sort} onSort={onSort} lane />
        </>
      )}
    </div>
  )
}
