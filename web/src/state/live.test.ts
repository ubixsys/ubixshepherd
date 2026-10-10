import type { Feed } from '../api/types'
import { ApiError } from '../api/client'
import { BACKOFF, INTERVALS, Live, type Api, type Clock } from './live'

// A clock whose timers run only when the test says, and a tab that can be hidden.
function fakeClock() {
  let now = Date.parse('2026-10-08T12:00:00Z')
  let hidden = false
  const timers: { fn: () => void; ms: number }[] = []
  const clock: Clock = {
    now: () => now,
    hidden: () => hidden,
    setTimeout: (fn, ms) => {
      const t = { fn, ms }
      timers.push(t)
      return t
    },
    clearTimeout: (id) => {
      const i = timers.indexOf(id as (typeof timers)[number])
      if (i >= 0) timers.splice(i, 1)
    },
  }
  return {
    clock,
    timers,
    hide: (h: boolean) => (hidden = h),
    advance: (ms: number) => (now += ms),
    /** Runs the timers due now, as the browser would. */
    fire: () => {
      const due = timers.splice(0)
      for (const t of due) t.fn()
    },
  }
}

const settle = () => new Promise((r) => setTimeout(r, 0))

function fakeApi(over: Partial<Api> = {}) {
  const feeds: Feed[] = []
  const calls: string[] = []
  const api: Api = {
    status: async () => (calls.push('status'), { version: 'x', pid: 1, started: '', store: '', config: '', workspaces: [{ id: 7, name: 'w', path: '/w', created: '', repos: 1, lanes: 1 }] }),
    lanes: async (ws) => (calls.push(`lanes:${ws}`), []),
    runs: async () => (calls.push('runs'), [
      { id: 3, lane_id: 1, agent: 'claude', prompt: '', state: 'running', log: '', start_sha: '', commits: 0, started: '2026-10-08T11:00:00Z', lane: 'l', repo: 'r', worktree: '' },
    ]),
    run: async () => { throw new Error('unused') },
    runLog: async () => { throw new Error('unused') },
    runEvents: async () => { throw new Error('unused') },
    decisions: async () => (calls.push('decisions'), []),
    requests: async () => (calls.push('requests'), []),
    spend: async () => (calls.push('spend'), { day: '2026-10-08', usd: 1, budget: 20, credit_usd: 0, by_source: null }),
    feedLatest: async () => (calls.push('latest'), { items: [], events: [], last: 5 }),
    feed: async (after) => (calls.push(`feed:${after}`), feeds.shift() ?? { items: [], events: [], last: after }),
    answer: async () => { throw new Error('unused') },
    ...over,
  }
  return { api, feeds, calls }
}

beforeEach(() => localStorage.clear())

