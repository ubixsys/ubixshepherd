import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { LaneRecord, RunHistory, RunRecord } from '../api/types'
import { boardItems } from '../model/board'
import { LiveProvider } from '../state/context'
import { Live, type Api, type Clock } from '../state/live'
import { spend, status } from '../test/fixtures'
import { BoardPage } from './Board'
import { LanePage } from './Lane'
import { RunsPage } from './Runs'

const NOW = Date.parse('2026-10-10T15:00:00Z')
const still: Clock = { now: () => NOW, hidden: () => false, setTimeout: () => 0, clearTimeout: () => {} }

function closedLane(o: Partial<LaneRecord>): LaneRecord {
  return {
    id: 1, repo_id: 1, name: 'feat/login', branch: 'feat/login', base: 'dev', worktree: '/w', scope: [],
    state: 'closed', created: '2026-10-09T10:00:00Z', closed: '2026-10-10T13:00:00Z', origin: {}, repo: 'acme-api',
    outcome: 'merged', mr: 34, mr_state: 'merged', mr_url: 'https://forge.test/acme/-/merge_requests/34',
    runs: 3, cost_usd: 1.24, ...o,
  }
}

function runRecord(o: Partial<RunRecord>): RunRecord {
  return {
    id: 10, lane_id: 1, repo: 'acme-api', lane: 'feat/login', lane_state: 'closed', agent: 'claude', state: 'succeeded',
    commits: 2, cost_usd: 0.5, started: '2026-10-10T12:00:00Z', ended: '2026-10-10T12:04:12Z', task: 'Fix the login redirect', ...o,
  }
}

const lanes: LaneRecord[] = [
  closedLane({ id: 1 }),
  closedLane({ id: 2, name: 'chore/deps', outcome: 'no_mr', mr: undefined, mr_state: undefined, mr_url: undefined, runs: 1, cost_usd: 0, closed: '2026-10-10T14:00:00Z' }),
  closedLane({ id: 3, name: 'spike', outcome: 'dropped', mr_state: 'open', runs: 2, cost_usd: 0.3, closed: '2026-10-10T09:00:00Z' }),
  closedLane({ id: 4, name: 'old', outcome: 'mr_closed', mr_state: 'closed', closed: '2026-10-05T09:00:00Z' }),
]

const runs: RunHistory = {
  runs: [
    runRecord({ id: 12, lane_id: 2, lane: 'chore/deps', agent: 'copilot', state: 'running', ended: undefined, commits: 0, cost_usd: undefined, credits: 3, task: 'Bump the modules' }),
    runRecord({ id: 11, state: 'failed', agent: 'claude', task: 'Second attempt' }),
    runRecord({ id: 10 }),
  ],
  count: 3, cost_usd: 1, credits: 3, agents: ['claude', 'copilot'],
  lanes: [{ id: 1, repo: 'acme-api', name: 'feat/login' }, { id: 2, repo: 'acme-api', name: 'chore/deps' }],
}

/** The daemon's history answers, and the requests the page made. */
function daemon() {
  const seen: URL[] = []
  vi.stubGlobal('fetch', async (path: string) => {
    const url = new URL(path, 'http://x')
    seen.push(url)
    if (url.pathname === '/v1/history/lanes') {
      const since = Date.parse(url.searchParams.get('since') ?? '')
      const body = lanes.filter((l) => Date.parse(l.closed as string) >= since)
      return new Response(JSON.stringify(body), { status: 200 })
    }
    if (url.pathname === '/v1/history/runs') {
      const agent = url.searchParams.get('agent')
      const lane = Number(url.searchParams.get('lane_id') ?? 0)
      const rs = runs.runs.filter((r) => (!agent || r.agent === agent) && (!lane || r.lane_id === lane))
      const body: RunHistory = { ...runs, runs: rs, count: rs.length, cost_usd: rs.length === 3 ? 1 : 0.5 }
      return new Response(JSON.stringify(body), { status: 200 })
    }
    return new Response('{}', { status: 404 })
  })
  return seen
}

