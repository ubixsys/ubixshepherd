import type { UsageGroup } from '../api/types'
import { chartMax, chartSections, DEFAULT_SORT, nextSort, sortGroups, usageDays } from './usage'

const NOW = new Date(2026, 9, 10, 15, 0).getTime()

function group(key: string, o: Partial<UsageGroup> = {}): UsageGroup {
  return {
    key, count: 1, measured: 1, input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0,
    peak_context: 0, over_200k: 0, compactions: 0, cost_usd: 0, ...o,
  }
}

describe('usageDays', () => {
  it('names the days of each preset, ending today', () => {
    expect(usageDays('today', NOW)).toEqual({ from: '2026-10-10', to: '2026-10-10' })
    expect(usageDays('week', NOW)).toEqual({ from: '2026-10-04', to: '2026-10-10' })
    expect(usageDays('month', NOW)).toEqual({ from: '2026-09-11', to: '2026-10-10' })
  })

  it('takes a custom pair, repairs a bad or reversed one', () => {
    expect(usageDays('custom', NOW, '2026-10-01', '2026-10-07')).toEqual({ from: '2026-10-01', to: '2026-10-07' })
    expect(usageDays('custom', NOW, '2026-10-07', '2026-10-01')).toEqual({ from: '2026-10-01', to: '2026-10-07' })
    expect(usageDays('custom', NOW, '2026-02-31', undefined)).toEqual({ from: '2026-10-04', to: '2026-10-10' })
  })
})

describe('sorting', () => {
  const gs = [group('b', { cost_usd: 1, peak_context: 90 }), group('a', { cost_usd: 5, peak_context: 10 }), group('c', { cost_usd: 1, peak_context: 50 })]

  it('defaults to the biggest cost first and keeps the daemon order on a tie', () => {
    expect(sortGroups(gs, DEFAULT_SORT).map((g) => g.key)).toEqual(['a', 'b', 'c'])
  })

  it('sorts names ascending first and flips on a second click', () => {
    const byName = nextSort(DEFAULT_SORT, 'name')
    expect(sortGroups(gs, byName).map((g) => g.key)).toEqual(['a', 'b', 'c'])
    expect(sortGroups(gs, nextSort(byName, 'name')).map((g) => g.key)).toEqual(['c', 'b', 'a'])
    expect(sortGroups(gs, nextSort(DEFAULT_SORT, 'peak_context')).map((g) => g.key)).toEqual(['b', 'c', 'a'])
  })
})

describe('chart', () => {
  it('leaves out groups with no measured peak and orders by peak', () => {
    const s = chartSections(
      [group('claude sonnet', { peak_context: 90_000 }), group('copilot', { measured: 0 }), group('claude opus[1m]', { peak_context: 350_000, over_200k: 2, context_window: 1_000_000 })],
      [],
      [group('r/x', { repo: 'r', lane: 'x', peak_context: 10_000 })],
    )
    expect(s.map((x) => x.title)).toEqual(['Agent runs, by agent and model', 'Agent runs, by lane'])
    expect(s[0]?.rows.map((r) => r.key)).toEqual(['claude opus[1m]', 'claude sonnet'])
    expect(s[1]?.rows[0]).toMatchObject({ label: 'x', prefix: 'r' })
  })

  it('scales to the 200k line at least, not to a window far above the peaks', () => {
    expect(chartMax([])).toBeCloseTo(220_000)
    const s = chartSections([group('m', { peak_context: 400_000, context_window: 1_000_000 })], [], [])
    expect(chartMax(s)).toBeCloseTo(440_000)
  })
})
