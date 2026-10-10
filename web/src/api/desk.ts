// The front desk's endpoints (internal/api/desk.go). A browser session may call them;
// workspaceId 0 leaves workspace_id out, which the daemon takes as its only workspace.
import type { DeskHistory, DeskStatus, DeskTurnAccepted } from './types'
import { get, post, qs } from './client'

export const DESK_STREAM = '/v1/desk/stream'

function ws(workspaceId: number): Record<string, number> {
  return workspaceId > 0 ? { workspace_id: workspaceId } : {}
}

export const deskApi = {
  /** The newest `limit` events before `before` (0: the newest), oldest first. */
  history: (workspaceId: number, before = 0, limit = 100) =>
    get<DeskHistory>(`/v1/desk/history?${qs({ ...ws(workspaceId), ...(before > 0 ? { before } : {}), limit })}`),
  status: (workspaceId: number) => {
    const q = qs(ws(workspaceId))
    return get<DeskStatus>(`/v1/desk/status${q ? `?${q}` : ''}`)
  },
  turn: (workspaceId: number, text: string) =>
    post<DeskTurnAccepted>('/v1/desk/turn', { ...ws(workspaceId), text }),
  interrupt: (workspaceId: number) => post<{ interrupted: boolean }>('/v1/desk/interrupt', ws(workspaceId)),
  newConversation: (workspaceId: number) => post<{ workspace_id?: number }>('/v1/desk/new', ws(workspaceId)),
  /** An EventSource sends no headers, so it resumes by query: the cookie carries the session. */
  streamUrl: (workspaceId: number, after: number) => `${DESK_STREAM}?${qs({ ...ws(workspaceId), after })}`,
}

export type DeskApi = typeof deskApi
