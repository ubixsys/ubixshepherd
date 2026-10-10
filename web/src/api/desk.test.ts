import { afterEach, describe, expect, it, vi } from 'vitest'
import { deskApi } from './desk'
import { CSRF_HEADER } from './client'

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

afterEach(() => vi.unstubAllGlobals())

describe('the desk API', () => {
  it('names the workspace only when there is one to name, and puts the session in no URL', async () => {
    const calls: [string, RequestInit | undefined][] = []
    vi.stubGlobal('fetch', vi.fn(async (path: string, init?: RequestInit) => {
      calls.push([path, init])
      if (path === '/v1/web/session') return json({ role: 'web', csrf: 'tok' })
      return json({ events: [], more: false })
    }))
    await deskApi.history(0)
    await deskApi.history(3, 40, 50)
    await deskApi.turn(0, 'hello')
    await deskApi.turn(3, 'hi')
    expect(calls.map(([p]) => p)).toEqual([
      '/v1/desk/history?limit=100',
      '/v1/desk/history?workspace_id=3&before=40&limit=50',
      '/v1/web/session',
      '/v1/desk/turn',
      '/v1/desk/turn',
    ])
    const turn = calls[3]?.[1]
    expect((turn?.headers as Record<string, string>)[CSRF_HEADER]).toBe('tok')
    expect(JSON.parse(String(turn?.body))).toEqual({ text: 'hello' })
    expect(JSON.parse(String(calls[4]?.[1]?.body))).toEqual({ workspace_id: 3, text: 'hi' })
    for (const [p] of calls) expect(p).not.toContain('tok')
  })

  it('resumes the stream by query, since an EventSource sends no headers', () => {
    expect(deskApi.streamUrl(0, 42)).toBe('/v1/desk/stream?after=42')
    expect(deskApi.streamUrl(2, 0)).toBe('/v1/desk/stream?workspace_id=2&after=0')
  })
})
