// The daemon's answer to GET /v1/lanes/{id}/conversation (internal/convo's Thread).

export type ItemKind = 'run' | 'user' | 'agent' | 'thinking' | 'tool'
export type ToolStatus = 'running' | 'ok' | 'error' | 'no_result'

export interface Item {
  /** Position in the lane's thread, from 1: stable, so items merge by it. */
  seq: number
  kind: ItemKind
  run: number
  time?: string
  text?: string
  tool?: string
  summary?: string
  input?: string
  output?: string
  status?: ToolStatus
  /** Text, input or output was clipped; bytes is the longest's full length. */
  truncated?: boolean
  bytes?: number
  /** Run boundaries. A note says why the run has no transcript below it. */
  agent?: string
  state?: string
  end?: string
  session?: string
  note?: string
}

export interface Thread {
  items: Item[]
  cursor: number
  more: boolean
  running: boolean
}

/** What the tab needs from the daemon; tests give it fakes. */
export interface ConversationSource {
  read(after: number): Promise<Thread>
  /** One item with its full text. */
  expand(seq: number): Promise<Item>
}
