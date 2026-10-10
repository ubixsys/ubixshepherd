import { Fragment, useEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from 'react'
import type { DecisionView, DeskEvent } from '../api/types'
import { EventGlyph, Glyph } from '../components/Glyph'
import { since } from '../format'
import { useNow } from '../hooks'
import { MARKS } from '../model/marks'
import { href } from '../router'
import { useLive, useSnapshot } from '../state/context'
import { DeskThread, SIGN_IN_HINT, timeline, type Connection, type DeskSnapshot } from '../state/desk'
import '../styles/chat.css'

// What a model or agent wrote is text, never markup: the page builds elements, so nothing
// of it reaches innerHTML, and a link survives only as an http or https anchor.

/** The URL as an anchor target when it is http or https; null for anything else. */
export function safeHref(raw: string): string | null {
  try {
    const u = new URL(raw)
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : null
  } catch {
    return null
  }
}

const INLINE = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\([^)\s]+\))/

function inline(text: string): ReactNode[] {
  return text.split(INLINE).map((part, i) => {
    if (part.length > 2 && part.startsWith('`') && part.endsWith('`')) return <code key={i}>{part.slice(1, -1)}</code>
    if (part.length > 4 && part.startsWith('**') && part.endsWith('**')) return <strong key={i}>{part.slice(2, -2)}</strong>
    const m = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(part)
    if (m) {
      const url = safeHref(m[2] ?? '')
      if (url) {
        return (
          <a key={i} href={url} target="_blank" rel="noopener noreferrer">
            {m[1]}
          </a>
        )
      }
      // Not a web link: show what it said, with its target as plain text.
      return <Fragment key={i}>{`${m[1] ?? ''} (${m[2] ?? ''})`}</Fragment>
    }
    return <Fragment key={i}>{part}</Fragment>
  })
}

type Block =
  | { t: 'p'; text: string }
  | { t: 'h'; text: string }
  | { t: 'code'; text: string }
  | { t: 'list'; ordered: boolean; items: string[] }

/** A small markdown subset: paragraphs, headings, fenced code, bullet and numbered lists. */
export function blocks(src: string): Block[] {
  const out: Block[] = []
  const lines = src.replace(/\r\n?/g, '\n').split('\n')
  let para: string[] = []
  const flush = () => {
    if (para.length) out.push({ t: 'p', text: para.join('\n') })
    para = []
  }
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? ''
    if (/^\s*```/.test(line)) {
      flush()
      const code: string[] = []
      for (i++; i < lines.length && !/^\s*```/.test(lines[i] ?? ''); i++) code.push(lines[i] ?? '')
      out.push({ t: 'code', text: code.join('\n') })
      continue
    }
    const h = /^#{1,6}\s+(.*)$/.exec(line)
    const li = /^\s*(?:([-*])|\d+[.)])\s+(.*)$/.exec(line)
    if (h) {
      flush()
      out.push({ t: 'h', text: h[1] ?? '' })
    } else if (li) {
      flush()
      const ordered = !li[1]
      const last = out.at(-1)
      if (last?.t === 'list' && last.ordered === ordered) last.items.push(li[2] ?? '')
      else out.push({ t: 'list', ordered, items: [li[2] ?? ''] })
    } else if (line.trim() === '') {
      flush()
    } else {
      para.push(line)
    }
  }
  flush()
  return out
}

export function Markdown({ text }: { text: string }) {
  return (
    <div className="md">
      {blocks(text).map((b, i) => {
        switch (b.t) {
          case 'code':
            return (
              <pre key={i}>
                <code>{b.text}</code>
              </pre>
            )
          case 'h':
            return (
              <p key={i} className="md-h">
                {inline(b.text)}
              </p>
            )
          case 'list': {
            const items = b.items.map((it, j) => <li key={j}>{inline(it)}</li>)
            return b.ordered ? <ol key={i}>{items}</ol> : <ul key={i}>{items}</ul>
          }
          default:
            return <p key={i}>{inline(b.text)}</p>
        }
      })}
    </div>
  )
}

const CONNECTION: Record<Connection, { text: string; cls: string; glyph: string }> = {
  connecting: { text: 'Connecting…', cls: '', glyph: '○' },
  live: { text: 'Live', cls: 'is-up', glyph: '●' },
  reconnecting: { text: 'Reconnecting…', cls: 'is-down', glyph: '○' },
  'signed-out': { text: 'Signed out', cls: 'is-down', glyph: '○' },
  refused: { text: 'Refused', cls: 'is-down', glyph: '○' },
}

