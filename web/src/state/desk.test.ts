import { describe, expect, it, vi } from 'vitest'
import { ApiError, NetworkError } from '../api/client'
import type { DeskApi } from '../api/desk'
import { deskEvent, FakeSource, fakeDeskApi as fakeApi, feedItem } from '../test/fixtures'
import { entries } from '../model/feed'
import { BACKOFF, DeskThread, mergeEvents, OPEN_TIMEOUT, PROBE_EVERY, problemOf, timeline } from './desk'

/** Timers that run when the test says; a cleared one never runs. `now` moves with them. */
function timers() {
  let now = 1_000_000
  let seq = 0
  const pending = new Map<number, { fn: () => void; at: number }>()
  const wake: (() => void)[] = []
  return {
    now: () => now,
    onWake: (fn: () => void) => {
      wake.push(fn)
      return () => wake.splice(wake.indexOf(fn), 1)
    },
    /** Fires the browser's visible/online event. */
    wakeUp: () => wake.slice().forEach((fn) => fn()),
    setTimeout: (fn: () => void, ms: number) => {
      pending.set(++seq, { fn, at: now + ms })
      return seq
    },
    clearTimeout: (id: unknown) => void pending.delete(id as number),
    /** Delays of the timers waiting, soonest first. */
    waits: () => [...pending.values()].map((t) => t.at - now).sort((x, y) => x - y),
    /** Moves time on, running what falls due (and what that schedules within the span). */
    advance: async (ms: number) => {
      const end = now + ms
      for (;;) {
        const due = [...pending.entries()].filter(([, t]) => t.at <= end).sort((x, y) => x[1].at - y[1].at)[0]
        if (!due) break
        pending.delete(due[0])
        now = Math.max(now, due[1].at)
        due[1].fn()
        await flush()
      }
      now = end
    },
  }
}

const flush = () => new Promise((r) => setTimeout(r, 0))

