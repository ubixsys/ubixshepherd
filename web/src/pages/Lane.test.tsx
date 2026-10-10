import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Conversation } from '../conversation/Conversation'
import type { ConversationSource, Item, Thread } from '../conversation/types'
import { LiveProvider } from '../state/context'
import { Live, type Api, type Clock } from '../state/live'
import { decision, feedItem, lane, run, spend, status } from '../test/fixtures'
import { LanePage } from './Lane'

const still: Clock = { now: () => Date.parse('2026-10-08T10:05:00Z'), hidden: () => false, setTimeout: () => 0, clearTimeout: () => {} }
const NOW = Date.parse('2026-10-08T10:05:00Z')

function item(o: Partial<Item> & Pick<Item, 'seq' | 'kind'>): Item {
  return { run: 10, time: '2026-10-08T10:01:00Z', ...o }
}

const FULL_OUTPUT = 'line\n'.repeat(5000)

const dialogue: Item[] = [
  item({ seq: 1, kind: 'run', agent: 'claude', state: 'succeeded', time: '2026-10-08T10:00:00Z', end: '2026-10-08T10:04:00Z' }),
  item({ seq: 2, kind: 'user', text: 'Fix the login redirect.' }),
  item({ seq: 3, kind: 'thinking', text: 'The redirect probably drops the query string.' }),
  item({ seq: 4, kind: 'agent', text: 'I will **read** the handler, see `auth.go`.\n\n- first\n- second' }),
  item({ seq: 5, kind: 'tool', tool: 'Bash', summary: 'go test ./...', input: '{\n  "command": "go test ./..."\n}', output: 'ok  auth 0.2s', status: 'ok' }),
  item({ seq: 6, kind: 'tool', tool: 'Read', summary: '/x/big.log', input: '{}', output: 'line\n'.repeat(10), status: 'ok', truncated: true, bytes: FULL_OUTPUT.length }),
  item({ seq: 7, kind: 'run', run: 11, agent: 'copilot', state: 'failed', time: '2026-10-08T10:06:00Z', end: '2026-10-08T10:07:00Z', note: "no transcript for copilot: Shepherd does not read Copilot CLI's session store; see this run's log" }),
]

function source(items: Item[] = dialogue, over: Partial<ConversationSource> = {}): ConversationSource {
  return {
    read: async (after) => ({ items: items.filter((it) => it.kind === 'run' || it.seq > after), cursor: items.at(-1)?.seq ?? 0, more: false, running: false }),
    expand: async (seq) => ({ ...(items.find((it) => it.seq === seq) as Item), output: FULL_OUTPUT, truncated: false }),
    ...over,
  }
}

function liveWith(lanes = [lane()]) {
  const api: Api = {
    status: async () => status(),
    lanes: async () => lanes,
    runs: async () => [run()],
    run: async () => { throw new Error('unused') },
    runLog: async () => { throw new Error('unused') },
    runEvents: async () => { throw new Error('unused') },
    decisions: async () => [decision()],
    requests: async () => [],
    spend: async () => spend(),
    feedLatest: async () => ({ items: [], events: [], last: 1 }),
    feed: async (after) => ({ items: after === 0 ? [feedItem({ id: 1 })] : [], events: ['run_passed'], last: 1 }),
    answer: async () => { throw new Error('unused') },
  }
  return new Live(api, still)
}

async function showLane(src: ConversationSource, lanes = [lane()]) {
  const live = liveWith(lanes)
  await act(async () => {
    await live.refresh()
  })
  // The page reads the daemon for its runs and decisions through fetch.
  vi.stubGlobal('fetch', async (path: string) => {
    const body = path.startsWith('/v1/runs') ? [run()] : []
    return new Response(JSON.stringify(body), { status: 200 })
  })
  render(
    <LiveProvider live={live}>
      <LanePage id={1} conversation={() => src} />
    </LiveProvider>,
  )
  await userEvent.click(await screen.findByRole('tab', { name: 'Conversation' }))
}

afterEach(() => vi.unstubAllGlobals())

