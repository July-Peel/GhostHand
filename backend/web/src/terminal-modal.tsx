import { useId, useLayoutEffect, useRef, useState } from 'react'
import { Terminal, X } from 'lucide-react'
import { CellAttribute, clamp, CURSOR_COLOR, DEFAULT_BACKGROUND, SELECTION_COLOR, TerminalBuffer } from './terminal-buffer'
import type { TerminalCell, TerminalFrame, TerminalPosition } from './terminal-buffer'

export type TerminalShell = 'cmd' | 'powershell'
export const terminalShellNames: Record<TerminalShell, string> = { cmd: 'CMD', powershell: 'PowerShell' }

type Selection = { start: TerminalPosition; end: TerminalPosition }
type Status = { text: string; state: 'connecting' | 'connected' | 'closed' }
type TerminalElements = {
  dialog: HTMLDialogElement
  surface: HTMLDivElement
  canvas: HTMLCanvasElement
  input: HTMLTextAreaElement
  scrollbar: HTMLDivElement
  scrollTrack: HTMLDivElement
}

const font = '14px "Cascadia Mono", Consolas, "Courier New", "Microsoft YaHei Mono", monospace'
const cellHeight = 20
const esc = '\x1b'
const tildeKeys: Record<string, number> = {
  Insert: 2, Delete: 3, PageUp: 5, PageDown: 6,
  F5: 15, F6: 17, F7: 18, F8: 19, F9: 20, F10: 21, F11: 23, F12: 24,
}
const cursorKeys: Record<string, string> = { ArrowUp: 'A', ArrowDown: 'B', ArrowRight: 'C', ArrowLeft: 'D', Home: 'H', End: 'F' }
const functionKeys: Record<string, string> = { F1: 'P', F2: 'Q', F3: 'R', F4: 'S' }
const controlKeys: Record<string, string> = { ' ': '\x00', '@': '\x00', '[': esc, '\\': '\x1c', ']': '\x1d', '^': '\x1e', _: '\x1f', '?': '\x7f' }

function keySequence(event: KeyboardEvent, applicationKeys: boolean) {
  const { key, shiftKey, altKey, ctrlKey } = event
  const modifier = 1 + Number(shiftKey) + 2 * Number(altKey) + 4 * Number(ctrlKey)
  const cursorKey = cursorKeys[key]
  if (cursorKey) return modifier > 1 ? `${esc}[1;${modifier}${cursorKey}` : `${esc}${applicationKeys ? 'O' : '['}${cursorKey}`
  const functionKey = functionKeys[key]
  if (functionKey) return modifier > 1 ? `${esc}[1;${modifier}${functionKey}` : `${esc}O${functionKey}`
  const tildeKey = tildeKeys[key]
  if (tildeKey) return `${esc}[${tildeKey}${modifier > 1 ? `;${modifier}` : ''}~`
  if (key === 'Tab') return shiftKey ? `${esc}[Z` : '\t'
  if (key === 'Enter') return `${altKey ? esc : ''}\r`
  if (key === 'Backspace') return `${altKey ? esc : ''}${ctrlKey ? '\x08' : '\x7f'}`
  if (ctrlKey && !event.getModifierState('AltGraph')) {
    if (/^[a-z]$/i.test(key)) return `${altKey ? esc : ''}${String.fromCharCode(key.toUpperCase().charCodeAt(0) - 64)}`
    return controlKeys[key]
  }
  if (altKey && !event.getModifierState('AltGraph') && key.length === 1) return esc + key
}

function foreground(cell: TerminalCell) {
  return cell.style.attributes & CellAttribute.inverse ? cell.style.background : cell.style.foreground
}

function background(cell: TerminalCell) {
  return cell.style.attributes & CellAttribute.inverse ? cell.style.foreground : cell.style.background
}

function drawText(ctx: CanvasRenderingContext2D, text: string, cell: TerminalCell, color: string, x: number, y: number, width: number) {
  const { attributes } = cell.style
  if (attributes & CellAttribute.hidden) return
  ctx.fillStyle = color
  ctx.globalAlpha = attributes & CellAttribute.dim ? 0.55 : 1
  ctx.font = `${attributes & CellAttribute.italic ? 'italic ' : ''}${attributes & CellAttribute.bold ? 'bold ' : ''}${font}`
  if (text.trim()) ctx.fillText(text, x, y + cellHeight / 2, width)
  if (attributes & CellAttribute.underline) ctx.fillRect(x, y + cellHeight - 3, width, 1)
  if (attributes & CellAttribute.strike) ctx.fillRect(x, y + cellHeight / 2, width, 1)
  ctx.globalAlpha = 1
}

