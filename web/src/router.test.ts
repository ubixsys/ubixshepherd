import { href, parse } from './router'

describe('router', () => {
  it('parses every page and round-trips its href', () => {
    expect(parse('')).toEqual({ page: 'board' })
    expect(parse(href.board())).toEqual({ page: 'board' })
    expect(parse(href.decisions())).toEqual({ page: 'decisions' })
    expect(parse(href.decisions(9))).toEqual({ page: 'decisions', focus: 9 })
    expect(parse(href.chat())).toEqual({ page: 'chat' })
    expect(parse('#/chat?x=1')).toEqual({ page: 'chat' })
    expect(parse(href.lane(4))).toEqual({ page: 'lane', id: 4 })
    expect(parse(href.log(12))).toEqual({ page: 'log', run: 12 })
    expect(parse(href.board('week'))).toEqual({ page: 'board', range: 'week' })
    expect(parse('#/?range=bogus')).toEqual({ page: 'board' })
    expect(parse(href.runs())).toEqual({ page: 'runs' })
    const f = { range: 'week', agent: 'claude', lane: 7 } as const
    expect(parse(href.runs(f))).toEqual({ page: 'runs', ...f })
    expect(parse('#/nope')).toEqual({ page: 'missing', path: '/nope' })
  })
})
