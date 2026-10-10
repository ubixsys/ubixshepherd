import { useEffect, useMemo, useRef, useState } from 'react'
import { history } from '../api/client'
import type { LaneRecord } from '../api/types'
import { RangeFilter } from '../components/RangeFilter'
import { DEFAULT_RANGE, ending, finished, RANGE_PHRASE, rangeStart } from '../model/history'
import { useHistory } from '../state/history'
import type { Range } from '../router'
import { GROUP_NAMES, GROUPS, type BoardItem, type Group } from '../model/board'
import { GROUP_MARKS } from '../model/groups'
import { Glyph } from '../components/Glyph'
import { MrBadge } from '../components/MrBadge'
import { runCost, since, usd } from '../format'
import { useKeys, useNow } from '../hooks'
import { go, href } from '../router'
import { useLive, useSnapshot } from '../state/context'

/** Where a row leads: a decision to its answer, a lane to its page. */
function target(it: BoardItem): string | null {
  if (it.decision) return href.decisions(it.decision.id)
  if (it.request) return href.decisions()
  if (it.laneId !== undefined) return href.lane(it.laneId)
  return null
}

export function BoardPage({ items, range = DEFAULT_RANGE }: { items: BoardItem[]; range?: Range }) {
  const snap = useSnapshot()
  const live = useLive()
  const now = useNow()
  const [sel, setSel] = useState(0)
  const rows = useRef<(HTMLAnchorElement | HTMLDivElement | null)[]>([])

  const closed = useHistory(range, async (ids) => {
    const since0 = rangeStart(range, Date.now()).toISOString()
    return (await Promise.all(ids.map((id) => history.lanes(id, since0)))).flat()
  })
  const done = useMemo(() => finished(closed.data ?? [], rangeStart(range, now)), [closed.data, range, now])
  const total = items.length + done.length

  const clamped = Math.min(sel, Math.max(0, total - 1))
  useEffect(() => {
    rows.current[clamped]?.focus({ preventScroll: false })
  }, [clamped])

  useKeys(
    {
      j: () => setSel((s) => Math.min(s + 1, total - 1)),
      ArrowDown: (e) => (e.preventDefault(), setSel((s) => Math.min(s + 1, total - 1))),
      k: () => setSel((s) => Math.max(s - 1, 0)),
      ArrowUp: (e) => (e.preventDefault(), setSel((s) => Math.max(s - 1, 0))),
      c: () => live.clearDone(),
    },
    [total, live],
  )

  if (!snap.ready) {
    return <p className="empty">{snap.error ? 'Waiting for the daemon.' : 'Loading lanes…'}</p>
  }

  const byGroup = new Map<Group, BoardItem[]>()
  for (const it of items) byGroup.set(it.group, [...(byGroup.get(it.group) ?? []), it])
  let index = -1

  return (
    <div className="board">
      {items.length === 0 && (
        <div className="empty">
          <p>Nothing needs you, nothing is running, and nothing finished since you last looked.</p>
          <p className="faint">
            Start work from the terminal: <code>shepherd lane run &lt;lane&gt; --agent claude</code>.
          </p>
        </div>
      )}
      {GROUPS.flatMap((g) => {
        const list = byGroup.get(g)
        return list ? [{ g, list }] : []
      }).map(({ g, list }) => (
        <section key={g} className={`group group-${g}`} aria-labelledby={`group-${g}`}>
          <h2 id={`group-${g}`} className="group-head">
            <Glyph glyph={GROUP_MARKS[g].glyph} tone={GROUP_MARKS[g].tone} label="" />
            <span className="group-name">{GROUP_NAMES[g]}</span>
            <span className="group-count">{list.length}</span>
            {g === 'done' && (
              <button type="button" className="link-button" onClick={() => live.clearDone()}>
                Mark all seen <kbd>c</kbd>
              </button>
            )}
          </h2>
          <ul className="rows">
            {list.map((it) => {
              const i = ++index
              const to = target(it)
              const mark = GROUP_MARKS[it.group]
              const body = (
                <>
                  <Glyph glyph={it.glyph ?? mark.glyph} tone={mark.tone} label={GROUP_NAMES[it.group]} />
                  <span className="row-lane">{it.name}</span>
                  <span className="row-agent">{it.agent ?? ''}</span>
                  <span className="row-repo">{it.repo}</span>
                  <span className="row-mr">{it.lane && <MrBadge lane={it.lane} link={false} />}</span>
                  <span className="row-detail">{it.detail}</span>
                  <span className="row-age" title={it.at}>
                    {since(it.at, now)}
                  </span>
                </>
              )
              return (
                <li key={it.key}>
                  {to ? (
                    <a
                      ref={(el) => {
                        rows.current[i] = el
                      }}
                      className="row"
                      href={to}
                      aria-current={i === clamped ? 'true' : undefined}
                      onFocus={() => setSel(i)}
                      onClick={(e) => {
                        e.preventDefault()
                        go(to)
                      }}
                    >
                      {body}
                    </a>
                  ) : (
                    <div
                      ref={(el) => {
                        rows.current[i] = el
                      }}
                      className="row"
                      tabIndex={0}
                      onFocus={() => setSel(i)}
                    >
                      {body}
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        </section>
      ))}
      <FinishedSection
        lanes={done}
        range={range}
        loading={closed.loading && closed.data === null}
        error={closed.error}
        offset={items.length}
        now={now}
        selected={clamped}
        onSelect={setSel}
        rows={rows}
      />
    </div>
  )
}

/** Closed lanes, newest first: what ran and how it ended. Rows go on after the board's own. */
function FinishedSection({
  lanes, range, loading, error, offset, now, selected, onSelect, rows,
}: {
  lanes: LaneRecord[]
  range: Range
  loading: boolean
  error: string | null
  offset: number
  now: number
  selected: number
  onSelect: (i: number) => void
  rows: React.MutableRefObject<(HTMLAnchorElement | HTMLDivElement | null)[]>
}) {
  const phrase = RANGE_PHRASE[range]
  return (
    <section className="group group-finished" aria-labelledby="group-finished">
      <h2 id="group-finished" className="group-head">
        <Glyph glyph="−" tone="muted" label="" />
        <span className="group-name">Finished</span>
        <span className="group-count">{lanes.length}</span>
        <RangeFilter range={range} href={href.board} />
      </h2>
      {error && <p className="group-note tone-bad">Could not load closed lanes: {error}</p>}
      {loading && !error && <p className="group-note faint">Loading…</p>}
      {!loading && !error && lanes.length === 0 && <p className="group-note faint">No lane closed {phrase}.</p>}
      {lanes.length > 0 && (
        <ul className="rows">
          {lanes.map((l, n) => {
            const i = offset + n
            const end = ending(l)
            const cost = runCost(l) || usd(0)
            return (
              <li key={l.id}>
              <div className="row row-finished">
                <Glyph glyph="−" tone={end.tone} label="closed" />
                <span className="row-lane">
                  <a
                    ref={(el) => {
                      rows.current[i] = el
                    }}
                    className="row-link"
                    href={href.lane(l.id)}
                    aria-current={i === selected ? 'true' : undefined}
                    onFocus={() => onSelect(i)}
                    onClick={(e) => {
                      e.preventDefault()
                      go(href.lane(l.id))
                    }}
                  >
                    {l.name}
                  </a>
                </span>
                <span className="row-repo">{l.repo}</span>
                <span className="row-end" title={end.title}>
                  <span className={`tone-${end.tone}`}>{end.text}</span>
                  {l.mr ? (
                    <>
                      {' '}
                      {l.mr_url ? (
                        <a className="row-mr-link" href={l.mr_url} target="_blank" rel="noreferrer">
                          !{l.mr}
                        </a>
                      ) : (
                        <span>!{l.mr}</span>
                      )}
                    </>
                  ) : null}
                </span>
                <span className="row-runs">
                  {l.runs} {l.runs === 1 ? 'run' : 'runs'}
                </span>
                <span className="row-cost">{cost}</span>
                <span className="row-age" title={l.closed}>
                  {since(l.closed, now)} ago
                </span>
              </div>
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}