function useThread(thread: DeskThread): DeskSnapshot {
  return useSyncExternalStore(thread.subscribe, thread.getSnapshot)
}

/** The chat page. `make` builds the thread for a workspace; tests give it fakes. */
export function ChatPage({ make = (id: number) => new DeskThread(id) }: { make?: (workspaceId: number) => DeskThread }) {
  const snap = useSnapshot()
  const workspaces = snap.status?.workspaces ?? []
  const [pick, setPick] = useState(0)
  // The daemon takes a workspace_id only when it has more than one.
  const workspaceId = workspaces.length > 1 ? (workspaces.find((w) => w.id === pick) ?? workspaces[0])?.id ?? 0 : 0
  if (!snap.status && !snap.error) return <p className="empty">Connecting to the daemon…</p>
  return (
    <ChatThread key={workspaceId} thread={make} workspaceId={workspaceId}>
      {workspaces.length > 1 && (
        <label className="chat-ws">
          <span className="faint">Workspace</span>
          <select value={workspaceId} onChange={(e) => setPick(Number(e.target.value))}>
            {workspaces.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
      )}
    </ChatThread>
  )
}

function ChatThread({
  thread: make, workspaceId, children,
}: {
  thread: (workspaceId: number) => DeskThread
  workspaceId: number
  children?: ReactNode
}) {
  const [thread] = useState(() => make(workspaceId))
  useEffect(() => {
    thread.start()
    return () => thread.stop()
  }, [thread])
  const d = useThread(thread)
  const live = useSnapshot()
  const now = useNow()
  const [confirming, setConfirming] = useState(false)
  const newButton = useRef<HTMLButtonElement>(null)
  const confirmButton = useRef<HTMLButtonElement>(null)
  const end = useRef<HTMLDivElement>(null)
  const pinned = useRef(true)

  const items = useMemo(() => timeline(d.events, live.feed), [d.events, live.feed])
  const open = useMemo(() => new Map(live.decisions.map((x) => [x.id, x])), [live.decisions])
  // Open decisions whose "asked" row is not in the loaded thread still wait on the person.
  const shown = new Set(items.flatMap((it) => (it.type === 'feed' && it.f.event === 'decision_asked' ? [it.f.ref ?? 0] : [])))
  const unplaced = live.decisions.filter((x) => !shown.has(x.id))

  useEffect(() => {
    const onScroll = () => {
      pinned.current = window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 120
    }
    window.addEventListener('scroll', onScroll, { passive: true })
    return () => window.removeEventListener('scroll', onScroll)
  }, [])
  useEffect(() => {
    if (pinned.current) end.current?.scrollIntoView({ block: 'end' })
  }, [items.length, d.partial, unplaced.length])
  useEffect(() => {
    if (confirming) confirmButton.current?.focus()
  }, [confirming])

  const conn = CONNECTION[d.connection]
  const stopped = d.connection === 'signed-out' || d.connection === 'refused'

  return (
    <div className="page chat">
      <header className="page-head chat-head">
        <h1>Chat</h1>
        <p className={`chat-conn live-dot ${conn.cls}`} role="status">
          <span aria-hidden="true">{conn.glyph}</span> {conn.text}
        </p>
        {children}
        <span className="chat-tools">
          {d.busy && (
            <span className="faint">
              {d.queued > 1 ? `Working, ${d.queued - 1} waiting` : 'Working'}…
            </span>
          )}
          {!confirming ? (
            <button ref={newButton} type="button" className="button" onClick={() => setConfirming(true)}>
              New conversation
            </button>
          ) : (
            <span className="chat-confirm" role="group" aria-label="Start a new conversation?">
              <span>Start fresh? The desk forgets this conversation&rsquo;s context.</span>
              <button
                ref={confirmButton}
                type="button"
                className="button button-primary"
                onClick={() => {
                  setConfirming(false)
                  void thread.newConversation()
                  newButton.current?.focus()
                }}
              >
                Start new
              </button>
              <button
                type="button"
                className="button"
                onClick={() => {
                  setConfirming(false)
                  window.setTimeout(() => newButton.current?.focus(), 0)
                }}
              >
                Cancel
              </button>
            </span>
          )}
        </span>
      </header>

      {stopped && !d.problem && (
        <p className="chat-problem tone-bad" role="alert">
          {d.loadError ?? `You are signed out. ${SIGN_IN_HINT}`}
        </p>
      )}
      {d.loadError && !stopped && (
        <p className="chat-problem tone-bad" role="alert">
          Could not read the conversation: {d.loadError}. Retrying.
        </p>
      )}

      {d.more && (
        <p className="chat-earlier">
          <button type="button" className="link-button" onClick={() => void thread.loadEarlier()}>
            Load earlier messages
          </button>
        </p>
      )}

      <div className="thread" role="log" aria-live="polite" aria-relevant="additions" aria-label="Conversation with the front desk" aria-busy={!d.loaded}>
        {d.loaded && items.length === 0 && d.partial === '' && unplaced.length === 0 && (
          <p className="faint thread-empty">Nothing said yet. Ask the front desk what the flock is doing.</p>
        )}
        {items.map((it) =>
          it.type === 'desk' ? (
            <DeskRow key={it.key} e={it.e} now={now} />
          ) : (
            <FeedRow key={it.key} item={it.f} now={now} decision={it.f.event === 'decision_asked' ? open.get(it.f.ref ?? 0) : undefined} />
          ),
        )}
        {unplaced.map((x) => (
          <div key={`d${x.id}`} className="trow trow-decision">
            <Glyph glyph={MARKS.decision_asked.glyph} tone="warn" label="decision waiting" />
            <InlineDecision d={x} />
          </div>
        ))}
        {d.partial !== '' && (
          <div className="msg msg-assistant is-partial">
            <span className="msg-who">Front desk</span>
            <Markdown text={d.partial} />
          </div>
        )}
        <div ref={end} />
      </div>

      <Composer thread={thread} d={d} disabled={stopped} />
    </div>
  )
}

function Composer({ thread, d, disabled }: { thread: DeskThread; d: DeskSnapshot; disabled: boolean }) {
  const [text, setText] = useState('')
  const box = useRef<HTMLTextAreaElement>(null)
  const sending = d.sending

  const send = async () => {
    if (sending || disabled || !text.trim()) return
    // The text stays until the daemon has taken it, so a refusal loses nothing.
    if (await thread.send(text)) setText('')
    box.current?.focus()
  }

  return (
    <form
      className="composer"
      onSubmit={(e) => {
        e.preventDefault()
        void send()
      }}
    >
      {d.problem && (
        <p className="chat-problem tone-bad" role="alert">
          {d.problem.message}
        </p>
      )}
      {d.notice && !d.problem && (
        <p className="chat-notice faint" role="status">
          {d.notice}
        </p>
      )}
      <label htmlFor="chat-input" className="visually-hidden">
        Message to the front desk
      </label>
      <textarea
        id="chat-input"
        ref={box}
        rows={2}
        value={text}
        readOnly={sending}
        disabled={disabled}
        placeholder="Message the front desk. Enter sends, Shift+Enter adds a line."
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault()
            void send()
          }
        }}
      />
      <div className="composer-bar">
        {d.busy && (
          <button type="button" className="button" onClick={() => void thread.interrupt()}>
            Interrupt
          </button>
        )}
        <button type="submit" className="button button-primary" disabled={sending || disabled || !text.trim()}>
          {sending ? 'Sending…' : 'Send'}
        </button>
      </div>
    </form>
  )
}

