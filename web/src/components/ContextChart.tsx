import { tokens } from '../format'
import { chartMax, LARGE_CONTEXT, type ChartSection } from '../model/usage'

const pct = (n: number, max: number) => `${Math.min(100, (n / max) * 100)}%`

/**
 * The largest context any run or turn of each group reached, as a bar from zero, with a
 * line at 200k and a tick at the group's model window when it fits. A bar that stays left of
 * the line is work a 200k window would have held. Plain HTML: no chart library, and the
 * tables beside it carry every number.
 */
export function ContextChart({ sections }: { sections: ChartSection[] }) {
  const max = chartMax(sections)
  const axis = [0, 100_000, LARGE_CONTEXT, 300_000, 400_000, 600_000, 800_000, 1_000_000].filter((t) => t <= max)
  return (
    <figure className="ctx-chart" aria-label="Peak context by group">
      <figcaption className="ctx-key">
        <span><i className="ctx-swatch ctx-swatch-bar" aria-hidden="true" /> Largest context reached</span>
        <span><i className="ctx-swatch ctx-swatch-line" aria-hidden="true" /> 200k</span>
        <span><i className="ctx-swatch ctx-swatch-tick" aria-hidden="true" /> Model window, where it fits</span>
      </figcaption>
      {sections.map((s) => (
        <section key={s.title} className="ctx-section" aria-label={s.title}>
          <h3>{s.title}</h3>
          <ul>
            {s.rows.map((r) => {
              const over = r.over > 0
              const detail =
                `${r.key}: peak ${tokens(r.peak)} tokens` +
                (r.window ? ` of a ${tokens(r.window)} window` : '') +
                `, ${r.over} of ${r.measured} over 200k`
              return (
                <li key={r.key} className="ctx-row" title={detail}>
                  <span className="ctx-label">
                    {r.prefix && <span className="faint">{r.prefix} /</span>} {r.label}
                  </span>
                  <span className="ctx-plot" aria-hidden="true">
                    <span className="ctx-bar" style={{ width: pct(r.peak, max) }} />
                    <span className="ctx-200k" style={{ left: pct(LARGE_CONTEXT, max) }} />
                    {r.window > 0 && r.window <= max && <span className="ctx-window" style={{ left: pct(r.window, max) }} />}
                  </span>
                  <span className="ctx-value">
                    <strong>{tokens(r.peak)}</strong>
                    {r.window > 0 && <span className="faint"> of {tokens(r.window)}</span>}
                    {over ? <span className="ctx-over"> · {r.over} over 200k</span> : <span className="faint"> · none over 200k</span>}
                  </span>
                </li>
              )
            })}
          </ul>
        </section>
      ))}
      <div className="ctx-axis" aria-hidden="true">
        <span />
        <span className="ctx-axis-scale">
          {axis.map((t) => (
            <span key={t} style={{ left: pct(t, max) }}>{t === 0 ? '0' : tokens(t)}</span>
          ))}
        </span>
        <span />
      </div>
    </figure>
  )
}
