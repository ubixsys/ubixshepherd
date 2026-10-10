// The browser's view of the daemon-hosted front desk: the conversation from
// GET /v1/desk/history, followed over /v1/desk/stream with an EventSource, and the
// person's actions on it. Like internal/client's FollowDesk: a stored event carries a seq
// and is resumed from; a "partial" piece has none, is never stored, and the whole reply
// follows as an assistant event. An EventSource cannot send headers, so it resumes by
// ?after=, and the cookie carries the session.
import { ApiError, NetworkError, SIGN_IN_HINT } from '../api/client'
import { deskApi, type DeskApi } from '../api/desk'
import type { DeskEvent } from '../api/types'
import type { FeedEntry } from '../model/feed'
import type { EventKind } from '../model/marks'

export type Connection = 'connecting' | 'live' | 'reconnecting' | 'signed-out' | 'refused'

/** Why an action did not go through. `full` is the daemon's 429. */
export interface Problem {
  kind: 'full' | 'signed-out' | 'refused' | 'other'
  message: string
}

export interface DeskSnapshot {
  /** Stored events by seq, oldest first. */
  events: DeskEvent[]
  /** The reply streaming now, pieces joined; empty when none. */
  partial: string
  loaded: boolean
  /** Older events remain on the daemon. */
  more: boolean
  connection: Connection
  /** While reconnecting: when the next attempt runs (ms since the epoch), else null. */
  retryAt: number | null
  /** Attempts that failed since the connection was last live. */
  failures: number
  busy: boolean
  queued: number
  sending: boolean
  /** The last send, interrupt or new conversation that failed. */
  problem: Problem | null
  /** A short result worth saying, such as nothing being there to interrupt. */
  notice: string | null
  /** History could not be read: what the daemon said. */
  loadError: string | null
}

/** The part of EventSource the thread uses, so a test can stand one in. */
export interface Source {
  onopen: ((ev: Event) => void) | null
  onerror: ((ev: Event) => void) | null
  addEventListener(type: string, fn: (ev: MessageEvent<string>) => void): void
  close(): void
}

export interface DeskDeps {
  api: DeskApi
  open(url: string): Source
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(id: unknown): void
  now(): number
  /** Calls fn when the tab becomes visible or the browser comes online; returns the undo. */
  onWake(fn: () => void): () => void
}

const browserDeps: DeskDeps = {
  api: deskApi,
  open: (url) => new EventSource(url),
  setTimeout: (fn, ms) => window.setTimeout(fn, ms),
  clearTimeout: (id) => window.clearTimeout(id as number),
  now: () => Date.now(),
  onWake: (fn) => {
    const onVisible = () => {
      if (document.visibilityState === 'visible') fn()
    }
    document.addEventListener('visibilitychange', onVisible)
    window.addEventListener('online', fn)
    return () => {
      document.removeEventListener('visibilitychange', onVisible)
      window.removeEventListener('online', fn)
    }
  },
}

export const BACKOFF = { min: 1000, max: 15_000 }
/**
 * An EventSource shows neither a stalled stream nor a daemon that went away without
 * closing it (its ": ping" comments never reach script), so while the stream is live the
 * thread asks the daemon's status this often. A request that hangs counts as a failure.
 */
export const PROBE_EVERY = 3000
/** How long a stream may take to open before it counts as lost. */
export const OPEN_TIMEOUT = 8000
/** How many stored events the page keeps; older ones are reachable by "load earlier". */
export const KEEP = 2000
const PAGE = 100
const STORED = ['user', 'system', 'turn_start', 'assistant', 'tool', 'cost', 'error', 'turn_end', 'new']

export { SIGN_IN_HINT }

