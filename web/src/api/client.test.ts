import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, CSRF_HEADER } from './client'

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

afterEach(() => vi.unstubAllGlobals())

describe('the API client', () => {
  it("sends a cookie session's CSRF token on writes only", async () => {
    const calls: [string, RequestInit | undefined][] = []
    vi.stubGlobal('fetch', vi.fn(async (path: string, init?: RequestInit) => {
      calls.push([path, init])
      if (path === '/v1/web/session') return json({ role: 'web', csrf: 'tok' })
      return json({})
    }))
    await api.decisions('open')
    await api.answer(1, 'yes')
    await api.answer(2, 'no')
    const header = (init?: RequestInit) => (init?.headers as Record<string, string> | undefined)?.[CSRF_HEADER]
    expect(calls.map(([p]) => p)).toEqual([
      '/v1/decisions?state=open', '/v1/web/session', '/v1/decisions/1/answer', '/v1/decisions/2/answer',
    ])
    expect(header(calls[0]?.[1])).toBeUndefined()
    expect(header(calls[2]?.[1])).toBe('tok')
    expect(header(calls[3]?.[1])).toBe('tok')
  })
})
