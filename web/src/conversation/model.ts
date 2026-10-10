import type { Item } from './types'

/** Items by seq, in thread order: what a later read says replaces what an earlier one did. */
export function merge(have: Item[], got: Item[]): Item[] {
  if (got.length === 0) return have
  const bySeq = new Map(have.map((it) => [it.seq, it]))
  for (const it of got) bySeq.set(it.seq, it)
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq)
}

/** A tool call's one line when collapsed. */
export function toolLine(it: Item): string {
  return it.summary ? `${it.tool ?? 'tool'}  ${it.summary}` : (it.tool ?? 'tool')
}

export const TOOL_STATUS: Record<NonNullable<Item['status']>, string> = {
  running: 'running',
  ok: 'done',
  error: 'failed',
  no_result: 'no result',
}

/** How long a run took, "" while it has not ended. */
export function took(start?: string, end?: string): string {
  if (!start || !end) return ''
  const s = Math.max(0, Math.round((Date.parse(end) - Date.parse(start)) / 1000))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  return m < 60 ? `${m}m ${s % 60}s` : `${Math.floor(m / 60)}h ${m % 60}m`
}

export function sizeText(bytes: number): string {
  if (bytes < 1024) return `${bytes} bytes`
  if (bytes < 1 << 20) return `${(bytes / 1024).toFixed(1)} KiB`
  return `${(bytes / (1 << 20)).toFixed(1)} MiB`
}
