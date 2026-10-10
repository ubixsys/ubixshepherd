// The daemon's HTTP API, by relative URL so the app works unchanged wherever the daemon
// serves it. Authentication is the server's business: the dev proxy adds the token, and
// when the daemon serves the app the browser holds a session cookie (from the link
// `shepherd web` opens). Nothing here ever holds a token. A cookie session's writes carry
// its CSRF token, read once from /v1/web/session.
import type {
  Decision, DecisionView, Feed, LaneRecord, LaneView, RequestView, RunEvents, RunHistory, RunLog, RunView, SpendToday, Status,
} from './types'

export class ApiError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export const CSRF_HEADER = 'X-Shepherd-CSRF'

/** What to do about a signed-out session; a daemon restart ends every browser session. */
export const SIGN_IN_HINT = 'Run `shepherd web` in a terminal to open a new sign-in link.'

/** How long a request may take before it counts as lost: a hung daemon is an offline one. */
export const REQUEST_TIMEOUT = 6000

/** The daemon did not answer in time, or could not be reached at all. */
export class NetworkError extends Error {}

let csrf: Promise<string> | null = null

/** The session's CSRF token: "" through the dev proxy, which uses a token instead. */
function csrfToken(): Promise<string> {
  csrf ??= call<{ csrf?: string }>('/v1/web/session').then(
    (s) => s.csrf ?? '',
    () => {
      csrf = null
      return ''
    },
  )
  return csrf
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const write = init?.method !== undefined && init.method !== 'GET'
  const token = write ? await csrfToken() : ''
  // The timer covers the body too: a daemon that sends headers and stalls is as lost.
  const ctl = new AbortController()
  let timedOut = false
  const timer = setTimeout(() => {
    timedOut = true
    ctl.abort()
  }, REQUEST_TIMEOUT)
  try {
    return await exchange<T>(path, init, token, ctl.signal)
  } catch (e) {
    if (e instanceof ApiError) throw e
    throw new NetworkError(timedOut ? 'the daemon did not answer in time' : `the daemon could not be reached (${e instanceof Error ? e.message : String(e)})`)
  } finally {
    clearTimeout(timer)
  }
}

async function exchange<T>(path: string, init: RequestInit | undefined, token: string, signal: AbortSignal): Promise<T> {
  const res = await fetch(path, {
    ...init,
    signal,
    headers: {
      Accept: 'application/json',
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(token ? { [CSRF_HEADER]: token } : {}),
    },
  })
  // A session that ended (signed out, expired, a daemon restart) has a new token next time.
  if (res.status === 401 || res.status === 403) csrf = null
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = (await res.json()) as { error?: string }
      if (body.error) msg = body.error
    } catch {
      // not JSON: keep the status line
    }
    throw new ApiError(res.status, msg)
  }
  return (await res.json()) as T
}

export const get = <T>(path: string) => call<T>(path)
export const post = <T>(path: string, body: unknown) => call<T>(path, { method: 'POST', body: JSON.stringify(body) })
export const qs = (params: Record<string, string | number>) => new URLSearchParams(Object.entries(params).map(([k, v]) => [k, String(v)])).toString()

/** How many runs the board reads: enough to know every open lane's latest run. */
export const RECENT_RUNS = 200

export const api = {
  status: () => get<Status>('/v1/status'),
  lanes: (workspaceId: number) => get<LaneView[]>(`/v1/lanes?${qs({ workspace_id: workspaceId })}`),
  runs: (laneId = 0, limit = RECENT_RUNS) => get<RunView[]>(`/v1/runs?${qs({ lane_id: laneId, limit })}`),
  run: (id: number) => get<RunView>(`/v1/runs/${id}`),
  runLog: (id: number, offset: number) => get<RunLog>(`/v1/runs/${id}/log?${qs({ offset })}`),
  runEvents: (id: number) => get<RunEvents>(`/v1/runs/${id}/events`),
  /** state "" lists every decision. */
  decisions: (state: '' | 'open' | 'answered') => get<DecisionView[]>(`/v1/decisions?${qs({ state })}`),
  requests: (states: string) => get<RequestView[]>(`/v1/requests?${qs({ state: states })}`),
  spend: () => get<SpendToday>('/v1/spend'),
  /** The newest feed id only. */
  feedLatest: () => get<Feed>('/v1/feed?after=latest'),
  feed: (after: number) => get<Feed>(`/v1/feed?${qs({ after })}`),
  /** The person's words, sent only from their own click. */
  answer: (decisionId: number, answer: string) => post<Decision>(`/v1/decisions/${decisionId}/answer`, { answer }),
}

/** What ran and how it ended, closed lanes included. `since` is an RFC 3339 time. */
export const history = {
  /** Closed lanes by default, newest closed first; one lane by id whatever its state. */
  lanes: (workspaceId: number, since: string, state: 'closed' | 'open' | 'all' = 'closed') =>
    get<LaneRecord[]>(`/v1/history/lanes?${qs({ workspace_id: workspaceId, state, since })}`),
  lane: (laneId: number) => get<LaneRecord[]>(`/v1/history/lanes?${qs({ lane_id: laneId })}`),
  runs: (workspaceId: number, since: string, agent = '', laneId = 0) =>
    get<RunHistory>(
      `/v1/history/runs?${qs({ workspace_id: workspaceId, since, ...(agent ? { agent } : {}), ...(laneId ? { lane_id: laneId } : {}) })}`,
    ),
}
