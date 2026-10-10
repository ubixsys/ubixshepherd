import { useEffect, useState } from 'react'
import { useSnapshot } from './context'

export interface Loaded<T> {
  data: T | null
  error: string | null
  /** A load is under way; data is the last answer, if any. */
  loading: boolean
}

/**
 * Loads something from the daemon's history endpoints for the workspaces it lists, again
 * when the key changes and whenever the feed moves (a lane closing, a run ending). The
 * last answer stays on show while the next loads.
 */
export function useHistory<T>(key: string, load: (workspaceIds: number[]) => Promise<T>): Loaded<T> {
  const snap = useSnapshot()
  const ids = snap.status?.workspaces.map((w) => w.id).join(',') ?? ''
  const feedLast = snap.feed.at(-1)?.id ?? 0
  const [state, setState] = useState<Loaded<T> & { key: string }>({ data: null, error: null, loading: true, key })

  useEffect(() => {
    if (!ids) return
    let current = true
    load(ids.split(',').map(Number)).then(
      (data) => current && setState({ data, error: null, loading: false, key }),
      (e: unknown) => current && setState((s) => ({ ...s, error: e instanceof Error ? e.message : String(e), loading: false, key })),
    )
    return () => {
      current = false
    }
    // load closes over what key names.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids, key, feedLast])

  // A new key's answer is not the old key's: show nothing rather than the wrong range.
  if (state.key !== key) return { data: null, error: null, loading: true }
  return state
}