describe('the Conversation tab', () => {
  it('shows the lane page first, and the conversation on its tab', async () => {
    await showLane(source())
    expect(screen.getByRole('tab', { name: 'Conversation' })).toHaveAttribute('aria-selected', 'true')
    const thread = await screen.findByRole('region', { name: 'Conversation' })
    expect(await within(thread).findByText('Fix the login redirect.')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'Overview' }))
    expect(screen.queryByText('Fix the login redirect.')).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Runs' })).toBeVisible()
  })

  it('draws one thread with a divider per run, and markdown for agent text', async () => {
    await showLane(source())
    await screen.findByText('Fix the login redirect.')
    const dividers = screen.getAllByRole('separator')
    expect(dividers).toHaveLength(2)
    expect(dividers[0]).toHaveTextContent('Run 10 · claude · succeeded · 4m 0s')
    expect(dividers[1]).toHaveTextContent('Run 11 · copilot · failed')
    const strong = screen.getByText('read')
    expect(strong.tagName).toBe('STRONG')
    expect(screen.getByText('auth.go').tagName).toBe('CODE')
    expect(screen.getAllByRole('listitem').some((li) => li.textContent === 'second')).toBe(true)
  })

  it('falls back to the run log for an agent with no transcript', async () => {
    await showLane(source())
    const divider = (await screen.findAllByRole('separator'))[1] as HTMLElement
    expect(divider).toHaveTextContent('no transcript for copilot')
    expect(within(divider).getByRole('link', { name: /log/i })).toHaveAttribute('href', '#/runs/11/log')
  })

  it('collapses tool calls to a line and expands them', async () => {
    await showLane(source())
    const head = await screen.findByRole('button', { name: /Bash\s+go test/ })
    expect(head).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText(/ok auth 0\.2s/)).not.toBeInTheDocument()
    await userEvent.click(head)
    expect(head).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText(/ok auth 0\.2s/)).toBeInTheDocument()
    expect(screen.getByText(/"command": "go test/)).toBeInTheDocument()
    await userEvent.click(head)
    expect(screen.queryByText(/ok auth 0\.2s/)).not.toBeInTheDocument()
  })

  it('shows a clipped output collapsed and loads all of it on request', async () => {
    const expand = vi.fn(source().expand)
    await showLane(source(dialogue, { expand }))
    await userEvent.click(await screen.findByRole('button', { name: /Read\s+\/x\/big.log/ }))
    expect(screen.getByText(/Clipped \(/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Show all' }))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Show all' })).not.toBeInTheDocument())
    expect(expand).toHaveBeenCalledWith(6)
    expect(document.querySelector('.cv-tool-body pre:last-of-type')?.textContent?.length).toBe(FULL_OUTPUT.length)
  })

  it('hides thinking until asked', async () => {
    await showLane(source())
    await screen.findByText('Fix the login redirect.')
    expect(screen.queryByText(/drops the query string/)).not.toBeInTheDocument()
    const toggle = screen.getByRole('button', { name: /Show thinking \(1\)/ })
    await userEvent.click(toggle)
    expect(screen.getByText(/drops the query string/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Hide thinking' }))
    expect(screen.queryByText(/drops the query string/)).not.toBeInTheDocument()
  })

  it('works on a closed lane the daemon no longer lists, from its runs', async () => {
    await showLane(source(), [])
    expect(await screen.findByText('Fix the login redirect.')).toBeInTheDocument()
  })
})

describe('following a running lane', () => {
  let height = 2000
  let y = 1300
  const scrollTo = vi.fn()
  const saved = { scrollTo: window.scrollTo, scrollY: Object.getOwnPropertyDescriptor(window, 'scrollY'), innerHeight: Object.getOwnPropertyDescriptor(window, 'innerHeight') }

  beforeEach(() => {
    height = 2000
    y = 1300 // 1300 + 700 is the bottom
    scrollTo.mockClear()
    window.scrollTo = scrollTo as unknown as typeof window.scrollTo
    Object.defineProperty(document.documentElement, 'scrollHeight', { configurable: true, get: () => height })
    Object.defineProperty(window, 'scrollY', { configurable: true, get: () => y })
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 700 })
  })
  afterEach(() => {
    window.scrollTo = saved.scrollTo
    delete (document.documentElement as { scrollHeight?: number }).scrollHeight
    if (saved.scrollY) Object.defineProperty(window, 'scrollY', saved.scrollY)
    else delete (window as { scrollY?: number }).scrollY
    if (saved.innerHeight) Object.defineProperty(window, 'innerHeight', saved.innerHeight)
  })

  /** A source whose thread grows one agent message per read, asked after what was seen. */
  function growing() {
    const calls: number[] = []
    let n = 1
    const src: ConversationSource = {
      read: async (after): Promise<Thread> => {
        calls.push(after)
        const items: Item[] = [item({ seq: 1, kind: 'run', agent: 'claude', state: 'running' })]
        for (let i = 2; i <= n + 1; i++) if (i > after) items.push(item({ seq: i, kind: 'agent', text: `message ${i}` }))
        const th = { items, cursor: n + 1, more: false, running: true }
        n++
        return th
      },
      expand: async () => { throw new Error('unused') },
    }
    return { src, calls }
  }

  it('reads only what is new since the cursor, and shows it as it comes', async () => {
    const { src, calls } = growing()
    render(<Conversation source={src} pollMs={5} now={NOW} />)
    expect(await screen.findByText('message 3')).toBeInTheDocument()
    await waitFor(() => expect(calls.length).toBeGreaterThan(2))
    expect(calls.slice(0, 3)).toEqual([0, 2, 3])
    // Earlier messages are still there: items merge, they do not replace the thread.
    expect(screen.getByText('message 2')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('Following a running agent')
  })

  it('stays at the bottom while the reader is there', async () => {
    const { src } = growing()
    render(<Conversation source={src} pollMs={5} now={NOW} />)
    await screen.findByText('message 2')
    await waitFor(() => expect(scrollTo).toHaveBeenCalledWith({ top: 2000 }))
    scrollTo.mockClear()
    await screen.findByText('message 4')
    expect(scrollTo).toHaveBeenCalled()
  })

  it('leaves the reader alone once they scroll up, and offers a way back', async () => {
    const { src } = growing()
    render(<Conversation source={src} pollMs={5} now={NOW} />)
    await screen.findByText('message 2')
    y = 300
    act(() => {
      window.dispatchEvent(new Event('scroll'))
    })
    scrollTo.mockClear()
    await screen.findByText('message 5')
    expect(scrollTo).not.toHaveBeenCalled()
    const jump = screen.getByRole('button', { name: 'Jump to latest' })
    await userEvent.click(jump)
    expect(scrollTo).toHaveBeenCalledWith({ top: 2000 })
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).not.toBeInTheDocument()
  })

  it('stops reading when the lane is not running', async () => {
    const read = vi.fn(async (): Promise<Thread> => ({ items: dialogue.slice(0, 2), cursor: 2, more: false, running: false }))
    render(<Conversation source={{ read, expand: async () => dialogue[0] as Item }} pollMs={5} now={NOW} />)
    await screen.findByText('Fix the login redirect.')
    await new Promise((r) => setTimeout(r, 40))
    expect(read).toHaveBeenCalledTimes(1)
  })
})
