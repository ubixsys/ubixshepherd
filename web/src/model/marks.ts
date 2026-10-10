// How each feed event is drawn: a glyph that carries the meaning alone, so the timeline
// reads the same without colour, and a tone. The same table as the terminal's
// (internal/chat/events.go); the set of events is internal/api/events.go's.

/** Every event the daemon sends, in the order events.go lists them. */
export const EVENTS = [
  'lane_opened',
  'lane_closed',
  'run_started',
  'run_ended',
  'run_passed',
  'run_failed',
  'run_interrupted',
  'run_quota',
  'commit',
  'gate',
  'report',
  'decision_asked',
  'decision_answered',
  'request',
  'request_attention',
  'mr',
  'pipeline',
  'budget',
  'tag',
  'release',
  'config',
  'desk_rotated',
  'info'
] as const

export type EventKind = (typeof EVENTS)[number]

export type Tone = 'muted' | 'accent' | 'warn' | 'ok' | 'bad' | 'broken'

export interface Mark {
  glyph: string
  tone: Tone
  /** What the glyph means, for screen readers and the legend: "run passed". */
  label: string
}

export const MARKS: Record<EventKind, Mark> = {
  lane_opened: { glyph: '+', tone: 'muted', label: 'lane opened' },
  lane_closed: { glyph: '−', tone: 'muted', label: 'lane closed' },
  run_started: { glyph: '▸', tone: 'accent', label: 'run started' },
  run_ended: { glyph: '■', tone: 'accent', label: 'run ended' },
  run_passed: { glyph: '✓', tone: 'ok', label: 'run passed' },
  run_failed: { glyph: '✗', tone: 'bad', label: 'run failed' },
  run_interrupted: { glyph: '↯', tone: 'broken', label: 'run interrupted' },
  run_quota: { glyph: '∅', tone: 'broken', label: 'out of quota' },
  commit: { glyph: '*', tone: 'muted', label: 'commit' },
  gate: { glyph: '◇', tone: 'accent', label: 'gate' },
  report: { glyph: '»', tone: 'muted', label: 'report' },
  decision_asked: { glyph: '?', tone: 'warn', label: 'decision asked' },
  decision_answered: { glyph: '↳', tone: 'muted', label: 'decision answered' },
  request: { glyph: '⇄', tone: 'muted', label: 'request' },
  request_attention: { glyph: '!', tone: 'warn', label: 'request needs you' },
  mr: { glyph: '◆', tone: 'accent', label: 'merge request' },
  pipeline: { glyph: '◎', tone: 'accent', label: 'pipeline' },
  budget: { glyph: '$', tone: 'warn', label: 'budget' },
  tag: { glyph: '#', tone: 'muted', label: 'tag' },
  release: { glyph: '▲', tone: 'muted', label: 'release' },
  config: { glyph: '~', tone: 'muted', label: 'config' },
  desk_rotated: { glyph: '↻', tone: 'muted', label: 'desk session rotated' },
  info: { glyph: '·', tone: 'muted', label: 'note' }
}

/** The mark for any event string; one this table does not know reads as info. */
export function markFor(event: string): Mark {
  if ((EVENTS as readonly string[]).includes(event)) {
    return MARKS[event as keyof typeof MARKS]
  }
  return MARKS.info
}