async function liveReady() {
  const api: Api = {
    status: async () => status(),
    lanes: async () => [],
    runs: async () => [],
    run: async () => { throw new Error('unused') },
    runLog: async () => { throw new Error('unused') },
    runEvents: async () => { throw new Error('unused') },
    decisions: async () => [],
    requests: async () => [],
    spend: async () => spend(),
    feedLatest: async () => ({ items: [], events: [], last: 0 }),
    feed: async (after) => ({ items: [], events: [], last: after }),
    answer: async () => { throw new Error('unused') },
  }
  const live = new Live(api, still)
  await act(() => live.refresh())
  return live
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(NOW)
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('the Finished section of the board', () => {
  async function show(range?: 'today' | 'week') {
    const live = await liveReady()
    const items = boardItems({ ...live.getSnapshot(), lanes: [], runs: [], decisions: [], requests: [] })
    render(
      <LiveProvider live={live}>
        <BoardPage items={items} range={range} />
      </LiveProvider>,
    )
    return screen.findByRole('region', { name: /Finished/ })
  }

  it('lists the lanes closed today, newest first, with how each ended', async () => {
    const seen = daemon()
    const section = await show()
    const rows = await within(section).findAllByRole('listitem')
    expect(rows).toHaveLength(3)
    expect(rows[0]).toHaveTextContent('chore/deps')
    expect(rows[0]).toHaveTextContent('closed, no MR')
    expect(rows[0]).toHaveTextContent('1 run')
    expect(rows[1]).toHaveTextContent('feat/login')
    expect(rows[1]).toHaveTextContent('merged !34')
    expect(rows[1]).toHaveTextContent('3 runs')
    expect(rows[1]).toHaveTextContent('$1.24')
    expect(rows[2]).toHaveTextContent('spike')
    expect(rows[2]).toHaveTextContent('dropped')
    expect(rows[2]).toHaveTextContent('$0.30')
    // The merge request links out; the lane's name opens the lane page.
    expect(within(rows[1] as HTMLElement).getByRole('link', { name: '!34' })).toHaveAttribute('href', 'https://forge.test/acme/-/merge_requests/34')
    expect(within(rows[1] as HTMLElement).getByRole('link', { name: 'feat/login' })).toHaveAttribute('href', '#/lanes/1')
    // Today starts at local midnight.
    const asked = seen.find((u) => u.pathname === '/v1/history/lanes')
    const midnight = new Date(NOW)
    midnight.setHours(0, 0, 0, 0)
    expect(asked?.searchParams.get('since')).toBe(midnight.toISOString())
    expect(asked?.searchParams.get('state')).toBe('closed')
  })

  it('widens to the last 7 days', async () => {
    daemon()
    const section = await show('week')
    expect(await within(section).findAllByRole('listitem')).toHaveLength(4)
    expect(within(section).getByText('MR closed')).toBeInTheDocument()
    expect(within(section).getByRole('link', { name: 'Last 7 days' })).toHaveAttribute('aria-current', 'true')
    expect(within(section).getByRole('link', { name: 'Today' })).toHaveAttribute('href', '#/?range=today')
  })

  it('says so when nothing closed in the range', async () => {
    vi.stubGlobal('fetch', async () => new Response('[]', { status: 200 }))
    const section = await show()
    expect(await within(section).findByText('No lane closed today.')).toBeInTheDocument()
  })

  it('shows why the history is missing rather than an empty list', async () => {
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ error: 'not found' }), { status: 404 }))
    const section = await show()
    expect(await within(section).findByText(/Could not load closed lanes: not found/)).toBeInTheDocument()
  })
})

