// Live is the app's one view of the daemon: the lists the board is built from and a
// window of the feed, kept fresh by polling. The feed is polled with its cursor and wakes
// a refresh of the lists when something happened; the lists are also refreshed on a
// slower clock, since the forge's state changes without asking. Both slow down while the
// tab is hidden. (Server-sent events will replace the feed poll.)
import { api as realApi, ApiError, SIGN_IN_HINT } from '../api/client'
import type { DecisionView, LaneView, RequestView, RunView, SpendToday, Status } from '../api/types'
import { noteOutcome, type Seen } from '../model/board'
import { entries, type FeedEntry } from '../model/feed'

export type Api = typeof realApi

export interface Snapshot {
  status: Status | null
  lanes: LaneView[]
  runs: RunView[]
  decisions: DecisionView[]
  requests: RequestView[]
  spend: SpendToday | null
  /** The newest FEED_WINDOW items, oldest first. */
  feed: FeedEntry[]
  /** The daemon could not be reached, or refused: what it said. */
  error: string | null
  /** Lists have loaded at least once. */
  ready: boolean
  /** When the lists last loaded. */
  updated: number
  seen: Seen
}

export const FEED_WINDOW = 1000

export const INTERVALS = {
  feedVisible: 1500,
  feedHidden: 10_000,
  listsVisible: 5000,
  listsHidden: 30_000,
}

/** While the daemon does not answer, both polls back off from here, doubling to the cap. */
export const BACKOFF = { min: 1000, max: 15_000 }

const SEEN_KEY = 'shepherd.seen'

export interface Clock {
  now(): number
  hidden(): boolean
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(id: unknown): void
}

const browserClock: Clock = {
  now: () => Date.now(),
  hidden: () => typeof document !== 'undefined' && document.visibilityState === 'hidden',
  setTimeout: (fn, ms) => window.setTimeout(fn, ms),
  clearTimeout: (id) => window.clearTimeout(id as number),
}

export class Live {
  private snap: Snapshot
  private listeners = new Set<() => void>()
  private feedTimer: unknown = null
  private listsTimer: unknown = null
  private last = -1
  private stopped = true
  private listsInFlight: Promise<void> | null = null
  /** Consecutive failed reads; 0 while the daemon answers. */
  private failures = 0
  private readonly api: Api
  private readonly clock: Clock

