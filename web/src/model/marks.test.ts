import { describe, it, expect } from 'vitest'

import { EVENTS, MARKS, markFor } from './marks'

describe('marks', () => {
  it('should have all events in MARKS', () => {
    for (const event of EVENTS) {
      expect(Object.hasOwn(MARKS, event)).toBe(true)
    }
  })

  it('should have distinct glyphs', () => {
    const glyphs = Object.values(MARKS).map(mark => mark.glyph)
    expect(new Set(glyphs).size).toBe(glyphs.length)
  })

  it('should return correct marks for known events', () => {
    expect(markFor('run_started')).toEqual({ glyph: '▸', tone: 'accent', label: 'run started' })
    expect(markFor('run_failed')).toEqual({ glyph: '✗', tone: 'bad', label: 'run failed' })
    expect(markFor('desk_rotated')).toEqual({ glyph: '↻', tone: 'muted', label: 'desk session rotated' })
    expect(markFor('info')).toEqual({ glyph: '·', tone: 'muted', label: 'note' })
  })

  it('should return info mark for unknown events', () => {
    expect(markFor('something_new')).toEqual({ glyph: '·', tone: 'muted', label: 'note' })
    expect(markFor('')).toEqual({ glyph: '·', tone: 'muted', label: 'note' })
  })
})