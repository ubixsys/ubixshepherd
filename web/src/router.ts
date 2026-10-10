// Hash routes, so the app needs nothing from the server that serves it.
import { useSyncExternalStore } from 'react'

/** The time filter on the history views. */
export type Range = 'today' | 'week'

/** The usage page's range: three presets and a custom pair of days. */
export type UsageRange = 'today' | 'week' | 'month' | 'custom'

export type Route =
  | { page: 'board'; range?: Range }
  | { page: 'runs'; range?: Range; agent?: string; lane?: number }
  | { page: 'decisions'; focus?: number }
  | { page: 'usage'; range?: UsageRange; from?: string; to?: string }
  | { page: 'chat' }
  | { page: 'lane'; id: number }
  | { page: 'log'; run: number }
  | { page: 'missing'; path: string }

export function parse(hash: string): Route {
  const [path = '/', query = ''] = (hash.replace(/^#/, '') || '/').split('?')
  const q = new URLSearchParams(query)
  const range = q.get('range') === 'week' ? 'week' : q.get('range') === 'today' ? 'today' : undefined
  if (path === '/') return range ? { page: 'board', range } : { page: 'board' }
  if (path === '/runs') {
    const lane = Number(q.get('lane'))
    return {
      page: 'runs',
      ...(range ? { range } : {}),
      ...(q.get('agent') ? { agent: q.get('agent') as string } : {}),
      ...(lane > 0 ? { lane } : {}),
    }
  }
  if (path === '/decisions') {
    const d = Number(q.get('d'))
    return d > 0 ? { page: 'decisions', focus: d } : { page: 'decisions' }
  }
  if (path === '/usage') {
    const r = q.get('range')
    const usageRange = r === 'today' || r === 'week' || r === 'month' || r === 'custom' ? r : undefined
    const day = /^\d{4}-\d{2}-\d{2}$/
    const from = q.get('from') ?? ''
    const to = q.get('to') ?? ''
    return {
      page: 'usage',
      ...(usageRange ? { range: usageRange } : {}),
      ...(usageRange === 'custom' && day.test(from) ? { from } : {}),
      ...(usageRange === 'custom' && day.test(to) ? { to } : {}),
    }
  }
  if (path === '/chat') return { page: 'chat' }
  let m = /^\/lanes\/(\d+)$/.exec(path)
  if (m) return { page: 'lane', id: Number(m[1]) }
  m = /^\/runs\/(\d+)\/log$/.exec(path)
  if (m) return { page: 'log', run: Number(m[1]) }
  return { page: 'missing', path }
}

export const href = {
  board: (range?: Range) => (range ? `#/?range=${range}` : '#/'),
  runs: (o: { range?: Range; agent?: string; lane?: number } = {}) => {
    const q = new URLSearchParams()
    if (o.range) q.set('range', o.range)
    if (o.agent) q.set('agent', o.agent)
    if (o.lane) q.set('lane', String(o.lane))
    return q.size ? `#/runs?${q}` : '#/runs'
  },
  decisions: (focus?: number) => (focus ? `#/decisions?d=${focus}` : '#/decisions'),
  usage: (o: { range?: UsageRange; from?: string; to?: string } = {}) => {
    const q = new URLSearchParams()
    if (o.range) q.set('range', o.range)
    if (o.range === 'custom') {
      if (o.from) q.set('from', o.from)
      if (o.to) q.set('to', o.to)
    }
    return q.size ? `#/usage?${q}` : '#/usage'
  },
  chat: () => '#/chat',
  lane: (id: number) => `#/lanes/${id}`,
  log: (run: number) => `#/runs/${run}/log`,
}

function subscribe(fn: () => void) {
  window.addEventListener('hashchange', fn)
  return () => window.removeEventListener('hashchange', fn)
}

export function useRoute(): Route {
  const hash = useSyncExternalStore(subscribe, () => window.location.hash)
  return parse(hash)
}

export function go(to: string) {
  window.location.hash = to.replace(/^#/, '')
}
