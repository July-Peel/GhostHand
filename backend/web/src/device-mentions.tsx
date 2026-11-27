import type { Device } from './api'

function mentionBoundary(text: string) {
  return text !== '@' && text !== '-' && text !== '_' && /[\s\p{P}]/u.test(text)
}

function mentionStartBoundary(text: string) {
  return mentionBoundary(text) || /\p{Script=Han}/u.test(text)
}

function startsMention(text: string, start: number) {
  return start === 0 || mentionStartBoundary(Array.from(text.slice(Math.max(0, start - 2), start)).at(-1) ?? '')
}

export function detectMentionQuery(text: string, cursor: number) {
  if (cursor < 1) return null
  const start = text.lastIndexOf('@', cursor - 1)
  if (start < 0 || !startsMention(text, start)) return null
  const query = text.slice(start + 1, cursor)
  return /\s/u.test(query) ? null : { start, end: cursor, query }
}

export function deviceMentionTokens(devices: Device[]) {
  const tokens = new Map<string, Device | null>()
  for (const device of devices) {
    if (device.name) tokens.set(device.name, tokens.has(device.name) ? null : device)
  }
  for (const device of devices) tokens.set(device.id, device)
  return new Map([...tokens].sort(([a], [b]) => b.length - a.length))
}

export function renderDeviceMentions(text: string, tokens: ReadonlyMap<string, Device | null>, selfID: string) {
  if (!text.includes('@')) return text
  const parts = []
  let offset = 0
  for (let start = text.indexOf('@'); start >= 0; start = text.indexOf('@', start + 1)) {
    if (!startsMention(text, start)) continue
    for (const [token, device] of tokens) {
      const end = start + 1 + token.length
      const next = text.codePointAt(end)
      if (!text.startsWith(token, start + 1) || (next !== undefined && !mentionBoundary(String.fromCodePoint(next)))) continue
      if (device && device.id !== selfID) {
        if (start > offset) parts.push(text.slice(offset, start))
        parts.push(<span key={start} className="mention-tag" title={device.id}>@{device.name || device.id}</span>)
        offset = end
      }
      start = end - 1
      break
    }
  }
  if (parts.length === 0) return text
  if (offset < text.length) parts.push(text.slice(offset))
  return parts
}