export class DeskThread {
  private snap: DeskSnapshot = {
    events: [], partial: '', loaded: false, more: false, connection: 'connecting', retryAt: null, failures: 0,
    busy: false, queued: 0, sending: false, problem: null, notice: null, loadError: null,
  }
  private listeners = new Set<() => void>()
  private source: Source | null = null
  private timer: unknown = null
  private probeTimer: unknown = null
  private unwake: (() => void) | null = null
  private backoff = BACKOFF.min
  private after = 0
  private stopped = true
  /** Bumped by every new attempt, so an answer to an older one is ignored. */
  private epoch = 0
  private readonly deps: DeskDeps

  constructor(readonly workspaceId = 0, deps: Partial<DeskDeps> = {}) {
    this.deps = { ...browserDeps, ...deps }
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  getSnapshot = () => this.snap

  private set(patch: Partial<DeskSnapshot>) {
    this.snap = { ...this.snap, ...patch }
    for (const fn of this.listeners) fn()
  }

  start() {
    if (!this.stopped) return
    this.stopped = false
    this.unwake = this.deps.onWake(() => this.wake())
    void this.resync()
  }

  stop() {
    this.stopped = true
    this.epoch++
    this.deps.clearTimeout(this.timer)
    this.deps.clearTimeout(this.probeTimer)
    this.unwake?.()
    this.unwake = null
    this.source?.close()
    this.source = null
  }

  /** The tab is visible or the network is back: try now rather than wait out the backoff. */
  wake() {
    if (this.stopped) return
    const c = this.snap.connection
    if (c === 'live') {
      this.deps.clearTimeout(this.probeTimer)
      void this.probe()
    } else if (c !== 'connecting' || this.timer !== null) {
      this.backoff = BACKOFF.min
      void this.resync()
    }
  }

  /** Reads the newest page, merges it with what the page holds, then follows the stream. */
  private async resync() {
    this.deps.clearTimeout(this.timer)
    this.timer = null
    this.deps.clearTimeout(this.probeTimer)
    this.source?.close()
    this.source = null
    const epoch = ++this.epoch
    try {
      const h = await this.deps.api.history(this.workspaceId, 0, PAGE)
      if (this.stopped || epoch !== this.epoch) return
      const held = this.snap.events
      const newest = held.at(-1)?.seq ?? 0
      const first = h.events[0]?.seq ?? 0
      // A page that starts above what the page holds leaves a gap: take the daemon's page.
      const gap = newest > 0 && h.more && first > newest + 1
      const events = gap ? h.events : mergeEvents(held, h.events).slice(-KEEP)
      this.after = Math.max(this.after, events.at(-1)?.seq ?? 0)
      this.set({ events, more: gap || !this.snap.loaded ? h.more : this.snap.more, loaded: true, loadError: null, partial: '' })
      this.connect()
      void this.refreshStatus()
    } catch (e) {
      if (this.stopped || epoch !== this.epoch) return
      const p = problemOf(e)
      if (p.kind === 'signed-out' || p.kind === 'refused') this.end(p)
      else this.lost(p.message)
    }
  }

  /** Reads older events, above the oldest on the page. */
  async loadEarlier() {
    const first = this.snap.events[0]?.seq
    if (!first || !this.snap.more) return
    try {
      const h = await this.deps.api.history(this.workspaceId, first, PAGE)
      this.set({ events: mergeEvents(h.events, this.snap.events), more: h.more })
    } catch (e) {
      this.set({ problem: problemOf(e) })
    }
  }

  private connect() {
    if (this.stopped) return
    this.source?.close()
    const src = this.deps.open(this.deps.api.streamUrl(this.workspaceId, this.after))
    this.source = src
    // A stream that neither opens nor fails is lost too.
    this.deps.clearTimeout(this.probeTimer)
    this.probeTimer = this.deps.setTimeout(() => {
      if (this.source === src) this.lost('the event stream did not open in time')
    }, OPEN_TIMEOUT)
    src.onopen = () => {
      if (this.source !== src) return
      this.backoff = BACKOFF.min
      this.set({ connection: 'live', retryAt: null, failures: 0 })
      void this.refreshStatus()
      this.scheduleProbe()
    }
    // EventSource hides why it failed: leave it to the next history read, which says 401.
    src.onerror = () => {
      if (this.source !== src) return
      this.lost('the event stream closed')
    }
    for (const kind of STORED) src.addEventListener(kind, (ev) => this.onEvent(kind, ev.data))
    src.addEventListener('partial', (ev) => this.onEvent('partial', ev.data))
  }

  private scheduleProbe() {
    this.deps.clearTimeout(this.probeTimer)
    this.probeTimer = this.deps.setTimeout(() => void this.probe(), PROBE_EVERY)
  }

  /** While live, asks the daemon's status: the answer, or its absence, is the heartbeat. */
  private async probe() {
    const src = this.source
    if (this.stopped || !src || this.snap.connection !== 'live') return
    try {
      const s = await this.deps.api.status(this.workspaceId)
      if (this.stopped || this.source !== src) return
      this.set({ busy: s.busy, queued: s.queued })
      this.scheduleProbe()
    } catch (e) {
      if (this.stopped || this.source !== src) return
      const p = problemOf(e)
      if (p.kind === 'signed-out' || p.kind === 'refused') this.end(p)
      else this.lost(p.message)
    }
  }

  /** The connection is lost: say so, drop the stream, retry with backoff. */
  private lost(why: string) {
    if (this.stopped) return
    this.epoch++
    this.source?.close()
    this.source = null
    this.deps.clearTimeout(this.probeTimer)
    this.set({ connection: 'reconnecting', partial: '', loadError: this.snap.loaded ? this.snap.loadError : why, failures: this.snap.failures + 1 })
    this.retry(() => void this.resync())
  }

  /** The session cannot continue: say what to do and stop trying until the person acts. */
  private end(p: Problem) {
    this.epoch++
    this.source?.close()
    this.source = null
    this.deps.clearTimeout(this.timer)
    this.timer = null
    this.deps.clearTimeout(this.probeTimer)
    this.set({ connection: p.kind === 'refused' ? 'refused' : 'signed-out', problem: p, loadError: p.message, retryAt: null, partial: '' })
  }

  private retry(fn: () => void) {
    if (this.stopped) return
    this.deps.clearTimeout(this.timer)
    this.timer = this.deps.setTimeout(fn, this.backoff)
    this.set({ retryAt: this.deps.now() + this.backoff })
    this.backoff = Math.min(this.backoff * 2, BACKOFF.max)
  }

  private onEvent(kind: string, data: string) {
    let e: DeskEvent
    try {
      e = JSON.parse(data) as DeskEvent
    } catch {
      return
    }
    if (kind === 'partial') {
      this.set({ partial: this.snap.partial + (e.text ?? '') })
      return
    }
    if (!(e.seq > 0) || this.snap.events.some((x) => x.seq === e.seq)) return
    this.after = Math.max(this.after, e.seq)
    const events = mergeEvents(this.snap.events, [e]).slice(-KEEP)
    // Any stored event ends the streaming reply: the whole one is an assistant event.
    const patch: Partial<DeskSnapshot> = { events, partial: '' }
    if (kind === 'turn_start') patch.busy = true
    if (kind === 'turn_end') {
      patch.busy = false
      void this.refreshStatus()
    }
    this.set(patch)
  }

  private async refreshStatus() {
    try {
      const s = await this.deps.api.status(this.workspaceId)
      if (!this.stopped) this.set({ busy: s.busy, queued: s.queued })
    } catch {
      // the probe and the stream decide the connection; this is a refinement
    }
  }

  /** Sends the person's words; true when the daemon accepted them. */
  async send(text: string): Promise<boolean> {
    if (this.snap.sending || !text.trim()) return false
    this.set({ sending: true, problem: null, notice: null })
    try {
      const a = await this.deps.api.turn(this.workspaceId, text)
      this.set({ sending: false, busy: true, queued: Math.max(0, a.ahead) })
      return true
    } catch (e) {
      this.fail(e, { sending: false })
      return false
    }
  }

  async interrupt() {
    this.set({ problem: null, notice: null })
    try {
      const r = await this.deps.api.interrupt(this.workspaceId)
      this.set({ notice: r.interrupted ? 'Interrupted.' : 'Nothing was running.' })
    } catch (e) {
      this.fail(e)
    }
  }

  /** The daemon rotates the session; the stream then carries a "new" event. */
  async newConversation(): Promise<boolean> {
    this.set({ problem: null, notice: null })
    try {
      await this.deps.api.newConversation(this.workspaceId)
      this.set({ notice: 'Started a new conversation.' })
      return true
    } catch (e) {
      this.fail(e)
      return false
    }
  }

  private fail(e: unknown, patch: Partial<DeskSnapshot> = {}) {
    const p = problemOf(e)
    if (p.kind === 'signed-out' || p.kind === 'refused') {
      this.end(p)
      this.set(patch)
      return
    }
    this.set({ ...patch, problem: p })
    // A write that never got an answer says the daemon is gone: do not wait for the probe.
    if (e instanceof NetworkError && this.snap.connection === 'live') this.lost(p.message)
  }
}

/** What an API failure means for the person. Nothing here holds a token or session. */
export function problemOf(e: unknown): Problem {
  if (e instanceof ApiError) {
    if (e.status === 429) {
      return { kind: 'full', message: 'The front desk already has as many messages waiting as it takes. Wait for one to finish, then send this again.' }
    }
    if (e.status === 401) return { kind: 'signed-out', message: `You are signed out (a daemon restart ends every browser session). ${SIGN_IN_HINT}` }
    if (e.status === 403) {
      return { kind: 'refused', message: `The daemon refused this: ${e.message}. Reload the page; if it persists, sign in again with \`shepherd web\`.` }
    }
    return { kind: 'other', message: e.message }
  }
  if (e instanceof NetworkError) return { kind: 'other', message: `Cannot reach the daemon: ${e.message}.` }
  return { kind: 'other', message: e instanceof Error ? e.message : String(e) }
}

/** Two seq-ordered lists as one, without repeats. */
export function mergeEvents(a: DeskEvent[], b: DeskEvent[]): DeskEvent[] {
  const bySeq = new Map<number, DeskEvent>()
  for (const e of [...a, ...b]) bySeq.set(e.seq, e)
  return [...bySeq.values()].sort((x, y) => x.seq - y.seq)
}

// Feed events worth a row in the conversation: what the person acts on or learns of. The
// rest (commits, gate steps, run starts) stay on the board and the lane's timeline.
const THREAD_EVENTS = new Set<EventKind>([
  'run_ended', 'run_passed', 'run_failed', 'run_interrupted', 'run_quota',
  'decision_asked', 'decision_answered', 'request_attention', 'budget', 'mr', 'release',
  'lane_opened', 'lane_closed',
])

export type ThreadItem =
  | { type: 'desk'; key: string; e: DeskEvent }
  | { type: 'feed'; key: string; f: FeedEntry }

/**
 * The conversation with typed event rows between its messages, in time order. Feed rows
 * start where the loaded thread does; a desk event wins a tie.
 */
export function timeline(events: DeskEvent[], feed: FeedEntry[]): ThreadItem[] {
  const out: ThreadItem[] = []
  const from = events.length ? Date.parse(events[0]?.created ?? '') : NaN
  const rows = Number.isNaN(from)
    ? []
    : feed.filter((f) => THREAD_EVENTS.has(f.event) && Date.parse(f.created) >= from)
  let i = 0
  for (const e of events) {
    const t = Date.parse(e.created)
    while (i < rows.length && Date.parse(rows[i]?.created ?? '') < t) {
      const f = rows[i++]
      if (f) out.push({ type: 'feed', key: `f${f.id}`, f })
    }
    out.push({ type: 'desk', key: `d${e.seq}`, e })
  }
  for (; i < rows.length; i++) {
    const f = rows[i]
    if (f) out.push({ type: 'feed', key: `f${f.id}`, f })
  }
  return out
}
