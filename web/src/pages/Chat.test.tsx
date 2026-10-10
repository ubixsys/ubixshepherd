import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ApiError } from '../api/client'
import type { Decision } from '../api/types'
import { Live, type Api, type Clock } from '../state/live'
import { LiveProvider } from '../state/context'
import { DeskThread } from '../state/desk'
import { decision, deskEvent, FakeSource, fakeDeskApi, feedItem, request, spend, status } from '../test/fixtures'
import type { DeskApi } from '../api/desk'
import { ChatPage, Markdown } from './Chat'

const still: Clock = { now: () => Date.parse('2026-10-08T10:05:00Z'), hidden: () => false, setTimeout: () => 0, clearTimeout: () => {} }

function liveWith(feed = [feedItem({ id: 1, kind: 'decision', ref: 5, text: 'decision 5 asked', created: '2026-10-08T10:01:00Z' })], answer?: Api['answer']) {
  const api: Api = {
    status: async () => status(),
    lanes: async () => [],
    runs: async () => [],
    run: async () => { throw new Error('unused') },
    runLog: async () => { throw new Error('unused') },
    runEvents: async () => { throw new Error('unused') },
    decisions: async (state) => (state === 'open' ? [decision()] : []),
    requests: async () => [request()],
    spend: async () => spend(),
    feedLatest: async () => ({ items: [], events: [], last: feed.length }),
    feed: async (after) => ({ items: after === 0 ? feed : [], events: feed.map((f) => f.kind === 'decision' ? 'decision_asked' : 'run_passed'), last: feed.length }),
    answer: answer ?? vi.fn(async (id: number, words: string): Promise<Decision> => ({ ...decision({ id }), state: 'answered', answer: words })),
  }
  return new Live(api, still)
}

async function show(api: DeskApi, live = liveWith()) {
  FakeSource.all = []
  await act(async () => {
    await live.refresh()
  })
  await act(async () => {
    live.start()
  })
  render(
    <LiveProvider live={live}>
      <ChatPage make={(id) => new DeskThread(id, { api, open: (u) => new FakeSource(u) })} />
    </LiveProvider>,
  )
  await waitFor(() => expect(FakeSource.all.length).toBeGreaterThan(0))
  const src = FakeSource.get(0)
  act(() => src.open())
  return { src, live }
}

const history = [
  deskEvent({ seq: 1, kind: 'user', text: 'what is up?', created: '2026-10-08T10:00:00Z' }),
  deskEvent({ seq: 2, kind: 'assistant', text: 'Two lanes **need** you.', created: '2026-10-08T10:00:30Z' }),
]

