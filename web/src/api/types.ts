// The daemon's JSON, from internal/api and internal/store. Times are RFC 3339 strings.
// Fields Go marks omitempty are optional here: the daemon leaves them out, not null.

export type LaneState = 'opening' | 'open' | 'closed'
export type RunState = 'running' | 'succeeded' | 'failed' | 'stopped' | 'interrupted'
export type DecisionState = 'open' | 'answered'
export type MRState = 'open' | 'merged' | 'closed' | 'unknown'
export type PipelineStatus = 'pending' | 'running' | 'passed' | 'failed' | 'canceled' | 'skipped' | 'unknown'

export interface Workspace {
  id: number
  name: string
  path: string
  created: string
}

export interface WorkspaceSummary extends Workspace {
  repos: number
  lanes: number
}

export interface Status {
  version: string
  pid: number
  started: string
  store: string
  config: string
  workspaces: WorkspaceSummary[]
}

export interface Origin {
  via?: string
  agent?: string
  session?: string
  run?: number
  pid?: number
  dir?: string
  detail?: string
}

export interface Lane {
  id: number
  repo_id: number
  name: string
  branch: string
  base: string
  worktree: string
  scope: string[]
  state: LaneState
  created: string
  closed?: string
  origin: Origin
}

/** A lane with its repo's name and the forge's last word on its branch. */
export interface LaneView extends Lane {
  repo: string
  mr?: number
  mr_state?: MRState
  mr_url?: string
  pipeline?: number
  pipeline_status?: PipelineStatus
}

export interface Run {
  id: number
  lane_id: number
  agent: string
  model?: string
  prompt: string
  state: RunState
  pid?: number
  log: string
  start_sha: string
  end_sha?: string
  commits: number
  outside?: string[]
  exit_code?: number
  error?: string
  started: string
  ended?: string
  session?: string
  parent?: number
  cost_usd?: number
  credits?: number
  session_usd?: number
  session_credits?: number
}

export interface RunView extends Run {
  lane: string
  repo: string
  worktree: string
}

export interface RunLog {
  data: string
  offset: number
  /** The run has ended and data reaches the end of its log. */
  done: boolean
}

export interface Decision {
  id: number
  run_id: number
  question: string
  options?: string[]
  recommendation?: string
  why?: string
  state: DecisionState
  answer?: string
  answer_run?: number
  created: string
  answered?: string
}

export interface DecisionView extends Decision {
  agent: string
  lane: string
  repo: string
}

export interface RunEvent {
  id: number
  run_id: number
  kind: string
  status?: string
  text: string
  created: string
}

export interface RunEvents {
  events: RunEvent[]
  decisions: Decision[]
}

export interface Request {
  id: number
  from_run: number
  kind: string
  lane?: string
  message: string
  state: string
  agent?: string
  target_run?: number
  reply?: string
  reply_run?: number
  depth: number
  note?: string
  created: string
  updated: string
}

export interface RequestView extends Request {
  from_agent: string
  from_lane: string
  repo: string
}

export interface FeedItem {
  id: number
  /** The store's raw kind; it may grow. Draw from the parallel event instead. */
  kind: string
  text: string
  /** The run, lane, decision or request the item is about, depending on its kind. */
  ref?: number
  created: string
}

export interface Feed {
  items: FeedItem[]
  /** Parallel to items: the event of each, from a closed set. Older daemons omit it. */
  events?: string[]
  last: number
}

export interface Spend {
  day: string
  source: string
  ref?: number
  usd: number
  credits?: number
}

export interface SpendToday {
  day: string
  usd: number
  budget: number
  credit_usd: number
  by_source: Record<string, Spend> | null
}

/** The front desk's stored event kinds (store.Desk*); a newer daemon may add more. */
export type DeskKind = 'user' | 'system' | 'turn_start' | 'assistant' | 'tool' | 'cost' | 'error' | 'turn_end' | 'new'

/** One entry of the desk conversation. A streamed "partial" piece has no seq and is never stored. */
export interface DeskEvent {
  seq: number
  workspace_id: number
  kind: string
  text?: string
  /** The seq of the message that started the turn this belongs to. */
  turn?: number
  /** Who started the turn: "human" or "system". */
  origin?: string
  created: string
}

export interface DeskHistory {
  /** Oldest first. */
  events: DeskEvent[]
  /** Older events remain: ask again with before set to the first event's seq. */
  more: boolean
}

export interface DeskStatus {
  workspace_id: number
  busy: boolean
  queued: number
  attached: number
  session: string
  model: string
  wake: string
}

export interface DeskTurnAccepted {
  workspace_id: number
  turn: number
  /** How many turns run before this one, the one in progress included. */
  ahead: number
}

export interface ApiError {
  error: string
}

/** How a closed lane ended: what the forge last said about its merge request. */
export type LaneOutcome = 'merged' | 'mr_closed' | 'dropped' | 'no_mr'

/** A lane in the history (GET /v1/history/lanes): its runs' count and cost, and how a closed one ended. */
export interface LaneRecord extends LaneView {
  outcome?: LaneOutcome
  runs: number
  cost_usd: number
  credits?: number
}

/** A run in the history, without its prompt; task is the prompt's first line. */
export interface RunRecord {
  id: number
  lane_id: number
  repo: string
  lane: string
  lane_state: LaneState
  agent: string
  model?: string
  state: RunState
  commits: number
  cost_usd?: number
  credits?: number
  started: string
  ended?: string
  task: string
}

/** GET /v1/history/runs: count and cost cover every matching run, runs may be cut at the limit. */
export interface RunHistory {
  runs: RunRecord[]
  count: number
  cost_usd: number
  credits: number
  /** What the range holds before the agent and lane filters, to offer as choices. */
  agents: string[]
  lanes: { id: number; repo: string; name: string }[]
}