function DeskRow({ e, now }: { e: DeskEvent; now: number }) {
  const text = e.text ?? ''
  const age = since(e.created, now)
  const when = <time dateTime={e.created} title={e.created} className="faint trow-age">{age}</time>
  switch (e.kind) {
    case 'user':
      return (
        <div className="msg msg-user">
          <span className="msg-who">You {when}</span>
          <p className="msg-text">{text}</p>
        </div>
      )
    case 'assistant':
      return (
        <div className="msg msg-assistant">
          <span className="msg-who">Front desk {when}</span>
          <Markdown text={text} />
        </div>
      )
    case 'system':
      return (
        <details className="trow trow-system">
          <summary>
            <Glyph glyph={MARKS.info.glyph} tone="muted" label="note" /> Shepherd woke the front desk {when}
          </summary>
          <p className="msg-text">{text}</p>
        </details>
      )
    case 'tool':
      return (
        <p className="trow trow-tool faint">
          <Glyph glyph={MARKS.report.glyph} tone="muted" label="tool call" />
          <span className="mono">{text}</span>
        </p>
      )
    case 'error':
      return (
        <p className="trow tone-bad" role="alert">
          <Glyph glyph={MARKS.run_failed.glyph} tone="bad" label="error" />
          <span>{text}</span>
        </p>
      )
    case 'new':
      return (
        <p className="trow trow-new">
          <Glyph glyph={MARKS.lane_opened.glyph} tone="accent" label="new conversation" />
          <span>New conversation {when}</span>
        </p>
      )
    case 'turn_end':
      // A turn that ended well needs no row; one that did not says how.
      return text && text !== 'done' ? (
        <p className="trow faint">
          <Glyph glyph={MARKS.run_interrupted.glyph} tone="broken" label="turn ended early" />
          <span>Turn ended: {text}</span>
        </p>
      ) : null
    case 'cost':
      return text ? (
        <p className="trow faint">
          <Glyph glyph="$" tone="muted" label="cost" />
          <span>Turn cost ${text}</span>
        </p>
      ) : null
    case 'turn_start':
      return null
    default:
      // A kind a newer daemon adds still shows, as a note.
      return (
        <p className="trow faint">
          <Glyph glyph={MARKS.info.glyph} tone="muted" label={e.kind} />
          <span>{text || e.kind}</span>
        </p>
      )
  }
}

