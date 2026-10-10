import { RANGES } from '../model/history'
import type { Range } from '../router'

/** Today or the last 7 days: links, so the choice is in the address and survives a reload. */
export function RangeFilter({ range, href }: { range: Range; href: (r: Range) => string }) {
  return (
    <nav className="range" aria-label="Time range">
      {RANGES.map((r) => (
        <a key={r.id} href={href(r.id)} aria-current={r.id === range ? 'true' : undefined}>
          {r.label}
        </a>
      ))}
    </nav>
  )
}
