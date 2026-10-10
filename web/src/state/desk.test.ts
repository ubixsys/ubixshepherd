import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/client'
import type { DeskApi } from '../api/desk'
import { deskEvent, FakeSource, fakeDeskApi as fakeApi, feedItem } from '../test/fixtures'
import { entries } from '../model/feed'
import { BACKOFF, DeskThread, mergeEvents, problemOf, timeline } from './desk'

function timers() {
  const pending: { fn: () => void; ms: number }[] = []
  return {
    pending,
    setTimeout: (fn: () => void, ms: number) => pending.push({ fn, ms }),
    clearTimeout: () => {},
    fire: () => pending.splice(0).forEach((t) => t.fn()),
  }
}

const flush = () => new Promise((r) => setTimeout(r, 0))

function make(api: DeskApi) {
  FakeSource.all = []
  const t = timers()
  const thread = new DeskThread(0, { api, open: (u) => new FakeSource(u), setTimeout: t.setTimeout, clearTimeout: t.clearTimeout })
  return { thread, t }
}

describe('DeskThread', () => {
  it('follows the stream from the newest stored event of the history', async () => {
    const { thread } = make(fakeApi({ history: async () => ({ events: [deskEvent({ seq: 4 }), deskEvent({ seq: 7, kind: 'assistant', text: 'hi' })], more: true }) }))
    thread.start()
    await flush()
    expect(FakeSource.all.map((s) => s.url)).toEqual(['/stream?ws=0&after=7'])
    expect(thread.getSnapshot().events.map((e) => e.seq)).toEqual([4, 7])
    expect(thread.getSnapshot().more).toBe(true)
  })

  it('grows one reply from partial pieces and replaces it with the final event', async () => {
    const { thread } = make(fakeApi())
    thread.start()
    await flush()
    const s = FakeSource.get(0)
    s.open()
    s.emit('partial', { kind: 'partial', text: 'Hel' })
    s.emit('partial', { kind: 'partial', text: 'lo' })
    expect(thread.getSnapshot().partial).toBe('Hello')
    expect(thread.getSnapshot().events).toHaveLength(0)
    s.emit('assistant', deskEvent({ seq: 5, kind: 'assistant', text: 'Hello there' }))
    expect(thread.getSnapshot().partial).toBe('')
    expect(thread.getSnapshot().events.map((e) => e.text)).toEqual(['Hello there'])
  })

  it('resumes from the last stored event after a drop, backing off, without repeats', async () => {
    const { thread, t } = make(fakeApi())
    thread.start()
    await flush()
    const first = FakeSource.get(0)
    first.open()
    first.emit('user', deskEvent({ seq: 3 }))
    first.emit('partial', { kind: 'partial', text: 'x' })
    first.fail()
    expect(thread.getSnapshot().connection).toBe('reconnecting')
    await flush()
    expect(first.closed).toBe(true)
    expect(t.pending[0]?.ms).toBe(BACKOFF.min)
    t.fire()
    const second = FakeSource.get(1)
    expect(second.url).toBe('/stream?ws=0&after=3')
    second.open()
    expect(thread.getSnapshot().connection).toBe('live')
    second.emit('user', deskEvent({ seq: 3 }))
    second.emit('assistant', deskEvent({ seq: 4, kind: 'assistant', text: 'a' }))
    expect(thread.getSnapshot().events.map((e) => e.seq)).toEqual([3, 4])
  })

  it('doubles the wait while the stream keeps failing', async () => {
    const { thread, t } = make(fakeApi())
    thread.start()
    await flush()
    const waits: number[] = []
    for (let i = 0; i < 3; i++) {
      FakeSource.get(FakeSource.all.length - 1).fail()
      await flush()
      waits.push((t.pending[0]?.ms ?? 0))
      t.fire()
    }
    expect(waits).toEqual([500, 1000, 2000])
  })

  it('stops retrying and says so when the session is gone', async () => {
    const status = vi.fn().mockRejectedValue(new ApiError(401, 'missing, wrong or revoked token'))
    const { thread, t } = make(fakeApi({ status }))
    thread.start()
    await flush()
    FakeSource.get(0).fail()
    await flush()
    expect(thread.getSnapshot().connection).toBe('signed-out')
    expect(thread.getSnapshot().problem?.message).toContain('shepherd web')
    expect(t.pending).toHaveLength(0)
  })

  it('sends, and reports the full queue, signed out and refused apart', async () => {
    const turn = vi.fn()
      .mockResolvedValueOnce({ workspace_id: 1, turn: 2, ahead: 2 })
      .mockRejectedValueOnce(new ApiError(429, 'the desk has 8 turns waiting'))
      .mockRejectedValueOnce(new ApiError(403, 'bad origin'))
      .mockRejectedValueOnce(new ApiError(401, 'no'))
    const { thread } = make(fakeApi({ turn }))
    thread.start()
    await flush()
    expect(await thread.send('  ')).toBe(false)
    expect(await thread.send('hello')).toBe(true)
    expect(thread.getSnapshot()).toMatchObject({ busy: true, queued: 2, sending: false })
    expect(await thread.send('again')).toBe(false)
    expect(thread.getSnapshot().problem?.kind).toBe('full')
    await thread.send('x')
    expect(thread.getSnapshot().problem).toMatchObject({ kind: 'refused' })
    expect(thread.getSnapshot().problem?.message).toContain('bad origin')
    await thread.send('x')
    expect(thread.getSnapshot()).toMatchObject({ connection: 'signed-out', problem: { kind: 'signed-out' } })
  })

  it('maps API failures', () => {
    expect(problemOf(new Error('boom'))).toEqual({ kind: 'other', message: 'boom' })
    expect(problemOf(new ApiError(500, 'x')).kind).toBe('other')
  })
})

describe('timeline', () => {
  it('puts typed rows between messages by time, and drops the noise', () => {
    const events = [
      deskEvent({ seq: 1, created: '2026-10-08T10:00:00Z' }),
      deskEvent({ seq: 2, kind: 'assistant', created: '2026-10-08T10:02:00Z' }),
    ]
    const feed = entries({
      items: [
        feedItem({ id: 1, kind: 'run_passed', created: '2026-10-08T09:00:00Z' }),
        feedItem({ id: 2, kind: 'run_passed', created: '2026-10-08T10:01:00Z' }),
        feedItem({ id: 3, kind: 'commit', created: '2026-10-08T10:01:30Z' }),
        feedItem({ id: 4, kind: 'decision', ref: 5, created: '2026-10-08T10:03:00Z' }),
      ],
      last: 4,
    })
    expect(timeline(events, feed).map((i) => i.key)).toEqual(['d1', 'f2', 'd2', 'f4'])
  })

  it('shows no rows before any message loaded', () => {
    expect(timeline([], entries({ items: [feedItem()], last: 1 }))).toEqual([])
  })

  it('merges event lists by seq', () => {
    const m = mergeEvents([deskEvent({ seq: 5 }), deskEvent({ seq: 6 })], [deskEvent({ seq: 4 }), deskEvent({ seq: 5 })])
    expect(m.map((e) => e.seq)).toEqual([4, 5, 6])
  })
})