describe('ChatPage', () => {
  it('loads the thread from history and shows the connection', async () => {
    await show(fakeDeskApi({ history: async () => ({ events: history, more: false }) }))
    expect(await screen.findByText('what is up?')).toBeInTheDocument()
    expect(screen.getByText('need').tagName).toBe('STRONG')
    expect(screen.getByRole('status', { name: '' })).toBeTruthy()
    expect(screen.getByText('Live')).toBeInTheDocument()
    expect(screen.getByRole('log')).toHaveAttribute('aria-live', 'polite')
  })

  it('draws a partial reply as one growing message and a final one replaces it', async () => {
    const { src } = await show(fakeDeskApi())
    act(() => src.emit('partial', { kind: 'partial', text: 'Looking' }))
    act(() => src.emit('partial', { kind: 'partial', text: ' at the board' }))
    expect(screen.getByText('Looking at the board')).toBeInTheDocument()
    act(() => src.emit('assistant', deskEvent({ seq: 3, kind: 'assistant', text: 'All quiet.', created: '2026-10-08T10:04:00Z' })))
    expect(screen.queryByText('Looking at the board')).not.toBeInTheDocument()
    expect(screen.getByText('All quiet.')).toBeInTheDocument()
  })

  it('reopens a dropped stream after the last event it saw', async () => {
    const { src } = await show(fakeDeskApi({ history: async () => ({ events: history, more: false }) }))
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      act(() => src.emit('assistant', deskEvent({ seq: 8, kind: 'assistant', text: 'one more' })))
      act(() => src.fail())
      expect(await screen.findByText('Reconnecting…')).toBeInTheDocument()
      await act(async () => {
        await vi.advanceTimersByTimeAsync(600)
      })
      expect(FakeSource.all[1]?.url).toBe('/stream?ws=0&after=8')
      act(() => FakeSource.get(1).open())
      expect(await screen.findByText('Live')).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('Enter sends, Shift+Enter adds a line, and the box empties on success', async () => {
    const user = userEvent.setup()
    const turn = vi.fn(async () => ({ workspace_id: 1, turn: 9, ahead: 1 }))
    await show(fakeDeskApi({ turn }))
    const box = screen.getByLabelText('Message to the front desk')
    await user.type(box, 'line one{Shift>}{Enter}{/Shift}line two')
    expect(box).toHaveValue('line one\nline two')
    await user.keyboard('{Enter}')
    expect(turn).toHaveBeenCalledWith(0, 'line one\nline two')
    await waitFor(() => expect(box).toHaveValue(''))
    expect(await screen.findByRole('button', { name: 'Interrupt' })).toBeInTheDocument()
  })

  it('disables the composer while a send is pending', async () => {
    const user = userEvent.setup()
    let release: () => void = () => {}
    const turn = vi.fn(() => new Promise<{ workspace_id: number; turn: number; ahead: number }>((r) => {
      release = () => r({ workspace_id: 1, turn: 2, ahead: 1 })
    }))
    await show(fakeDeskApi({ turn }))
    await user.type(screen.getByLabelText('Message to the front desk'), 'hi{Enter}')
    expect(await screen.findByRole('button', { name: 'Sending…' })).toBeDisabled()
    await user.keyboard('{Enter}')
    expect(turn).toHaveBeenCalledTimes(1)
    await act(async () => release())
    expect(await screen.findByRole('button', { name: 'Send' })).toBeDisabled()
  })

  it('says the queue is full and keeps the words', async () => {
    const user = userEvent.setup()
    const turn = vi.fn().mockRejectedValue(new ApiError(429, 'the desk has 8 turns waiting'))
    await show(fakeDeskApi({ turn }))
    const box = screen.getByLabelText('Message to the front desk')
    await user.type(box, 'hello{Enter}')
    expect(await screen.findByRole('alert')).toHaveTextContent(/as many messages waiting/)
    expect(box).toHaveValue('hello')
  })

  it('tells a signed-out browser how to sign in', async () => {
    const user = userEvent.setup()
    const turn = vi.fn().mockRejectedValue(new ApiError(401, 'missing, wrong or revoked token'))
    await show(fakeDeskApi({ turn }))
    await user.type(screen.getByLabelText('Message to the front desk'), 'hello{Enter}')
    await waitFor(() => expect(screen.getAllByRole('alert')[0]).toHaveTextContent('shepherd web'))
    expect(screen.getByText('Signed out')).toBeInTheDocument()
  })

  it('shows a 403 with the daemon words', async () => {
    const user = userEvent.setup()
    const turn = vi.fn().mockRejectedValue(new ApiError(403, 'cross-site request refused'))
    await show(fakeDeskApi({ turn }))
    await user.type(screen.getByLabelText('Message to the front desk'), 'hello{Enter}')
    expect(await screen.findByText(/cross-site request refused/)).toBeInTheDocument()
  })

  it('interrupts a running turn', async () => {
    const user = userEvent.setup()
    const interrupt = vi.fn(async () => ({ interrupted: true }))
    const { src } = await show(fakeDeskApi({ interrupt }))
    expect(screen.queryByRole('button', { name: 'Interrupt' })).not.toBeInTheDocument()
    act(() => src.emit('turn_start', deskEvent({ seq: 3, kind: 'turn_start', text: '' })))
    await user.click(await screen.findByRole('button', { name: 'Interrupt' }))
    expect(interrupt).toHaveBeenCalledWith(0)
    expect(await screen.findByText('Interrupted.')).toBeInTheDocument()
  })

  it('asks before starting a new conversation, and cancel does nothing', async () => {
    const user = userEvent.setup()
    const newConversation = vi.fn(async () => ({}))
    await show(fakeDeskApi({ newConversation }))
    await user.click(screen.getByRole('button', { name: 'New conversation' }))
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(newConversation).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'New conversation' }))
    await user.click(screen.getByRole('button', { name: 'Start new' }))
    expect(newConversation).toHaveBeenCalledWith(0)
    expect(screen.queryByRole('button', { name: 'Start new' })).not.toBeInTheDocument()
  })

  it('draws a new-conversation row and a run row between messages', async () => {
    const { src } = await show(fakeDeskApi({ history: async () => ({ events: history, more: false }) }))
    act(() => src.emit('new', deskEvent({ seq: 9, kind: 'new', text: '', created: '2026-10-08T10:04:00Z' })))
    expect(await screen.findByText('New conversation', { selector: 'span' })).toBeInTheDocument()
  })

  it('answers a decision in place, with the same call as the Decisions page', async () => {
    const user = userEvent.setup()
    const answer = vi.fn(async (id: number, words: string): Promise<Decision> => ({ ...decision({ id }), state: 'answered', answer: words }))
    await show(fakeDeskApi({ history: async () => ({ events: history, more: false }) }), liveWith(undefined, answer))
    const card = (await screen.findByText(decision().question)).closest('section') as HTMLElement
    await user.click(within(card).getByRole('button', { name: /^No/ }))
    expect(answer).not.toHaveBeenCalled()
    await user.click(within(card).getByRole('button', { name: 'Answer' }))
    expect(answer).toHaveBeenCalledWith(decision().id, 'No')
    expect(await screen.findByText(/^Answered “No”/)).toBeInTheDocument()
  })

  it('renders hostile text inert', async () => {
    const evil = [
      '<script>window.pwned = 1</script><img src=x onerror="window.pwned=1">',
      '[click](javascript:alert(1)) and [ok](https://example.com/a?b=c) and [data](data:text/html,x)',
    ].join('\n\n')
    await show(fakeDeskApi({ history: async () => ({ events: [deskEvent({ seq: 1, kind: 'assistant', text: evil }), deskEvent({ seq: 2, kind: 'user', text: '<b onclick="x()">hi</b>' })], more: false }) }))
    expect(await screen.findByText(/<script>window.pwned = 1<\/script>/)).toBeInTheDocument()
    expect(document.querySelector('script')).toBeNull()
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('[onclick], [onerror]')).toBeNull()
    expect(screen.getByText('<b onclick="x()">hi</b>')).toBeInTheDocument()
    const links = Array.from(document.querySelectorAll('.md a'))
    expect(links.map((a) => a.getAttribute('href'))).toEqual(['https://example.com/a?b=c'])
    expect(links[0]).toHaveAttribute('rel', 'noopener noreferrer')
    expect(document.body.innerHTML).not.toMatch(/href="javascript:/i)
  })
})