function paint(ctx: CanvasRenderingContext2D, frame: TerminalFrame, selection: Selection | null, cellWidth: number, focused: boolean, blink: boolean) {
  for (let row = 0; row < frame.rows; row++) {
    const { cells } = frame.lines[row]
    const columns = Math.min(cells.length, frame.columns)
    const y = row * cellHeight
    for (let column = 0; column < columns;) {
      const color = background(cells[column])
      let end = column + 1
      while (end < columns && background(cells[end]) === color) end++
      if (color !== DEFAULT_BACKGROUND) {
        ctx.fillStyle = color
        ctx.fillRect(column * cellWidth, y, (end - column) * cellWidth, cellHeight)
      }
      column = end
    }
    const line = frame.firstLine + row
    if (selection && line >= selection.start.line && line <= selection.end.line) {
      let start = line === selection.start.line ? selection.start.column : 0
      let end = line === selection.end.line ? selection.end.column : frame.columns - 1
      if (cells[start]?.width === 0) start--
      if (cells[end]?.width === 2) end++
      ctx.fillStyle = SELECTION_COLOR
      ctx.fillRect(start * cellWidth, y, (Math.min(end, frame.columns - 1) - start + 1) * cellWidth, cellHeight)
    }
    for (let column = 0; column < columns;) {
      const cell = cells[column]
      if (cell.width === 0) { column++; continue }
      let end = column + cell.width
      let text = cell.text
      if (/^[ -~]$/.test(text)) {
        while (end < columns && cells[end].style === cell.style && /^[ -~]$/.test(cells[end].text)) text += cells[end++].text
      }
      if (cell.style.attributes || text.trim()) {
        drawText(ctx, text, cell, foreground(cell), column * cellWidth, y, (end - column) * cellWidth)
      }
      column = end
    }
  }
  if (!frame.cursorVisible || (focused && !blink)) return
  const { cursorRow: row, cursorColumn: column } = frame
  const cells = frame.lines[row].cells
  const start = cells[column]?.width === 0 ? column - 1 : column
  const under = cells[start]
  const width = Math.max(1, under?.width ?? 1) * cellWidth
  const x = start * cellWidth
  const y = row * cellHeight
  if (focused) {
    ctx.fillStyle = CURSOR_COLOR
    ctx.fillRect(x, y, width, cellHeight)
    if (under) drawText(ctx, under.text, under, background(under), x, y, width)
  } else {
    ctx.strokeStyle = CURSOR_COLOR
    ctx.lineWidth = 1.5
    ctx.strokeRect(x + 0.75, y + 0.75, width - 1.5, cellHeight - 1.5)
  }
}

