import { isValidElement, useState } from 'react'
import type { ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkBreaks from 'remark-breaks'
import remarkGfm from 'remark-gfm'
import { Check, Copy } from 'lucide-react'

function safeUrl(value: string): string {
  const trimmed = value.trim()
  if (!trimmed) return ''
  if (trimmed.startsWith('/api/attachments/')) return trimmed
  try {
    const parsed = new URL(trimmed, window.location.origin)
    if (parsed.protocol === 'http:' || parsed.protocol === 'https:' || parsed.protocol === 'mailto:') {
      return trimmed
    }
  } catch {
    return ''
  }
  return ''
}

function extractText(node: ReactNode): string {
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(extractText).join('')
  if (isValidElement<{ children?: ReactNode }>(node)) return extractText(node.props.children)
  return ''
}

function CodeBlock({ children }: { children: ReactNode }) {
  const [copied, setCopied] = useState(false)
  let lang = ''
  if (isValidElement<{ className?: string }>(children) && typeof children.props.className === 'string') {
    const match = /language-([A-Za-z0-9_+-]+)/.exec(children.props.className)
    if (match) lang = match[1]
  }
  const code = extractText(children).replace(/\n$/, '')

  async function copyCode() {
    try {
      await navigator.clipboard.writeText(code)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1600)
    } catch {
      // Ignore clipboard permission errors in restricted contexts.
    }
  }

  return (
    <div className="md-code-block">
      <div className="md-code-header">
        <span>{lang || 'text'}</span>
        <button type="button" className="md-copy-button" onClick={() => void copyCode()} aria-label="复制代码">
          {copied ? <><Check size={12} /> 已复制</> : <><Copy size={12} /> 复制</>}
        </button>
      </div>
      <pre>{children}</pre>
    </div>
  )
}

export function MarkdownContent({ text }: { text: string }) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm, remarkBreaks]}
      urlTransform={safeUrl}
      components={{
        pre: ({ children }) => <CodeBlock>{children}</CodeBlock>,
        table: ({ children }) => (
          <div className="md-table-wrap">
            <table>{children}</table>
          </div>
        ),
        a: ({ href, children }) => (
          href
            ? <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>
            : <span>{children}</span>
        ),
        img: ({ src, alt }) => (
          src && src.startsWith('/api/attachments/')
            ? <img src={src} alt={alt ?? ''} loading="lazy" />
            : src
              ? <a href={src} target="_blank" rel="noopener noreferrer">{alt || src}</a>
              : null
        ),
      }}
    >
      {text}
    </ReactMarkdown>
  )
}
