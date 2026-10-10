import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { clockTime } from '../format'
import { href } from '../router'
import '../styles/conversation.css'
import { Markdown } from './Markdown'
import { merge, sizeText, TOOL_STATUS, toolLine, took } from './model'
import type { ConversationSource, Item } from './types'
import { useFollowBottom } from './useFollowBottom'

/** How often a lane with a running run is read again. */
const POLL_MS = 2000

interface Props {
  source: ConversationSource
  /** Bumps when something about the lane changed (a feed item), to read now. */
  refreshKey?: number
  pollMs?: number
  now: number
}

/** A lane's whole dialogue as one thread, followed live while a run is going. */
export function Conversation({ source, refreshKey = 0, pollMs = POLL_MS, now }: Props) {
  const [items, setItems] = useState<Item[]>([])
  const [loaded, setLoaded] = useState(false)
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [thinking, setThinking] = useState(false)
  const [full, setFull] = useState<Map<number, Item>>(new Map())
  const cursor = useRef(0)
  const root = useRef<HTMLDivElement>(null)

  // Read what is new since the cursor, a page at a time; again after pollMs while a run
  // goes on, and whenever the lane's feed moves.
  useEffect(() => {
    let alive = true
    let timer: ReturnType<typeof setTimeout> | undefined
    const tick = async () => {
      let goOn = false
      try {
        let more = true
        while (more && alive) {
          const th = await source.read(cursor.current)
          if (!alive) return
          cursor.current = th.cursor
          setItems((have) => merge(have, th.items))
          // A changed item's expanded copy is stale.
          setFull((f) => (f.size === 0 ? f : new Map([...f].filter(([seq]) => !th.items.some((it) => it.seq === seq && it.kind === 'tool' && it.status !== f.get(seq)?.status)))))
          setRunning(th.running)
          goOn = th.running
          more = th.more
        }
        setError(null)
      } catch (e) {
        if (!alive) return
        setError(e instanceof Error ? e.message : String(e))
        goOn = true // try again: the daemon may be restarting
      }
      if (!alive) return
      setLoaded(true)
      if (goOn) timer = setTimeout(() => void tick(), pollMs)
    }
    void tick()
    return () => {
      alive = false
      if (timer) clearTimeout(timer)
    }
  }, [source, refreshKey, pollMs])

  const expand = useCallback(
    async (seq: number) => {
      const it = await source.expand(seq)
      setFull((f) => new Map(f).set(seq, it))
    },
    [source],
  )

  const shown = useMemo(() => items.filter((it) => thinking || it.kind !== 'thinking'), [items, thinking])
  const hidden = items.length - shown.length
  const follow = useFollowBottom(root, [items.length, thinking])

  return (
    <section ref={root} className="cv" aria-label="Conversation">
      <div className="cv-bar">
        <button type="button" className="button" aria-pressed={thinking} onClick={() => setThinking((v) => !v)}>
          {thinking ? 'Hide thinking' : 'Show thinking'}
          {!thinking && hidden > 0 ? ` (${hidden})` : ''}
        </button>
        {running && (
          <span className="faint" role="status">
            Following a running agent…
          </span>
        )}
      </div>
      {error && <p className="tone-bad">Could not read the conversation: {error}</p>}
      {!loaded && <p className="faint">Reading the conversation…</p>}
      {loaded && items.length === 0 && !error && <p className="faint">No agent has run in this lane yet.</p>}
      <ol className="cv-thread">
        {shown.map((it) => (
          <li key={it.seq} className={`cv-${it.kind}`}>
            <Row it={full.get(it.seq) ?? it} expand={expand} now={now} />
          </li>
        ))}
      </ol>
      {!follow.following && running && (
        <button type="button" className="button cv-jump" onClick={follow.jump}>
          Jump to latest
        </button>
      )}
    </section>
  )
}

function Row({ it, expand, now }: { it: Item; expand: (seq: number) => Promise<void>; now: number }) {
  switch (it.kind) {
    case 'run':
      return <RunDivider it={it} now={now} />
    case 'user':
      return (
        <div className="cv-msg cv-user-msg">
          <span className="cv-who">Brief</span>
          <Clipped it={it} expand={expand}>
            <p className="cv-plain">{it.text}</p>
          </Clipped>
        </div>
      )
    case 'agent':
      return (
        <div className="cv-msg cv-agent-msg">
          <span className="cv-who">{it.time ? `Agent · ${clockTime(it.time, now)}` : 'Agent'}</span>
          <Clipped it={it} expand={expand}>
            <Markdown text={it.text ?? ''} />
          </Clipped>
        </div>
      )
    case 'thinking':
      return (
        <div className="cv-msg cv-thinking-msg">
          <span className="cv-who">Thinking</span>
          <p className="cv-plain faint">{it.text || '(the agent did not record its reasoning)'}</p>
        </div>
      )
    case 'tool':
      return <Tool it={it} expand={expand} />
  }
}

function RunDivider({ it, now }: { it: Item; now: number }) {
  const took_ = took(it.time, it.end)
  return (
    <div className="cv-divider" role="separator" aria-label={`Run ${it.run}`}>
      <span className="cv-divider-line">
        <a href={href.log(it.run)}>Run {it.run}</a> · {it.agent} · {it.state}
        {took_ && <> · {took_}</>}
        {it.time && <span className="faint"> · started {clockTime(it.time, now)}</span>}
      </span>
      {it.note && (
        <span className="cv-note faint">
          {it.note} <a href={href.log(it.run)}>Open the run&rsquo;s log</a>
        </span>
      )}
    </div>
  )
}

function Tool({ it, expand }: { it: Item; expand: (seq: number) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const status = it.status ?? 'ok'
  return (
    <div className={`cv-tool is-${status}`}>
      <button type="button" className="cv-tool-head" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        <span aria-hidden="true">{open ? '▾' : '▸'}</span>
        <span className="cv-tool-line">{toolLine(it)}</span>
        {status !== 'ok' && <span className="cv-tool-status">{TOOL_STATUS[status]}</span>}
      </button>
      {open && (
        <div className="cv-tool-body">
          {it.input && (
            <>
              <span className="cv-who">Input</span>
              <pre>{it.input}</pre>
            </>
          )}
          {(it.output || status === 'ok' || status === 'error') && (
            <>
              <span className="cv-who">Output</span>
              <Clipped it={it} expand={expand}>
                <pre>{it.output || '(no output)'}</pre>
              </Clipped>
            </>
          )}
        </div>
      )}
    </div>
  )
}

/** A body that may have been clipped by the daemon, with a control to read all of it. */
function Clipped({ it, expand, children }: { it: Item; expand: (seq: number) => Promise<void>; children: React.ReactNode }) {
  const [busy, setBusy] = useState(false)
  const [failed, setFailed] = useState<string | null>(null)
  return (
    <>
      {children}
      {it.truncated && (
        <p className="cv-clip faint">
          Clipped{it.bytes ? ` (${sizeText(it.bytes)} in all)` : ''}.{' '}
          <button
            type="button"
            className="link-button"
            disabled={busy}
            onClick={() => {
              setBusy(true)
              expand(it.seq).catch((e: unknown) => setFailed(e instanceof Error ? e.message : String(e))).finally(() => setBusy(false))
            }}
          >
            {busy ? 'Loading…' : 'Show all'}
          </button>
          {failed && <span className="tone-bad"> {failed}</span>}
        </p>
      )}
    </>
  )
}