function attachTerminal(elements: TerminalElements, deviceID: string, shell: TerminalShell, updateStatus: (status: Status) => void, updateTitle: (title: string) => void, notice: (text: string) => void) {
  const { dialog, surface, canvas, input, scrollbar, scrollTrack } = elements
  const ctx = canvas.getContext('2d', { alpha: false })
  if (!ctx) {
    updateStatus({ state: 'closed', text: '浏览器无法绘制终端。' })
    return
  }
  const buffer = new TerminalBuffer(120, 32)
  const listeners = new AbortController()
  const { signal } = listeners
  let socket: WebSocket | undefined
  let ended = false
  let cellWidth = 8
  let offset = 0
  let wheelPixels = 0
  let selection: Selection | null = null
  let drag: { pointer: number; anchor: TerminalPosition } | null = null
  let composing = false
  let compositionTimer: number | undefined
  let blink = true
  let drawing = 0
  let pending: string[] = []
  let pendingSize = 0

  function draw() {
    drawing = 0
    const width = surface.clientWidth
    const height = surface.clientHeight
    if (!width || !height || !ctx) return
    const scale = window.devicePixelRatio || 1
    const pixelWidth = Math.max(1, Math.round(width * scale))
    const pixelHeight = Math.max(1, Math.round(height * scale))
    if (canvas.width !== pixelWidth || canvas.height !== pixelHeight) {
      canvas.width = pixelWidth
      canvas.height = pixelHeight
    }
    ctx.setTransform(pixelWidth / width, 0, 0, pixelHeight / height, 0, 0)
    ctx.textBaseline = 'middle'
    ctx.textAlign = 'left'
    ctx.fontKerning = 'none'
    ctx.fillStyle = DEFAULT_BACKGROUND
    ctx.fillRect(0, 0, width, height)
    const frame = buffer.frame(offset)
    if (ended) frame.cursorVisible = false
    paint(ctx, frame, selection, cellWidth, document.activeElement === input, blink)
    input.style.left = `${Math.min(width - 1, frame.cursorColumn * cellWidth)}px`
    input.style.top = `${Math.max(0, Math.min(height - cellHeight, frame.cursorRow * cellHeight))}px`
    scrollTrack.style.height = `${scrollbar.clientHeight + buffer.scrollbackCount * cellHeight}px`
    scrollbar.scrollTop = (buffer.scrollbackCount - offset) * cellHeight
  }

  function scheduleDraw() {
    if (!drawing) drawing = requestAnimationFrame(draw)
  }

  function finish(text: string) {
    if (ended) return
    ended = true
    pending = []
    pendingSize = 0
    notice('')
    updateStatus({ state: 'closed', text })
    socket?.close(1000)
    scheduleDraw()
  }

  function send(command: { type: 'input'; data: string } | { type: 'resize'; cols: number; rows: number }) {
    if (socket?.readyState !== WebSocket.OPEN) return
    if (socket.bufferedAmount > 256 * 1024) {
      finish('网络发送缓冲区已满，终端连接已结束。')
      return
    }
    socket.send(JSON.stringify(command))
  }

  function sendInput(text: string, user = true) {
    if (ended || !text) return
    if (user) {
      offset = 0
      selection = null
      blink = true
      notice('')
      scheduleDraw()
    }
    if (socket?.readyState !== WebSocket.OPEN) {
      pendingSize += text.length
      if (pendingSize > 64 * 1024) finish('等待连接期间的终端输入过多。')
      else pending.push(text)
      return
    }
    for (let start = 0; start < text.length && !ended;) {
      let end = Math.min(start + 4096, text.length)
      if (end < text.length && /[\uD800-\uDBFF]/.test(text[end - 1])) end--
      send({ type: 'input', data: text.slice(start, end) })
      start = end
    }
  }

  function resize() {
    if (!ctx || !surface.clientWidth || !surface.clientHeight) return
    ctx.font = font
    cellWidth = ctx.measureText('M').width || 8
    const cols = clamp(Math.floor(surface.clientWidth / cellWidth), 20, 500)
    const rows = clamp(Math.floor(surface.clientHeight / cellHeight), 5, 200)
    if (cols !== buffer.columns || rows !== buffer.rows) {
      const activeLine = buffer.activeLine
      buffer.resize(cols, rows)
      if (offset) offset = clamp(offset + buffer.activeLine - activeLine, 0, buffer.scrollbackCount)
      selection = null
      send({ type: 'resize', cols, rows })
    }
    scheduleDraw()
  }

  function connect() {
    if (ended) return
    resize()
    const cols = buffer.columns
    const rows = buffer.rows
    const url = new URL(`/api/devices/${encodeURIComponent(deviceID)}/terminal/ws`, window.location.href)
    url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    url.search = new URLSearchParams({ shell, cols: String(cols), rows: String(rows) }).toString()
    try {
      socket = new WebSocket(url)
    } catch {
      finish('无法连接设备终端。')
      return
    }
    socket.addEventListener('open', () => {
      if (ended) return
      updateStatus({ state: 'connected', text: '已连接' })
      if (cols !== buffer.columns || rows !== buffer.rows) send({ type: 'resize', cols: buffer.columns, rows: buffer.rows })
      const queued = pending
      pending = []
      pendingSize = 0
      queued.forEach(text => sendInput(text, false))
    }, { signal })
    socket.addEventListener('message', event => {
      if (ended || typeof event.data !== 'string') return
      try {
        const message = JSON.parse(event.data) as { type: string; data?: string; exit_code?: number; error?: string }
        if (message.type === 'output' && typeof message.data === 'string') {
          const activeLine = buffer.activeLine
          const alternate = buffer.isAlternateScreen
          const result = buffer.process(message.data)
          if (buffer.isAlternateScreen !== alternate) {
            offset = 0
            selection = null
            drag = null
          } else if (offset) {
            offset = clamp(offset + buffer.activeLine - activeLine, 0, buffer.scrollbackCount)
          }
          if (selection && selection.end.line < buffer.activeLine - buffer.scrollbackCount) selection = null
          if (result.title !== undefined) updateTitle(result.title)
          result.responses.forEach(response => sendInput(response, false))
          scheduleDraw()
        } else if (message.type === 'exit') {
          finish(message.error || (message.exit_code === undefined ? '终端会话已结束。' : `Shell 已退出（退出码 ${message.exit_code}）。`))
        }
      } catch {
        finish('收到无效的终端数据。')
      }
    }, { signal })
    socket.addEventListener('error', () => finish('无法连接设备终端，请确认 Windows 节点在线且支持远程终端。'), { signal })
    socket.addEventListener('close', () => finish('终端连接已断开。'), { signal })
  }

  function position(event: PointerEvent): TerminalPosition {
    const rect = canvas.getBoundingClientRect()
    return {
      line: buffer.activeLine - offset + clamp(Math.floor((event.clientY - rect.top) / cellHeight), 0, buffer.rows - 1),
      column: clamp(Math.floor((event.clientX - rect.left) / cellWidth), 0, buffer.columns - 1),
    }
  }

  function copyText(text: string) {
    if (!text) return
    function copyThroughInput() {
      input.value = text
      input.focus({ preventScroll: true })
      input.select()
      let copied = false
      try {
        copied = document.execCommand('copy')
      } catch {
        copied = false
      } finally {
        input.value = ''
      }
      if (!copied) notice('浏览器无法访问剪贴板。')
    }
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(text).catch(() => { if (!signal.aborted) copyThroughInput() })
    } else {
      copyThroughInput()
    }
  }

  function paste(text: string) {
    if (!text) return
    sendInput(buffer.bracketedPasteEnabled
      ? `${esc}[200~${text.replace(/\r\n/g, '\n')}${esc}[201~`
      : text.replace(/\r\n|\n/g, '\r'))
  }

  function pasteClipboard() {
    if (!navigator.clipboard?.readText) {
      notice('请按 Ctrl+V 粘贴。')
      return
    }
    navigator.clipboard.readText().then(text => { if (!signal.aborted) paste(text) }).catch(() => {
      if (!signal.aborted) notice('浏览器未允许读取剪贴板，请按 Ctrl+V 粘贴。')
    })
  }

  dialog.addEventListener('keydown', event => {
    // Workspace shortcuts such as Ctrl+K must stay out of the terminal.
    event.stopPropagation()
    if (event.key === 'Escape') {
      event.preventDefault()
      sendInput(esc)
      return
    }
    if (event.target !== input || event.isComposing || composing || event.key === 'Process') return
    const shortcut = (event.ctrlKey || event.metaKey) && !event.altKey
    if (shortcut && event.key.toLowerCase() === 'c') {
      event.preventDefault()
      if (selection) copyText(buffer.selectionText(selection.start, selection.end))
      else sendInput('\x03')
      return
    }
    // Let editing shortcuts deliver the browser's native paste event.
    if ((shortcut && event.key.toLowerCase() === 'v') ||
      (event.shiftKey && event.key === 'Insert' && !event.ctrlKey && !event.altKey && !event.metaKey)) return
    const sequence = keySequence(event, buffer.applicationCursorKeys)
    if (sequence) {
      event.preventDefault()
      sendInput(sequence)
    }
  }, { signal })
  input.addEventListener('input', event => {
    if (composing || (event as InputEvent).isComposing || compositionTimer !== undefined) return
    const text = input.value
    input.value = ''
    sendInput(text.replace(/\n/g, '\r'))
  }, { signal })
  input.addEventListener('compositionstart', () => { composing = true }, { signal })
  input.addEventListener('compositionend', event => {
    composing = false
    sendInput(event.data)
    window.clearTimeout(compositionTimer)
    compositionTimer = window.setTimeout(() => {
      input.value = ''
      compositionTimer = undefined
    }, 0)
  }, { signal })
  input.addEventListener('paste', event => {
    event.preventDefault()
    paste(event.clipboardData?.getData('text/plain') ?? '')
  }, { signal })
  input.addEventListener('focus', () => { blink = true; scheduleDraw() }, { signal })
  input.addEventListener('blur', scheduleDraw, { signal })

  canvas.addEventListener('pointerdown', event => {
    if (event.button !== 0) return
    event.preventDefault()
    input.focus({ preventScroll: true })
    drag = { pointer: event.pointerId, anchor: position(event) }
    selection = null
    canvas.setPointerCapture(event.pointerId)
    scheduleDraw()
  }, { signal })
  canvas.addEventListener('pointermove', event => {
    if (drag?.pointer !== event.pointerId) return
    const { anchor } = drag
    const point = position(event)
    if (point.line === anchor.line && point.column === anchor.column) selection = null
    else if (point.line < anchor.line || (point.line === anchor.line && point.column < anchor.column)) selection = { start: point, end: anchor }
    else selection = { start: anchor, end: point }
    scheduleDraw()
  }, { signal })
  canvas.addEventListener('lostpointercapture', () => { drag = null }, { signal })
  canvas.addEventListener('contextmenu', event => {
    event.preventDefault()
    input.focus({ preventScroll: true })
    if (selection) {
      copyText(buffer.selectionText(selection.start, selection.end))
      selection = null
      scheduleDraw()
    } else {
      pasteClipboard()
    }
  }, { signal })
  surface.addEventListener('wheel', event => {
    event.preventDefault()
    if (!buffer.scrollbackCount) return
    const unit = event.deltaMode === 1 ? cellHeight : event.deltaMode === 2 ? surface.clientHeight : 1
    wheelPixels += event.deltaY * unit
    const lines = Math.trunc(wheelPixels / cellHeight)
    wheelPixels -= lines * cellHeight
    offset = clamp(offset - lines, 0, buffer.scrollbackCount)
    scheduleDraw()
  }, { signal, passive: false })
  scrollbar.addEventListener('scroll', () => {
    const next = clamp(buffer.scrollbackCount - Math.round(scrollbar.scrollTop / cellHeight), 0, buffer.scrollbackCount)
    if (offset === next) return
    offset = next
    scheduleDraw()
  }, { signal })
  scrollbar.addEventListener('pointerup', () => input.focus({ preventScroll: true }), { signal })
  window.addEventListener('pagehide', () => finish('终端连接已断开。'), { signal })
  window.addEventListener('resize', resize, { signal })
  const observer = new ResizeObserver(resize)
  observer.observe(surface)
  const blinker = window.setInterval(() => {
    if (ended) return
    blink = !blink
    scheduleDraw()
  }, 500)
  resize()
  const opening = requestAnimationFrame(connect)
  return () => {
    listeners.abort()
    observer.disconnect()
    cancelAnimationFrame(opening)
    cancelAnimationFrame(drawing)
    window.clearInterval(blinker)
    window.clearTimeout(compositionTimer)
    if (socket && socket.readyState < WebSocket.CLOSING) socket.close(1000)
  }
}

