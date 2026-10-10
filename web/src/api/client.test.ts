import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError, CSRF_HEADER, NetworkError, REQUEST_TIMEOUT } from './client'

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

  it('counts a request that hangs as lost, by aborting it after the timeout', async () => {
    vi.useFakeTimers()
    try {
      vi.stubGlobal('fetch', vi.fn((_p: string, init?: RequestInit) => new Promise((_, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
      })))
      const out = api.status().then(() => 'answered', (e: unknown) => e)
      await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT)
      const e = await out
      expect(e).toBeInstanceOf(NetworkError)
      expect((e as Error).message).toContain('did not answer')
    } finally {
      vi.useRealTimers()
    }
  })

  it('reports an unreachable daemon as a network error and a refusal as an API error', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => {
      throw new TypeError('Failed to fetch')
    }))
    await expect(api.status()).rejects.toBeInstanceOf(NetworkError)
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'nope' }, 401)))
    await expect(api.status()).rejects.toBeInstanceOf(ApiError)
  })
})
