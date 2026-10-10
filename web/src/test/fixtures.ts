import type { DeskApi } from '../api/desk'
import type { Source } from '../state/desk'
import type {
  LaneView,
  RunView,
  DecisionView,
  RequestView,
  FeedItem,
  SpendToday,
  Status,
  LaneState,
  RunState,
  DecisionState,
  DeskEvent
} from '../api/types';

export function lane(o?: Partial<LaneView>): LaneView {
  const base: LaneView = {
    id: 1,
    repo_id: 1,
    name: "feat/login",
    branch: "feat/login",
    base: "dev",
    worktree: "/tmp/acme-api",
    scope: ["src/auth/**"],
    state: "open" as LaneState,
    created: "2026-10-08T10:00:00Z",
    origin: { via: "cli" },
    repo: "acme-api",
  };
  return { ...base, ...o };
}

export function run(o?: Partial<RunView>): RunView {
  const base: RunView = {
    id: 10,
    lane_id: 1,
    agent: "claude",
    model: "claude-sonnet",
    prompt: "",
    state: "succeeded" as RunState,
    log: "",
    start_sha: "abc123",
    end_sha: "def456",
    commits: 1,
    started: "2026-10-08T10:00:00Z",
    ended: "2026-10-08T10:04:00Z",
    cost_usd: 0.42,
    lane: "feat/login",
    repo: "acme-api",
    worktree: "/tmp/acme-api",
  };
  return { ...base, ...o };
}

export function decision(o?: Partial<DecisionView>): DecisionView {
  const base: DecisionView = {
    id: 5,
    run_id: 10,
    question: "Should we use authentication?",
    options: ["Yes", "No"],
    recommendation: "Yes",
    why: "Because it's important for security",
    state: "open" as DecisionState,
    agent: "claude",
    lane: "feat/login",
    repo: "acme-api",
    created: "2026-10-08T10:00:00Z",
  };
  return { ...base, ...o };
}

export function request(o?: Partial<RequestView>): RequestView {
  const base: RequestView = {
    id: 3,
    from_run: 10,
    kind: "question",
    state: "needs_routing",
    message: "What should we do about authentication?",
    depth: 0,
    from_agent: "claude",
    from_lane: "feat/login",
    repo: "acme-api",
    created: "2026-10-08T10:00:00Z",
    updated: "2026-10-08T10:00:00Z",
  };
  return { ...base, ...o };
}

export function feedItem(o?: Partial<FeedItem>): FeedItem {
  const base: FeedItem = {
    id: 100,
    kind: "run_passed",
    text: "run 10: claude in lane feat/login succeeded, 1 commit(s)",
    ref: 10,
    created: "2026-10-08T10:00:00Z",
  };
  return { ...base, ...o };
}

export function spend(o?: Partial<SpendToday>): SpendToday {
  const base: SpendToday = {
    day: "2026-10-08",
    usd: 4.1,
    budget: 20,
    credit_usd: 0.04,
    by_source: {},
  };
  return { ...base, ...o };
}

export function status(o?: Partial<Status>): Status {
  const base: Status = {
    version: "0.0.0",
    pid: 1234,
    started: "2026-10-08T10:00:00Z",
    store: "/tmp/store",
    config: "/tmp/config",
    workspaces: [
      {
        id: 1,
        name: "workspace-1",
        path: "/tmp/workspace-1",
        created: "2026-10-08T10:00:00Z",
        repos: 1,
        lanes: 1,
      }
    ],
  };
  return { ...base, ...o };
}

export function deskEvent(o?: Partial<DeskEvent>): DeskEvent {
  const base: DeskEvent = {
    seq: 1,
    workspace_id: 1,
    kind: "user",
    text: "what is the flock doing?",
    turn: 1,
    origin: "human",
    created: "2026-10-08T10:00:00Z",
  };
  return { ...base, ...o };
}

/** A stand-in for EventSource: the test drives open, error and events by hand. */
export class FakeSource implements Source {
  static all: FakeSource[] = []
  /** The nth stream opened so far. */
  static get(n = 0): FakeSource {
    const s = FakeSource.all[n]
    if (!s) throw new Error(`no stream ${n} opened`)
    return s
  }
  onopen: ((ev: Event) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  closed = false
  private handlers = new Map<string, ((ev: MessageEvent<string>) => void)[]>()
  constructor(readonly url: string) {
    FakeSource.all.push(this)
  }
  addEventListener(type: string, fn: (ev: MessageEvent<string>) => void) {
    this.handlers.set(type, [...(this.handlers.get(type) ?? []), fn])
  }
  close() {
    this.closed = true
  }
  open() {
    this.onopen?.(new Event('open'))
  }
  fail() {
    this.onerror?.(new Event('error'))
  }
  emit(type: string, data: unknown) {
    for (const fn of this.handlers.get(type) ?? []) fn({ data: JSON.stringify(data) } as MessageEvent<string>)
  }
}

export function fakeDeskApi(over: Partial<DeskApi> = {}): DeskApi {
  return {
    history: async () => ({ events: [], more: false }),
    status: async () => ({ workspace_id: 1, busy: false, queued: 0, attached: 1, session: '', model: '', wake: 'attached' }),
    turn: async () => ({ workspace_id: 1, turn: 9, ahead: 1 }),
    interrupt: async () => ({ interrupted: true }),
    newConversation: async () => ({}),
    streamUrl: (ws, after) => `/stream?ws=${ws}&after=${after}`,
    ...over,
  }
}