const RUN_KINDS = new Set(['run_ended', 'run_passed', 'run_failed', 'run_interrupted', 'run_quota'])

function FeedRow({ item, now, decision }: { item: { event: string; text: string; ref?: number; created: string }; now: number; decision?: DecisionView }) {
  const age = since(item.created, now)
  return (
    <div className={`trow${decision ? ' trow-decision' : ''}`}>
      <EventGlyph event={item.event} />
      <div className="trow-body">
        <p className="trow-line">
          <span>{item.text}</span>{' '}
          {RUN_KINDS.has(item.event) && item.ref ? <a href={href.log(item.ref)}>log</a> : null}{' '}
          <time dateTime={item.created} title={item.created} className="faint trow-age">{age}</time>
        </p>
        {decision && <InlineDecision d={decision} />}
      </div>
    </div>
  )
}

/**
 * A decision answered in place. The words go through Live.answer, the same call the
 * Decisions page makes, and only from the person's own click.
 */
function InlineDecision({ d }: { d: DecisionView }) {
  const live = useLive()
  const options = d.options ?? []
  const [choice, setChoice] = useState('')
  const [own, setOwn] = useState('')
  const [state, setState] = useState<{ s: 'idle' | 'sending' | 'done' } | { s: 'error'; msg: string }>({ s: 'idle' })
  const words = own.trim() || choice
  const id = `chat-decision-${d.id}`

  if (state.s === 'done') {
    return (
      <p className="decision-done" role="status">
        <Glyph glyph="↳" tone="ok" label="answered" /> Answered “{words}”.
      </p>
    )
  }
  const send = async () => {
    if (!words) return
    setState({ s: 'sending' })
    try {
      await live.answer(d.id, words)
      setState({ s: 'done' })
    } catch (e) {
      setState({ s: 'error', msg: e instanceof Error ? e.message : String(e) })
    }
  }
  return (
    <section className="decision decision-inline" aria-labelledby={`${id}-q`}>
      <p className="decision-meta">
        <span>decision {d.id}</span>
        <span>{d.lane}</span>
        <span className="faint">{d.repo}</span>
        <a href={href.decisions(d.id)}>open</a>
      </p>
      <p id={`${id}-q`} className="decision-q">{d.question}</p>
      {d.why && <p className="decision-why">{d.why}</p>}
      <fieldset className="decision-answer" disabled={state.s === 'sending'}>
        <legend className="visually-hidden">Your answer to decision {d.id}</legend>
        {options.length > 0 && (
          <ol className="options">
            {options.map((o) => (
              <li key={o}>
                <button
                  type="button"
                  className="option"
                  aria-pressed={choice === o && !own.trim()}
                  onClick={() => {
                    setChoice(o)
                    setOwn('')
                  }}
                >
                  <span className="option-text">{o}</span>
                  {d.recommendation && o.trim() === d.recommendation.trim() && <span className="option-rec">recommended</span>}
                </button>
              </li>
            ))}
          </ol>
        )}
        <label className="own">
          <span className="visually-hidden">Or write your own answer to decision {d.id}</span>
          <textarea
            rows={1}
            placeholder={options.length ? 'Or write your own answer' : 'Write your answer'}
            value={own}
            onChange={(e) => setOwn(e.target.value)}
          />
        </label>
        <div className="send">
          <button type="button" className="button button-primary" disabled={!words} onClick={() => void send()}>
            {state.s === 'sending' ? 'Sending…' : 'Answer'}
          </button>
          {state.s === 'error' && (
            <span className="tone-bad" role="alert">
              Not sent: {state.msg}
            </span>
          )}
        </div>
      </fieldset>
    </section>
  )
}
