import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { UsageGroup, UsageStats, UsageTotals } from '../api/types'
import { LiveProvider } from '../state/context'
import { Live, type Api, type Clock } from '../state/live'
import { spend, status } from '../test/fixtures'
import { UsagePage } from './Usage'

const NOW = new Date(2026, 9, 10, 15, 0).getTime()
const still: Clock = { now: () => NOW, hidden: () => false, setTimeout: () => 0, clearTimeout: () => {} }

function totals(o: Partial<UsageTotals> = {}): UsageTotals {
  return {
    count: 0, measured: 0, input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0,
    peak_context: 0, over_200k: 0, compactions: 0, cost_usd: 0, ...o,
  }
}
const group = (key: string, o: Partial<UsageGroup> = {}): UsageGroup => ({ key, ...totals(o), ...o })

const stats: UsageStats = {
  from: '2026-10-04',
  to: '2026-10-10',
  total: totals({ count: 7, measured: 6, cost_usd: 14.2 }),
  runs: totals({ count: 5, measured: 4, cost_usd: 4.2, over_200k: 1, peak_context: 412_000 }),
  desk: totals({ count: 2, measured: 2, cost_usd: 10, over_200k: 0, peak_context: 160_000 }),
  by_model: [
    group('claude sonnet-4', { count: 3, measured: 3, cost_usd: 1.2, peak_context: 120_000, context_window: 200_000, input_tokens: 2100, cache_read_tokens: 18_000_000, output_tokens: 310_000 }),
    group('claude opus-4[1m]', { count: 1, measured: 1, cost_usd: 3, peak_context: 412_000, over_200k: 1, context_window: 1_000_000, compactions: 2 }),
    group('copilot', { count: 1, measured: 0, cost_usd: 0, credits: 3 }),
  ],
  by_lane: [group('acme/feat-x', { repo: 'acme', lane: 'feat-x', count: 4, measured: 4, cost_usd: 4.2, peak_context: 412_000, over_200k: 1 })],
  desk_by_model: [group('claude opus-4', { count: 2, measured: 2, cost_usd: 10, peak_context: 160_000, context_window: 200_000 })],
}

function daemon(body: UsageStats = stats) {
  const seen: URL[] = []
  vi.stubGlobal('fetch', async (path: string) => {
    const url = new URL(path, 'http://x')
    seen.push(url)
    if (url.pathname === '/v1/usage') return new Response(JSON.stringify(body), { status: 200 })
    return new Response('{}', { status: 404 })
  })
  return seen
}

async function show(props: Parameters<typeof UsagePage>[0] = {}) {
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
  render(
    <LiveProvider live={live}>
      <UsagePage {...props} />
    </LiveProvider>,
  )
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(NOW)
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('the Usage page', () => {
  it('asks for the last 7 days by default and shows the tiles', async () => {
    const seen = daemon()
    await show()
    await screen.findByText('Total cost')
    const req = seen.find((u) => u.pathname === '/v1/usage')
    expect(req?.searchParams.get('from')).toBe('2026-10-04')
    expect(req?.searchParams.get('to')).toBe('2026-10-10')
    const tiles = screen.getByText('Total cost').closest('dl') as HTMLElement
    expect(tiles).toHaveTextContent('$14.20')
    expect(tiles).toHaveTextContent('$10.00 / $4.20')
    expect(tiles).toHaveTextContent('the desk is 70% of dollars')
    expect(tiles).toHaveTextContent('5 / 2')
    expect(within(tiles).getByText('Over 200k').nextElementSibling).toHaveTextContent('1')
  })

  it('asks for the days of each range', async () => {
    const seen = daemon()
    await show({ range: 'month' })
    await screen.findByText('Total cost')
    expect(seen[0]?.searchParams.get('from')).toBe('2026-09-11')
    expect(seen[0]?.searchParams.get('to')).toBe('2026-10-10')
  })

  it('takes a custom range from the address and offers its days', async () => {
    const seen = daemon()
    await show({ range: 'custom', from: '2026-10-01', to: '2026-10-07' })
    await screen.findByText('Total cost')
    expect(seen[0]?.searchParams.get('from')).toBe('2026-10-01')
    expect(screen.getByLabelText('From')).toHaveValue('2026-10-01')
    expect(screen.getByLabelText('To')).toHaveValue('2026-10-07')
  })

  it('sorts the tables by a clicked column, and flips on a second click', async () => {
    daemon()
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    await show()
    const section = await screen.findByRole('region', { name: 'By agent and model' })
    const names = () => within(section).getAllByRole('row').slice(1).map((r) => within(r).getAllByRole('cell')[0]?.textContent)
    expect(names()).toEqual(['claude opus-4[1m]', 'claude sonnet-4', 'copilot'])
    await user.click(within(section).getByRole('button', { name: /Peak context/ }))
    expect(names()).toEqual(['claude opus-4[1m]', 'claude sonnet-4', 'copilot'])
    await user.click(within(section).getByRole('button', { name: /Peak context/ }))
    expect(names()).toEqual(['copilot', 'claude sonnet-4', 'claude opus-4[1m]'])
    expect(within(section).getByRole('columnheader', { name: /Peak context/ })).toHaveAttribute('aria-sort', 'ascending')
  })

  it('charts peak context for measured groups only, with the 200k count', async () => {
    daemon()
    await show()
    const chart = await screen.findByRole('figure', { name: 'Peak context by group' })
    const rows = within(chart).getAllByRole('listitem')
    expect(rows.map((r) => r.getAttribute('title'))).toContain('claude opus-4[1m]: peak 412k tokens of a 1M window, 1 of 1 over 200k')
    expect(within(chart).queryByText('copilot')).toBeNull()
    expect(chart).toHaveTextContent('none over 200k')
  })

  it('says so when nothing was recorded', async () => {
    daemon({ ...stats, total: totals() })
    await show()
    expect(await screen.findByText(/No run or desk turn was recorded/)).toBeInTheDocument()
  })
})
