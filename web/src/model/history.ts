// The history views: closed lanes on the board and every run on the Runs page. The time
// filter is two ranges, both counted back from the browser's clock.
import type { LaneRecord, RunHistory } from '../api/types'
import type { Range } from '../router'
import type { Tone } from './marks'

export const RANGES: readonly { id: Range; label: string }[] = [
  { id: 'today', label: 'Today' },
  { id: 'week', label: 'Last 7 days' },
]

export const DEFAULT_RANGE: Range = 'today'

/** What a range's total is called: "today", "in the last 7 days". */
export const RANGE_PHRASE: Record<Range, string> = { today: 'today', week: 'in the last 7 days' }

/** The start of a range: today is since local midnight, a week is 7 days back from now. */
export function rangeStart(range: Range, now: number): Date {
  if (range === 'week') return new Date(now - 7 * 24 * 3600 * 1000)
  const d = new Date(now)
  d.setHours(0, 0, 0, 0)
  return d
}

export interface Ending {
  /** "merged", "MR closed", "dropped", "closed, no MR". */
  text: string
  tone: Tone
  /** More for the tooltip. */
  title: string
}

/** How a closed lane ended, in words. The daemon does not record a close as forced; this is
 * what the forge last said about the lane's merge request. */
export function ending(l: Pick<LaneRecord, 'outcome' | 'mr' | 'mr_state'>): Ending {
  const mr = l.mr ? `!${l.mr}` : 'its merge request'
  switch (l.outcome) {
    case 'merged':
      return { text: 'merged', tone: 'ok', title: `${mr} was merged` }
    case 'mr_closed':
      return { text: 'MR closed', tone: 'muted', title: `${mr} was closed without a merge` }
    case 'dropped':
      return { text: 'dropped', tone: 'muted', title: `closed while ${mr} was still ${l.mr_state === 'open' ? 'open' : 'unmerged'}` }
    default:
      return { text: 'closed, no MR', tone: 'muted', title: 'closed with no merge request' }
  }
}

/** The lanes closed since a time, newest closed first, from a daemon that may have sent more. */
export function finished(lanes: LaneRecord[], since: Date): LaneRecord[] {
  return lanes
    .filter((l) => l.state === 'closed' && l.closed !== undefined && Date.parse(l.closed) >= since.getTime())
    .sort((a, b) => Date.parse(b.closed as string) - Date.parse(a.closed as string) || b.id - a.id)
}

/** One history from several workspaces' (a daemon answers per workspace). */
export function mergeHistories(parts: RunHistory[]): RunHistory {
  if (parts.length === 1) return parts[0] as RunHistory
  const lanes = new Map<number, RunHistory['lanes'][number]>()
  for (const p of parts) for (const l of p.lanes) lanes.set(l.id, l)
  return {
    runs: parts.flatMap((p) => p.runs).sort((a, b) => b.id - a.id),
    count: parts.reduce((n, p) => n + p.count, 0),
    cost_usd: parts.reduce((n, p) => n + p.cost_usd, 0),
    credits: parts.reduce((n, p) => n + p.credits, 0),
    agents: [...new Set(parts.flatMap((p) => p.agents))].sort(),
    lanes: [...lanes.values()].sort((a, b) => a.repo.localeCompare(b.repo) || a.name.localeCompare(b.name)),
  }
}
