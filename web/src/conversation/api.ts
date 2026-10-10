import { get, qs } from '../api/client'
import type { ConversationSource, Item, Thread } from './types'

/** The conversation of one lane, read from the daemon. */
export function laneSource(laneId: number): ConversationSource {
  const path = (params: Record<string, string | number>) => `/v1/lanes/${laneId}/conversation?${qs(params)}`
  return {
    read: (after) => get<Thread>(path({ after })),
    expand: async (seq) => {
      const th = await get<Thread>(path({ expand: seq }))
      const it: Item | undefined = th.items[0]
      if (!it) throw new Error(`item ${seq} is gone`)
      return it
    },
  }
}
