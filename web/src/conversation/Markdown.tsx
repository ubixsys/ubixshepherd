import { Fragment, type ReactNode } from 'react'

// What an agent wrote is text, never markup: elements are built here, so nothing of it
// reaches innerHTML, and a link survives only as an http or https anchor.

function safeHref(raw: string): string | null {
  try {
    const u = new URL(raw)
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : null
  } catch {
    return null
  }
}

const INLINE = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\([^)\s]+\))/

function inline(text: string): ReactNode[] {
  return text.split(INLINE).map((part, i) => {
    if (part.length > 2 && part.startsWith('`') && part.endsWith('`')) return <code key={i}>{part.slice(1, -1)}</code>
    if (part.length > 4 && part.startsWith('**') && part.endsWith('**')) return <strong key={i}>{part.slice(2, -2)}</strong>
    const m = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(part)
    if (m) {
      const url = safeHref(m[2] ?? '')
      if (url) {
        return (
          <a key={i} href={url} target="_blank" rel="noopener noreferrer">
            {m[1]}
          </a>
        )
      }
      return <Fragment key={i}>{`${m[1] ?? ''} (${m[2] ?? ''})`}</Fragment>
    }
    return <Fragment key={i}>{part}</Fragment>
  })
}

type Block =
  | { t: 'p'; text: string }
  | { t: 'h'; text: string }
  | { t: 'code'; text: string }
  | { t: 'list'; ordered: boolean; items: string[] }

/** A small markdown subset: paragraphs, headings, fenced code, bullet and numbered lists. */
export function blocks(src: string): Block[] {
  const out: Block[] = []
  const lines = src.replace(/\r\n?/g, '\n').split('\n')
  let para: string[] = []
  const flush = () => {
    if (para.length) out.push({ t: 'p', text: para.join('\n') })
    para = []
  }
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? ''
    if (/^\s*```/.test(line)) {
      flush()
      const code: string[] = []
      for (i++; i < lines.length && !/^\s*```/.test(lines[i] ?? ''); i++) code.push(lines[i] ?? '')
      out.push({ t: 'code', text: code.join('\n') })
      continue
    }
    const h = /^#{1,6}\s+(.*)$/.exec(line)
    const li = /^\s*(?:([-*])|\d+[.)])\s+(.*)$/.exec(line)
    if (h) {
      flush()
      out.push({ t: 'h', text: h[1] ?? '' })
    } else if (li) {
      flush()
      const ordered = !li[1]
      const last = out.at(-1)
      if (last?.t === 'list' && last.ordered === ordered) last.items.push(li[2] ?? '')
      else out.push({ t: 'list', ordered, items: [li[2] ?? ''] })
    } else if (line.trim() === '') {
      flush()
    } else {
      para.push(line)
    }
  }
  flush()
  return out
}

export function Markdown({ text }: { text: string }) {
  return (
    <div className="cv-md">
      {blocks(text).map((b, i) => {
        switch (b.t) {
          case 'code':
            return (
              <pre key={i}>
                <code>{b.text}</code>
              </pre>
            )
          case 'h':
            return (
              <p key={i} className="cv-md-h">
                {inline(b.text)}
              </p>
            )
          case 'list': {
            const items = b.items.map((it, j) => <li key={j}>{inline(it)}</li>)
            return b.ordered ? <ol key={i}>{items}</ol> : <ul key={i}>{items}</ul>
          }
          default:
            return <p key={i}>{inline(b.text)}</p>
        }
      })}
    </div>
  )
}
