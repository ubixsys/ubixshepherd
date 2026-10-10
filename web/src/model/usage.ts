// The usage page: which days a range names, and how the groups sort and chart. Pure
// functions over what GET /v1/usage answers.
import type { UsageGroup, UsageTotals } from '../api/types'
import type { UsageRange } from '../router'

/** The context size past which a 200k window could not have held a run without compacting. */
export const LARGE_CONTEXT = 200_000

export const USAGE_RANGES: readonly { id: UsageRange; label: string }[] = [
  { id: 'today', label: 'Today' },
  { id: 'week', label: 'Last 7 days' },
  { id: 'month', label: 'Last 30 days' },
  { id: 'custom', label: 'Custom' },
]

/** The window the page opens on: the one the usage doc suggests for the 1M question. */
export const DEFAULT_USAGE_RANGE: UsageRange = 'week'

/** A local day as the daemon spells it: "2026-10-10". */
export function localDay(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

function validDay(s: string | undefined): s is string {
  return !!s && /^\d{4}-\d{2}-\d{2}$/.test(s) && localDay(new Date(`${s}T12:00:00`)) === s
}

/** The first and last day (inclusive) a range names. A custom range missing or reversing a day falls back to the week. */
export function usageDays(range: UsageRange, now: number, from?: string, to?: string): { from: string; to: string } {
  const today = new Date(now)
  const back = (days: number) => {
    const d = new Date(today)
    d.setDate(d.getDate() - days)
    return localDay(d)
  }
  if (range === 'custom') {
    const f = validDay(from) ? from : back(6)
    const t = validDay(to) ? to : localDay(today)
    if (f <= t) return { from: f, to: t }
    return { from: t, to: f }
  }
  if (range === 'today') return { from: localDay(today), to: localDay(today) }
  return { from: back(range === 'month' ? 29 : 6), to: localDay(today) }
}

export type SortKey =
  | 'name' | 'count' | 'input_tokens' | 'cache_read_tokens' | 'cache_creation_tokens' | 'output_tokens'
  | 'peak_context' | 'over_200k' | 'compactions' | 'cost_usd'

export interface Sort {
  key: SortKey
  /** Descending, the way numbers read best. */
  desc: boolean
}

export const DEFAULT_SORT: Sort = { key: 'cost_usd', desc: true }

/** The first click on a column sorts names ascending and numbers biggest first; a second flips it. */
export function nextSort(cur: Sort, key: SortKey): Sort {
  if (cur.key === key) return { key, desc: !cur.desc }
  return { key, desc: key !== 'name' }
}

/** A copy of the groups in the order asked. Ties keep the daemon's order, then name. */
export function sortGroups(groups: readonly UsageGroup[], s: Sort): UsageGroup[] {
  const dir = s.desc ? -1 : 1
  return groups
    .map((g, i) => ({ g, i }))
    .sort((a, b) => {
      const d = s.key === 'name' ? a.g.key.localeCompare(b.g.key) : a.g[s.key] - b.g[s.key]
      return d * dir || a.i - b.i
    })
    .map((x) => x.g)
}

/** Whether a group has a measured peak to chart: Copilot and Cursor runs carry cost only. */
export function hasPeak(t: UsageTotals): boolean {
  return t.measured > 0 && t.peak_context > 0
}

export interface ChartRow {
  key: string
  /** What the row is, for the label: the model or lane, and for lanes the repo. */
  label: string
  prefix?: string
  peak: number
  window: number
  over: number
  measured: number
}

export interface ChartSection {
  title: string
  rows: ChartRow[]
}

/** The groups that carry a measured peak, biggest peak first, a section each. Lanes are cut to the `lanes` biggest. */
export function chartSections(
  models: readonly UsageGroup[],
  desk: readonly UsageGroup[],
  lanes: readonly UsageGroup[],
  maxLanes = 12,
): ChartSection[] {
  const rows = (gs: readonly UsageGroup[], lane = false): ChartRow[] =>
    gs
      .filter(hasPeak)
      .sort((a, b) => b.peak_context - a.peak_context)
      .map((g) => ({
        key: g.key,
        label: lane ? (g.lane ?? g.key) : g.key,
        ...(lane && g.repo ? { prefix: g.repo } : {}),
        peak: g.peak_context,
        window: g.context_window ?? 0,
        over: g.over_200k,
        measured: g.measured,
      }))
  return [
    { title: 'Agent runs, by agent and model', rows: rows(models) },
    { title: 'Front desk, by model', rows: rows(desk) },
    { title: 'Agent runs, by lane', rows: rows(lanes, true).slice(0, maxLanes) },
  ].filter((s) => s.rows.length > 0)
}

/** The chart's scale: far enough for the biggest peak and the 200k line, with room for its label. A window beyond it is marked in words, so a 1M window does not squash every run. */
export function chartMax(sections: readonly ChartSection[]): number {
  const peak = Math.max(0, ...sections.flatMap((s) => s.rows.map((r) => r.peak)))
  return Math.max(LARGE_CONTEXT, peak) * 1.1
}