function make(api: DeskApi) {
  FakeSource.all = []
  const t = timers()
  const thread = new DeskThread(0, {
    api, open: (u) => new FakeSource(u), setTimeout: t.setTimeout, clearTimeout: t.clearTimeout, now: t.now, onWake: t.onWake,
  })
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

  it('shows a dropped stream at once, then resumes from the last stored event without repeats', async () => {
    const { thread, t } = make(fakeApi())
    thread.start()
    await flush()
    const first = FakeSource.get(0)
    first.open()
    first.emit('user', deskEvent({ seq: 3 }))
    first.emit('partial', { kind: 'partial', text: 'x' })
    first.fail()
    expect(thread.getSnapshot()).toMatchObject({ connection: 'reconnecting', partial: '', failures: 1 })
    expect(thread.getSnapshot().retryAt).toBe(t.now() + BACKOFF.min)
    expect(first.closed).toBe(true)
    await t.advance(BACKOFF.min)
    const second = FakeSource.get(1)
    expect(second.url).toBe('/stream?ws=0&after=3')
    second.open()
    expect(thread.getSnapshot()).toMatchObject({ connection: 'live', retryAt: null, failures: 0 })
    second.emit('user', deskEvent({ seq: 3 }))
    second.emit('assistant', deskEvent({ seq: 4, kind: 'assistant', text: 'a' }))
    expect(thread.getSnapshot().events.map((e) => e.seq)).toEqual([3, 4])
  })

  it('backs off from 1s doubling to 15s while the daemon is down, and resets once live', async () => {
    let up = true
    const history = vi.fn(async () => {
      if (!up) throw new TypeError('Failed to fetch')
      return { events: [], more: false }
    })
    const { thread, t } = make(fakeApi({ history }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    up = false
    FakeSource.get(0).fail()
    const waits: number[] = []
    for (let i = 0; i < 7; i++) {
      waits.push(t.waits()[0] ?? 0)
      await t.advance(waits.at(-1) ?? 0)
      expect(thread.getSnapshot().connection).toBe('reconnecting')
    }
    expect(waits).toEqual([1000, 2000, 4000, 8000, 15000, 15000, 15000])
    up = true
    await t.advance(15_000)
    FakeSource.get(FakeSource.all.length - 1).open()
    expect(thread.getSnapshot().connection).toBe('live')
    FakeSource.get(FakeSource.all.length - 1).fail()
    expect(t.waits()[0]).toBe(BACKOFF.min)
  })

  it('counts a stream that never opens as lost', async () => {
    const { thread, t } = make(fakeApi())
    thread.start()
    await flush()
    await t.advance(OPEN_TIMEOUT)
    expect(thread.getSnapshot().connection).toBe('reconnecting')
    expect(FakeSource.get(0).closed).toBe(true)
  })

  it('counts a failed status probe (a hang fails by the client timeout) as lost, though the stream says nothing', async () => {
    let answer: 'ok' | 'fail' = 'ok'
    const status = vi.fn(async () => {
      if (answer === 'fail') throw new NetworkError('the daemon did not answer in time')
      return { workspace_id: 1, busy: false, queued: 0, attached: 1, session: '', model: '', wake: 'attached' }
    })
    const { thread, t } = make(fakeApi({ status }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    await t.advance(PROBE_EVERY)
    expect(thread.getSnapshot().connection).toBe('live')
    answer = 'fail'
    await t.advance(PROBE_EVERY)
    expect(thread.getSnapshot().connection).toBe('reconnecting')
    expect(FakeSource.get(0).closed).toBe(true)
    expect(status).toHaveBeenCalled()
  })

  it('catches up on what was missed during a drop, merging the daemon page without repeats', async () => {
    let page = [deskEvent({ seq: 1 }), deskEvent({ seq: 2, kind: 'assistant', text: 'two' })]
    const status = vi.fn(async () => ({ workspace_id: 1, busy: true, queued: 1, attached: 1, session: '', model: '', wake: 'attached' }))
    const { thread, t } = make(fakeApi({ history: async () => ({ events: page, more: false }), status }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    FakeSource.get(0).emit('partial', { kind: 'partial', text: 'half a rep' })
    FakeSource.get(0).fail()
    page = [...page, deskEvent({ seq: 3, kind: 'assistant', text: 'three' }), deskEvent({ seq: 4, kind: 'turn_end' })]
    await t.advance(BACKOFF.min)
    expect(thread.getSnapshot().events.map((e) => e.seq)).toEqual([1, 2, 3, 4])
    expect(thread.getSnapshot().partial).toBe('')
    expect(FakeSource.get(1).url).toBe('/stream?ws=0&after=4')
    FakeSource.get(1).open()
    FakeSource.get(1).emit('turn_end', deskEvent({ seq: 4, kind: 'turn_end' }))
    FakeSource.get(1).emit('assistant', deskEvent({ seq: 5, kind: 'assistant', text: 'five' }))
    expect(thread.getSnapshot().events.map((e) => e.seq)).toEqual([1, 2, 3, 4, 5])
  })

  it('takes the daemon page when the drop left a gap larger than a page', async () => {
    let page = [deskEvent({ seq: 1 }), deskEvent({ seq: 2 })]
    let more = false
    const { thread, t } = make(fakeApi({ history: async () => ({ events: page, more }) }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    FakeSource.get(0).fail()
    page = [deskEvent({ seq: 50 }), deskEvent({ seq: 51 })]
    more = true
    await t.advance(BACKOFF.min)
    expect(thread.getSnapshot().events.map((e) => e.seq)).toEqual([50, 51])
    expect(thread.getSnapshot().more).toBe(true)
  })

  it('retries at once when the tab becomes visible or the browser is online, and resets the backoff', async () => {
    let up = false
    const history = vi.fn(async () => {
      if (!up) throw new NetworkError('down')
      return { events: [], more: false }
    })
    const { thread, t } = make(fakeApi({ history }))
    thread.start()
    await flush()
    for (let i = 0; i < 3; i++) await t.advance(t.waits()[0] ?? 0)
    expect(t.waits()[0]).toBe(8000)
    const calls = history.mock.calls.length
    up = true
    t.wakeUp()
    await flush()
    expect(history.mock.calls.length).toBe(calls + 1)
    expect(FakeSource.all.length).toBeGreaterThan(0)
    FakeSource.get(FakeSource.all.length - 1).open()
    expect(thread.getSnapshot().connection).toBe('live')
  })

  it('probes at once on wake while live, and stops listening when stopped', async () => {
    const status = vi.fn(async () => ({ workspace_id: 1, busy: false, queued: 0, attached: 1, session: '', model: '', wake: 'attached' }))
    const { thread, t } = make(fakeApi({ status }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    await flush()
    const n = status.mock.calls.length
    t.wakeUp()
    await flush()
    expect(status.mock.calls.length).toBe(n + 1)
    thread.stop()
    t.wakeUp()
    await flush()
    expect(status.mock.calls.length).toBe(n + 1)
  })

  it('says signed out, and stops retrying, when a restart ended the session', async () => {
    let signedOut = false
    const history = vi.fn(async () => {
      if (signedOut) throw new ApiError(401, 'missing, wrong or revoked token')
      return { events: [], more: false }
    })
    const { thread, t } = make(fakeApi({ history }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    signedOut = true
    FakeSource.get(0).fail()
    await t.advance(BACKOFF.min)
    const s = thread.getSnapshot()
    expect(s.connection).toBe('signed-out')
    expect(s.problem?.message).toContain('shepherd web')
    expect(s.problem?.message).toContain('restart')
    expect(t.waits()).toHaveLength(0)
  })

  it('treats a probe answered with 403 as refused', async () => {
    const status = vi.fn().mockResolvedValueOnce({ workspace_id: 1, busy: false, queued: 0, attached: 1, session: '', model: '', wake: 'attached' })
      .mockRejectedValue(new ApiError(403, 'bad origin'))
    const { thread, t } = make(fakeApi({ status }))
    thread.start()
    await flush()
    FakeSource.get(0).open()
    await t.advance(PROBE_EVERY)
    expect(thread.getSnapshot().connection).toBe('refused')
    expect(t.waits()).toHaveLength(0)
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