  constructor(apiImpl: Api = realApi, clock: Clock = browserClock) {
    this.api = apiImpl
    this.clock = clock
    this.snap = {
      status: null, lanes: [], runs: [], decisions: [], requests: [], spend: null, feed: [],
      error: null, ready: false, updated: 0, seen: loadSeen(clock.now()),
    }
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  getSnapshot = () => this.snap

  private set(patch: Partial<Snapshot>) {
    this.snap = { ...this.snap, ...patch }
    for (const fn of this.listeners) fn()
  }

  start() {
    if (!this.stopped) return
    this.stopped = false
    void this.loadFeed().then(() => this.scheduleFeed())
    void this.refresh().then(() => this.scheduleLists())
  }

  stop() {
    this.stopped = true
    this.clock.clearTimeout(this.feedTimer)
    this.clock.clearTimeout(this.listsTimer)
  }

  /** Poll now, as when the tab becomes visible again. */
  wake() {
    if (this.stopped) return
    this.clock.clearTimeout(this.feedTimer)
    this.clock.clearTimeout(this.listsTimer)
    void this.pollFeed().then(() => this.scheduleFeed())
    void this.refresh().then(() => this.scheduleLists())
  }

  private scheduleFeed() {
    if (this.stopped) return
    const ms = this.failures > 0 ? this.retryIn() : this.clock.hidden() ? INTERVALS.feedHidden : INTERVALS.feedVisible
    this.feedTimer = this.clock.setTimeout(() => void this.pollFeed().then(() => this.scheduleFeed()), ms)
  }

  private scheduleLists() {
    if (this.stopped) return
    const ms = this.failures > 0 ? this.retryIn() : this.clock.hidden() ? INTERVALS.listsHidden : INTERVALS.listsVisible
    this.listsTimer = this.clock.setTimeout(() => void this.refresh().then(() => this.scheduleLists()), ms)
  }

  private retryIn() {
    return Math.min(BACKOFF.min * 2 ** (this.failures - 1), BACKOFF.max)
  }

  private ok() {
    this.failures = 0
  }

  /** The feed poll alone counts failures, so the two polls share one backoff. */
  private bad(e: unknown, counts = false) {
    if (counts) this.failures++
    this.set({ error: message(e) })
  }

  /** Reads the newest window of the feed, and where to poll from. */
  private async loadFeed() {
    try {
      const { last } = await this.api.feedLatest()
      let after = Math.max(0, last - FEED_WINDOW)
      const feed: FeedEntry[] = []
      while (after < last) {
        const page = await this.api.feed(after)
        if (page.items.length === 0 || page.last <= after) break
        feed.push(...entries(page).filter((e) => e.id <= last))
        after = page.last
      }
      this.last = last
      this.ok()
      this.set({ feed, error: null })
    } catch (e) {
      this.bad(e, true)
    }
  }

  private async pollFeed() {
    if (this.last < 0) return this.loadFeed()
    try {
      const page = await this.api.feed(this.last)
      this.ok()
      if (page.items.length === 0) {
        if (this.snap.error) this.set({ error: null })
        return
      }
      this.last = page.last
      const fresh = entries(page)
      let runs = this.snap.runs
      for (const e of fresh) runs = noteOutcome(runs, e.event, e.ref, e.created)
      this.set({ feed: [...this.snap.feed, ...fresh].slice(-FEED_WINDOW), runs, error: null })
      void this.refresh()
    } catch (e) {
      this.bad(e, true)
    }
  }

  /** Reloads the lists; concurrent calls share one load. */
  refresh(): Promise<void> {
    this.listsInFlight ??= this.loadLists().finally(() => {
      this.listsInFlight = null
    })
    return this.listsInFlight
  }

  private async loadLists() {
    try {
      const status = await this.api.status()
      const [laneLists, runs, decisions, requests, spend] = await Promise.all([
        Promise.all(status.workspaces.map((w) => this.api.lanes(w.id))),
        this.api.runs(),
        this.api.decisions('open'),
        this.api.requests('needs_routing'),
        this.api.spend(),
      ])
      this.set({
        status, lanes: laneLists.flat(), runs, decisions, requests, spend,
        error: null, ready: true, updated: this.clock.now(),
      })
    } catch (e) {
      this.bad(e)
    }
  }

  /** The person has opened something that finished: a run's log, a lane. */
  markSeen(key: string) {
    if (this.snap.seen.keys.has(key)) return
    const seen = { until: this.snap.seen.until, keys: new Set([...this.snap.seen.keys, key]) }
    saveSeen(seen)
    this.set({ seen })
  }

  /** Everything finished so far is seen. */
  clearDone() {
    const seen = { until: new Date(this.clock.now()).toISOString(), keys: new Set<string>() }
    saveSeen(seen)
    this.set({ seen })
  }

  /** Answers a decision with the person's words, then reloads so it leaves the board. */
  async answer(id: number, words: string) {
    const d = await this.api.answer(id, words)
    await this.refresh()
    return d
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError && e.status === 401) {
    return `signed out (a daemon restart ends every browser session). ${SIGN_IN_HINT}`
  }
  return e instanceof Error ? e.message : String(e)
}

// What was seen survives a reload; the first visit starts with nothing unseen, as the
// terminal does when the chat starts.
function loadSeen(now: number): Seen {
  try {
    const raw = localStorage.getItem(SEEN_KEY)
    if (raw) {
      const v = JSON.parse(raw) as { until?: string; keys?: string[] }
      if (v.until) return { until: v.until, keys: new Set(v.keys ?? []) }
    }
  } catch {
    // no storage, or not ours: start fresh
  }
  const seen = { until: new Date(now).toISOString(), keys: new Set<string>() }
  saveSeen(seen)
  return seen
}

function saveSeen(seen: Seen) {
  try {
    // Keys only matter for things finished after `until`; keep the list short.
    localStorage.setItem(SEEN_KEY, JSON.stringify({ until: seen.until, keys: [...seen.keys].slice(-500) }))
  } catch {
    // storage full or blocked: seen state lasts for this tab only
  }
}
