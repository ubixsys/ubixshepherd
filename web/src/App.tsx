import { useEffect, useMemo, useRef, useState } from 'react'
import { boardItems, counts, countsText, GROUP_NAMES, GROUPS } from './model/board'
import { usd } from './format'
import { useKeys } from './hooks'
import { go, href, useRoute } from './router'
import { useSnapshot } from './state/context'
import { BoardPage } from './pages/Board'
import { ChatPage } from './pages/Chat'
import { DecisionsPage } from './pages/Decisions'
import { LanePage } from './pages/Lane'
import { LogPage } from './pages/Log'
import { RunsPage } from './pages/Runs'
import { Help } from './components/Help'
import { GROUP_MARKS } from './model/groups'

export function App() {
  const route = useRoute()
  const snap = useSnapshot()
  const items = useMemo(() => boardItems(snap), [snap])
  const n = counts(items)
  const [help, setHelp] = useState(false)
  const pending = useRef('')

  // The counts go to the window title, so a background tab shows that something waits.
  useEffect(() => {
    const c = countsText(n)
    document.title = c ? `${c} · Shepherd` : 'Shepherd'
  }, [n])

  useKeys(
    {
      '?': () => setHelp((h) => !h),
      Escape: () => setHelp(false),
      g: () => {
        pending.current = 'g'
        window.setTimeout(() => (pending.current = ''), 1200)
      },
      b: () => pending.current === 'g' && go(href.board()),
      d: () => pending.current === 'g' && go(href.decisions()),
    },
    [],
  )

  const decisions = snap.decisions.length + snap.requests.length
  const spend = snap.spend

  return (
    <div className="app">
      <header className="bar">
        <a className="wordmark" href={href.board()}>
          <svg className="wordmark-mark" viewBox="0 0 32 32" aria-hidden="true" focusable="false">
            <rect width="32" height="32" fill="#0f62fe" />
            <rect x="6" y="6" width="9" height="9" fill="#ffffff" />
            <rect x="17" y="6" width="9" height="9" fill="#ffffff" opacity="0.55" />
            <rect x="6" y="17" width="9" height="9" fill="#ffffff" opacity="0.55" />
            <rect x="17" y="17" width="9" height="9" fill="#ffffff" />
          </svg>
          <span>
            uBix<b>Shepherd</b>
          </span>
        </a>
        <nav className="nav" aria-label="Views">
          <a href={href.board()} aria-current={route.page === 'board' ? 'page' : undefined}>
            Board
          </a>
          <a href={href.decisions()} aria-current={route.page === 'decisions' ? 'page' : undefined}>
            Decisions
            {decisions > 0 && <span className="nav-count tone-warn">{decisions}</span>}
          </a>
          <a href={href.runs()} aria-current={route.page === 'runs' ? 'page' : undefined}>
            Runs
          </a>
          <a href={href.chat()} aria-current={route.page === 'chat' ? 'page' : undefined}>
            Chat
          </a>
        </nav>
        <p className="bar-counts" aria-live="polite">
          {GROUPS.filter((g) => n[g] > 0).map((g) => (
            <span key={g} className={`bar-count tone-${g === 'needs' ? 'warn' : g === 'broken' ? 'broken' : 'muted'}`}>
              <span aria-hidden="true">{GROUP_MARKS[g].glyph}</span> {n[g]} {GROUP_NAMES[g]}
            </span>
          ))}
        </p>
        <p className="bar-meta">
          {spend && (
            <span title={`Spent today (${spend.day})`}>
              {usd(spend.usd)}
              {spend.budget > 0 && <span className="faint"> of {usd(spend.budget)}</span>} today
            </span>
          )}
          <span className={`live-dot ${snap.error ? 'is-down' : snap.ready ? 'is-up' : ''}`} title={snap.error ?? 'Live'}>
            <span aria-hidden="true">{snap.error ? '○' : '●'}</span>
            <span className="visually-hidden">{snap.error ? 'Not connected' : 'Connected'}</span>
          </span>
          <button type="button" className="help-button" onClick={() => setHelp(true)} aria-label="Keyboard shortcuts">
            <kbd>?</kbd>
          </button>
        </p>
      </header>

      {snap.error && (
        <div className="banner tone-bad" role="alert">
          The daemon did not answer: {snap.error}. Showing what was last loaded; retrying.
        </div>
      )}

      <main className="main">
        {route.page === 'board' && <BoardPage items={items} range={route.range} />}
        {route.page === 'runs' && <RunsPage range={route.range} agent={route.agent} lane={route.lane} />}
        {route.page === 'decisions' && <DecisionsPage focus={route.focus} />}
        {route.page === 'chat' && <ChatPage />}
        {route.page === 'lane' && <LanePage key={route.id} id={route.id} />}
        {route.page === 'log' && <LogPage key={route.run} run={route.run} />}
        {route.page === 'missing' && (
          <p className="empty">
            Nothing lives at <code>{route.path}</code>. <a href={href.board()}>Go to the board</a>.
          </p>
        )}
      </main>

      {help && <Help onClose={() => setHelp(false)} />}
    </div>
  )
}
