import type { LaneRecord, RunHistory } from '../api/types'
import { ending, finished, mergeHistories, rangeStart } from './history'

const rec = (o: Partial<LaneRecord>): LaneRecord => ({
  id: 1, repo_id: 1, name: 'l', branch: 'l', base: 'dev', worktree: '/w', scope: [], state: 'closed',
  created: '2026-10-08T10:00:00Z', origin: {}, repo: 'r', runs: 1, cost_usd: 0, ...o,
})

describe('rangeStart', () => {
  it('is local midnight for today and seven days back for a week', () => {
    const now = new Date(2026, 9, 10, 15, 30).getTime()
    expect(rangeStart('today', now)).toEqual(new Date(2026, 9, 10, 0, 0, 0, 0))
    expect(rangeStart('week', now).getTime()).toBe(now - 7 * 24 * 3600 * 1000)
  })
})

describe('ending', () => {
  it('words each outcome', () => {
    expect(ending({ outcome: 'merged', mr: 4, mr_state: 'merged' }).text).toBe('merged')
    expect(ending({ outcome: 'mr_closed', mr: 4, mr_state: 'closed' }).text).toBe('MR closed')
    const dropped = ending({ outcome: 'dropped', mr: 4, mr_state: 'open' })
    expect(dropped.text).toBe('dropped')
    expect(dropped.title).toContain('!4')
    expect(ending({ outcome: 'no_mr' }).text).toBe('closed, no MR')
    expect(ending({}).text).toBe('closed, no MR')
  })
})

describe('finished', () => {
  it('keeps closed lanes closed since the cut-off, newest first', () => {
    const lanes = [
      rec({ id: 1, closed: '2026-10-10T09:00:00Z' }),
      rec({ id: 2, closed: '2026-10-10T11:00:00Z' }),
      rec({ id: 3, closed: '2026-10-09T11:00:00Z' }),
      rec({ id: 4, state: 'open' }),
    ]
    const got = finished(lanes, new Date('2026-10-10T00:00:00Z')).map((l) => l.id)
    expect(got).toEqual([2, 1])
  })
})

describe('mergeHistories', () => {
  const h = (o: Partial<RunHistory>): RunHistory => ({ runs: [], count: 0, cost_usd: 0, credits: 0, agents: [], lanes: [], ...o })
  it('adds totals and unions the choices', () => {
    const got = mergeHistories([
      h({ count: 2, cost_usd: 1.5, agents: ['claude'], lanes: [{ id: 1, repo: 'b', name: 'x' }] }),
      h({ count: 1, cost_usd: 0.5, credits: 3, agents: ['claude', 'copilot'], lanes: [{ id: 2, repo: 'a', name: 'y' }] }),
    ])
    expect(got.count).toBe(3)
    expect(got.cost_usd).toBe(2)
    expect(got.credits).toBe(3)
    expect(got.agents).toEqual(['claude', 'copilot'])
    expect(got.lanes.map((l) => l.id)).toEqual([2, 1])
  })
})