export function TerminalModal({ deviceID, deviceName, shell, onClose }: { deviceID: string; deviceName: string; shell: TerminalShell; onClose: () => void }) {
  const headingID = useId()
  const dialog = useRef<HTMLDialogElement>(null)
  const surface = useRef<HTMLDivElement>(null)
  const canvas = useRef<HTMLCanvasElement>(null)
  const input = useRef<HTMLTextAreaElement>(null)
  const scrollbar = useRef<HTMLDivElement>(null)
  const scrollTrack = useRef<HTMLDivElement>(null)
  const [status, updateStatus] = useState<Status>({ state: 'connecting', text: '正在连接…' })
  const [title, updateTitle] = useState('')
  const [notice, setNotice] = useState('')

  useLayoutEffect(() => {
    const dialogElement = dialog.current!
    const inputElement = input.current!
    dialogElement.showModal()
    const cleanup = attachTerminal({
      dialog: dialogElement, input: inputElement, surface: surface.current!, canvas: canvas.current!,
      scrollbar: scrollbar.current!, scrollTrack: scrollTrack.current!,
    }, deviceID, shell, updateStatus, updateTitle, setNotice)
    inputElement.focus({ preventScroll: true })
    return () => {
      cleanup?.()
      dialogElement.close()
    }
  }, [deviceID, shell])

  return (
    <dialog
      ref={dialog} className="terminal-dialog" aria-labelledby={headingID}
      onCancel={event => event.preventDefault()}
      onClick={event => { if (event.target === event.currentTarget) input.current?.focus({ preventScroll: true }) }}
    >
      <header className="terminal-header">
        <Terminal size={17} aria-hidden="true" />
        <h2 id={headingID} title={title || deviceName}>{terminalShellNames[shell]} · {deviceName}</h2>
        <button type="button" className="icon-button terminal-close" title="关闭终端" aria-label="关闭终端" onClick={onClose}><X size={19} /></button>
      </header>
      <div className="terminal-viewport">
        <div ref={surface} className="terminal-surface">
          <canvas ref={canvas} aria-hidden="true" />
          <textarea ref={input} className="terminal-input" aria-label={`${terminalShellNames[shell]} 终端输入`} spellCheck={false} autoCapitalize="off" autoComplete="off" autoCorrect="off" />
        </div>
        <div ref={scrollbar} className="terminal-scrollbar" aria-hidden="true"><div ref={scrollTrack} /></div>
      </div>
      <div role="status" className={`terminal-status ${status.state}`}>{notice || status.text}</div>
    </dialog>
  )
}