describe('Live', () => {
  it('loads the lists for every workspace and starts the feed at the newest item', async () => {
    const c = fakeClock()
    const f = fakeApi()
    const live = new Live(f.api, c.clock)
    live.start()
    await settle()
    expect(f.calls).toContain('lanes:7')
    expect(f.calls).toContain('latest')
    expect(live.getSnapshot().ready).toBe(true)
    expect(live.getSnapshot().spend?.usd).toBe(1)
  })

  it('polls the feed from its cursor, notes a run outcome at once and reloads the lists', async () => {
    const c = fakeClock()
    const f = fakeApi()
    const live = new Live(f.api, c.clock)
    live.start()
    await settle()
    f.feeds.push({
      items: [{ id: 6, kind: 'run_passed', text: 'run 3 passed', ref: 3, created: '2026-10-08T12:00:05Z' }],
      events: ['run_passed'],
      last: 6,
    })
    // The runs reload answers "running" again; the outcome must still show until the
    // list catches up, so hold the reload.
    let release: () => void = () => {}
    f.api.status = () => new Promise((r) => (release = () => r({ version: 'x', pid: 1, started: '', store: '', config: '', workspaces: [] })))
    f.calls.length = 0
    c.fire()
    await settle()
    expect(f.calls[0]).toBe('feed:5')
    const snap = live.getSnapshot()
    expect(snap.feed.map((e) => e.event)).toEqual(['run_passed'])
    expect(snap.runs[0]?.state).toBe('succeeded')
    expect(snap.runs[0]?.ended).toBe('2026-10-08T12:00:05Z')
    release()
  })

  it('polls more slowly while the tab is hidden', async () => {
    const c = fakeClock()
    const live = new Live(fakeApi().api, c.clock)
    live.start()
    await settle()
    expect(c.timers.map((t) => t.ms).sort()).toEqual([INTERVALS.feedVisible, INTERVALS.listsVisible].sort())
    c.hide(true)
    c.fire()
    await settle()
    expect(c.timers.map((t) => t.ms).sort()).toEqual([INTERVALS.feedHidden, INTERVALS.listsHidden].sort())
    live.stop()
  })

  it('keeps what it had and says why when the daemon fails', async () => {
    const c = fakeClock()
    const f = fakeApi()
    const live = new Live(f.api, c.clock)
    live.start()
    await settle()
    f.api.status = async () => {
      throw new Error('the daemon is not reachable')
    }
    await live.refresh()
    expect(live.getSnapshot().error).toBe('the daemon is not reachable')
    expect(live.getSnapshot().ready).toBe(true)
    expect(live.getSnapshot().runs).toHaveLength(1)
  })

  it('backs off from 1s to 15s while the daemon is down, then catches up from its cursor', async () => {
    const c = fakeClock()
    const f = fakeApi()
    const live = new Live(f.api, c.clock)
    live.start()
    await settle()
    const { feed, status } = f.api
    f.api.feed = f.api.status = async () => {
      throw new Error('Failed to fetch')
    }
    const waits: number[] = []
    for (let i = 0; i < 6; i++) {
      c.fire()
      await settle()
      waits.push(Math.min(...c.timers.map((t) => t.ms)))
    }
    expect(live.getSnapshot().error).toBe('Failed to fetch')
    expect(waits.slice(0, 5)).toEqual([BACKOFF.min, 2000, 4000, 8000, BACKOFF.max])
    expect(waits[5]).toBe(BACKOFF.max)
    // The daemon is back with two items missed: they arrive once, after the cursor.
    f.api.feed = feed
    f.api.status = status
    f.feeds.push({
      items: [
        { id: 6, kind: 'commit', text: 'a', ref: 1, created: '2026-10-08T12:00:05Z' },
        { id: 7, kind: 'commit', text: 'b', ref: 1, created: '2026-10-08T12:00:06Z' },
      ],
      events: ['commit', 'commit'],
      last: 7,
    })
    live.wake()
    await settle()
    expect(live.getSnapshot().error).toBeNull()
    expect(live.getSnapshot().feed.map((e) => e.id)).toEqual([6, 7])
    expect(c.timers.some((t) => t.ms === INTERVALS.feedVisible)).toBe(true)
  })

  it('counts a request that never answers as lost once the client gives up on it', async () => {
    const c = fakeClock()
    const f = fakeApi()
    const live = new Live(f.api, c.clock)
    live.start()
    await settle()
    f.api.status = async () => {
      throw new Error('the daemon did not answer in time')
    }
    await live.refresh()
    expect(live.getSnapshot().error).toContain('did not answer')
  })

  it('says what to do when the session ended', async () => {
    const c = fakeClock()
    const f = fakeApi()
    const live = new Live(f.api, c.clock)
    live.start()
    await settle()
    f.api.status = async () => {
      throw new ApiError(401, 'missing, wrong or revoked token')
    }
    await live.refresh()
    expect(live.getSnapshot().error).toContain('shepherd web')
  })

  it('remembers what was seen across reloads', () => {
    const c = fakeClock()
    const a = new Live(fakeApi().api, c.clock)
    a.markSeen('run:3')
    const b = new Live(fakeApi().api, c.clock)
    expect(b.getSnapshot().seen.keys.has('run:3')).toBe(true)
    c.advance(60_000)
    b.clearDone()
    expect(new Live(fakeApi().api, c.clock).getSnapshot().seen).toEqual({ until: '2026-10-08T12:01:00.000Z', keys: new Set() })
  })
})
