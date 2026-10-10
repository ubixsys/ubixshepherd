// Hash routes, so the app needs nothing from the server that serves it.
import { useSyncExternalStore } from 'react'

export type Route =
  | { page: 'board' }
  | { page: 'decisions'; focus?: number }
  | { page: 'chat' }
  | { page: 'lane'; id: number }
  | { page: 'log'; run: number }
  | { page: 'missing'; path: string }

export function parse(hash: string): Route {
  const [path = '/', query = ''] = (hash.replace(/^#/, '') || '/').split('?')
  if (path === '/') return { page: 'board' }
  if (path === '/decisions') {
    const d = Number(new URLSearchParams(query).get('d'))
    return d > 0 ? { page: 'decisions', focus: d } : { page: 'decisions' }
  }
  if (path === '/chat') return { page: 'chat' }
  let m = /^\/lanes\/(\d+)$/.exec(path)
  if (m) return { page: 'lane', id: Number(m[1]) }
  m = /^\/runs\/(\d+)\/log$/.exec(path)
  if (m) return { page: 'log', run: Number(m[1]) }
  return { page: 'missing', path }
}

export const href = {
  board: () => '#/',
  decisions: (focus?: number) => (focus ? `#/decisions?d=${focus}` : '#/decisions'),
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