describe('following the bottom', () => {
  let height = 2000
  let y = 1300
  const scrollTo = vi.fn()
  const observers: (() => void)[] = []
  const saved = { scrollTo: window.scrollTo, scrollY: Object.getOwnPropertyDescriptor(window, 'scrollY'), innerHeight: Object.getOwnPropertyDescriptor(window, 'innerHeight') }

  beforeEach(() => {
    height = 2000
    y = 1300 // 1300 + 700 is the bottom
    scrollTo.mockClear()
    observers.length = 0
    window.scrollTo = scrollTo as unknown as typeof window.scrollTo
    Object.defineProperty(document.documentElement, 'scrollHeight', { configurable: true, get: () => height })
    Object.defineProperty(window, 'scrollY', { configurable: true, get: () => y })
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 700 })
    vi.stubGlobal('ResizeObserver', class {
      constructor(cb: () => void) {
        observers.push(cb)
      }
      observe() {}
      disconnect() {}
    })
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    window.scrollTo = saved.scrollTo
    delete (document.documentElement as { scrollHeight?: number }).scrollHeight
    if (saved.scrollY) Object.defineProperty(window, 'scrollY', saved.scrollY)
    else delete (window as { scrollY?: number }).scrollY
    if (saved.innerHeight) Object.defineProperty(window, 'innerHeight', saved.innerHeight)
  })

  const grow = (to: number) => {
    height = to
    act(() => observers.forEach((cb) => cb()))
  }
  const scrollTop = (to: number) => {
    y = to
    act(() => {
      window.dispatchEvent(new Event('scroll'))
    })
  }
  const last = () => scrollTo.mock.calls.at(-1)?.[0] as { top: number } | undefined

  it('lands at the bottom when the thread first loads', async () => {
    await show(fakeDeskApi({ history: async () => ({ events: history, more: false }) }))
    await screen.findByText('what is up?')
    expect(last()).toEqual({ top: 2000 })
  })

  it('follows content that grows without a new item, such as a streaming reply', async () => {
    const { src } = await show(fakeDeskApi())
    scrollTo.mockClear()
    act(() => src.emit('partial', { kind: 'partial', text: 'a' }))
    grow(2400)
    expect(last()).toEqual({ top: 2400 })
  })

  it('stays put once the reader scrolls up, and shows a way back', async () => {
    const { src } = await show(fakeDeskApi())
    scrollTop(600)
    scrollTo.mockClear()
    act(() => src.emit('partial', { kind: 'partial', text: 'more' }))
    grow(2600)
    expect(scrollTo).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Jump to latest' })).toBeInTheDocument()
  })

  it('stops following when the wheel goes up during a reply, before any scroll event', async () => {
    const { src } = await show(fakeDeskApi())
    act(() => src.emit('partial', { kind: 'partial', text: 'a' }))
    act(() => {
      window.dispatchEvent(new WheelEvent('wheel', { deltaY: -40 }))
    })
    scrollTo.mockClear()
    grow(2500)
    expect(scrollTo).not.toHaveBeenCalled()
  })

  it('follows again when the reader scrolls back to the bottom', async () => {
    await show(fakeDeskApi())
    scrollTop(600)
    grow(2600)
    scrollTo.mockClear()
    scrollTop(1900) // 1900 + 700 = 2600
    grow(2800)
    expect(last()).toEqual({ top: 2800 })
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).not.toBeInTheDocument()
  })

  it('does not take its own scrolling, or growth under it, for the reader leaving', async () => {
    await show(fakeDeskApi())
    grow(2500) // we scroll to the bottom, which the browser reports as scrollY 1800...
    scrollTo.mockClear()
    height = 3000 // ...but more arrives before that scroll event is handled
    scrollTop(1800)
    grow(3000)
    expect(last()).toEqual({ top: 3000 })
  })

  it('Jump to latest returns to the bottom and resumes following', async () => {
    const user = userEvent.setup()
    await show(fakeDeskApi())
    scrollTop(500)
    scrollTo.mockClear()
    await user.click(screen.getByRole('button', { name: 'Jump to latest' }))
    expect(last()).toEqual({ top: 2000 })
    grow(2200)
    expect(last()).toEqual({ top: 2200 })
  })
})

describe('Markdown', () => {
  it('renders lists and fenced code as plain text inside elements', () => {
    const { container } = render(<Markdown text={'- a\n- `b`\n\n```\n<i>raw</i>\n```'} />)
    expect(container.querySelectorAll('li')).toHaveLength(2)
    expect(container.querySelector('pre code')?.textContent).toBe('<i>raw</i>')
    expect(container.querySelector('i')).toBeNull()
  })
})
