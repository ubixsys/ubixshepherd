import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'

/** How close to the bottom, in px, still counts as being at it. */
const NEAR_BOTTOM = 24

/**
 * Keeps the page at the bottom of a growing thread while the reader is there, and leaves
 * them alone once they scroll up. The same behaviour as the chat page's hook, kept here
 * so the two pages do not depend on each other: the reader leaves by scrolling up
 * (scrollY falls, or the wheel goes up) and returns by reaching the bottom, while our
 * own scrolling and content growth never move scrollY up. `deps` re-pins on our own
 * changes, and a ResizeObserver on `target` on content that grows by itself.
 */
export function useFollowBottom(target: RefObject<HTMLElement | null>, deps: unknown[]) {
  const pinned = useRef(true)
  const lastY = useRef(0)
  const [following, setFollowing] = useState(true)

  const pin = useCallback((v: boolean) => {
    if (pinned.current !== v) {
      pinned.current = v
      setFollowing(v)
    }
  }, [])
  const toBottom = useCallback(() => {
    const el = document.documentElement
    window.scrollTo({ top: el.scrollHeight })
    lastY.current = Math.max(0, el.scrollHeight - window.innerHeight)
  }, [])
  const jump = useCallback(() => {
    pin(true)
    toBottom()
  }, [pin, toBottom])

  useEffect(() => {
    const onScroll = () => {
      const y = window.scrollY
      if (window.innerHeight + y >= document.documentElement.scrollHeight - NEAR_BOTTOM) pin(true)
      else if (y < lastY.current) pin(false)
      lastY.current = y
    }
    const onWheel = (e: WheelEvent) => {
      if (e.deltaY < 0) pin(false)
    }
    const onResize = () => {
      if (pinned.current) toBottom()
    }
    lastY.current = window.scrollY
    window.addEventListener('scroll', onScroll, { passive: true })
    window.addEventListener('wheel', onWheel, { passive: true })
    window.addEventListener('resize', onResize)
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(onResize)
    if (target.current) ro?.observe(target.current)
    return () => {
      window.removeEventListener('scroll', onScroll)
      window.removeEventListener('wheel', onWheel)
      window.removeEventListener('resize', onResize)
      ro?.disconnect()
    }
  }, [target, pin, toBottom])

  useEffect(() => {
    if (pinned.current) toBottom()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [toBottom, ...deps])

  return { following, jump }
}