describe('the Runs page', () => {
  async function show(props: Parameters<typeof RunsPage>[0] = {}) {
    const live = await liveReady()
    render(
      <LiveProvider live={live}>
        <RunsPage {...props} />
      </LiveProvider>,
    )
  }

  it('lists every run with its lane, outcome, commits, cost and task, and the spend', async () => {
    daemon()
    await show()
    const table = await screen.findByRole('table')
    const body = within(table).getAllByRole('row').slice(1)
    expect(body).toHaveLength(3)
    expect(body[0]).toHaveTextContent('12')
    expect(body[0]).toHaveTextContent('copilot')
    expect(body[0]).toHaveTextContent('running')
    expect(body[0]).toHaveTextContent('3 credits')
    expect(body[0]).toHaveTextContent('Bump the modules')
    expect(body[1]).toHaveTextContent('failed')
    expect(body[2]).toHaveTextContent('succeeded')
    expect(body[2]).toHaveTextContent('$0.50')
    expect(body[2]).toHaveTextContent('4m 12s')
    expect(body[2]).toHaveTextContent('Fix the login redirect')
    // A run links to its log and its lane.
    expect(within(body[2] as HTMLElement).getByRole('link', { name: '10' })).toHaveAttribute('href', '#/runs/10/log')
    expect(within(body[2] as HTMLElement).getByRole('link', { name: /feat\/login/ })).toHaveAttribute('href', '#/lanes/1')
    expect(screen.getByText(/spent today/)).toHaveTextContent('$1.00 + 3 credits spent today')
    expect(screen.getByText(/across 3 runs/)).toBeInTheDocument()
  })

  it('asks the daemon for the range and the filters', async () => {
    const seen = daemon()
    await show({ range: 'week', agent: 'claude', lane: 1 })
    await screen.findByRole('table')
    const asked = seen.find((u) => u.pathname === '/v1/history/runs')
    expect(asked?.searchParams.get('agent')).toBe('claude')
    expect(asked?.searchParams.get('lane_id')).toBe('1')
    expect(Date.parse(asked?.searchParams.get('since') as string)).toBe(NOW - 7 * 24 * 3600 * 1000)
    expect(await screen.findByText(/matching the filters/)).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: 'Agent' })).toHaveValue('claude')
    expect(screen.getByRole('combobox', { name: 'Lane' })).toHaveValue('1')
  })

  it('puts a filter choice in the address', async () => {
    daemon()
    await show()
    await screen.findByRole('table')
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Agent' }), 'copilot')
    await waitFor(() => expect(window.location.hash).toBe('#/runs?range=today&agent=copilot'))
    window.location.hash = ''
  })

  it('says when nothing ran', async () => {
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ ...runs, runs: [], count: 0, cost_usd: 0, credits: 0 }), { status: 200 }))
    await show()
    expect(await screen.findByText('No run started in today.')).toBeInTheDocument()
    expect(screen.getByText(/\$0\.00/)).toBeInTheDocument()
  })
})

describe('the Lane page for a closed lane', () => {
  it('shows its scope, merge request and how it ended, from the history', async () => {
    vi.stubGlobal('fetch', async (path: string) => {
      const url = new URL(path, 'http://x')
      if (url.pathname === '/v1/history/lanes') {
        expect(url.searchParams.get('lane_id')).toBe('1')
        return new Response(JSON.stringify([closedLane({ scope: ['src/auth/**'] })]), { status: 200 })
      }
      return new Response('[]', { status: 200 })
    })
    const live = await liveReady()
    render(
      <LiveProvider live={live}>
        <LanePage id={1} conversation={() => ({ read: async () => ({ items: [], cursor: 0, more: false, running: false }), expand: async () => { throw new Error('unused') } })} />
      </LiveProvider>,
    )
    expect(await screen.findByRole('heading', { level: 1, name: /feat\/login/ })).toBeInTheDocument()
    expect(await screen.findByText('src/auth/**')).toBeInTheDocument()
    expect(screen.getByText(/closed .* ago/)).toBeInTheDocument()
    expect(screen.getByText('merged', { selector: 'span[title]' })).toBeInTheDocument()
    expect(screen.queryByText(/not listed by the daemon/)).not.toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Conversation' })).toBeInTheDocument()
  })
})
