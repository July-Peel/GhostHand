export const STANDARD_PALETTE = [
  '#2C2622', '#D8473F', '#26A06C', '#C49F00', '#3A78B8', '#B5768D', '#298A96', '#D8CBB5',
  '#655A50', '#F54E42', '#33B87D', '#EBC400', '#589AE8', '#D3869B', '#38A8B8', '#F6EFE2',
] as const

export const DEFAULT_BACKGROUND = STANDARD_PALETTE[0]
export const DEFAULT_FOREGROUND = STANDARD_PALETTE[7]
export const CURSOR_COLOR = STANDARD_PALETTE[15]
export const SELECTION_COLOR = 'rgba(41, 138, 150, 0.4)'

export const CellAttribute = { bold: 1, dim: 2, italic: 4, underline: 8, inverse: 16, hidden: 32, strike: 64 } as const

type TerminalStyle = { foreground: string; background: string; attributes: number }
export type TerminalCell = { text: string; width: 0 | 1 | 2; style: TerminalStyle }
export type TerminalLine = { cells: TerminalCell[]; wrapped: boolean }
export type TerminalPosition = { line: number; column: number }
export type TerminalFrame = {
  columns: number
  rows: number
  firstLine: number
  lines: TerminalLine[]
  cursorRow: number
  cursorColumn: number
  cursorVisible: boolean
}

type Cursor = { row: number; column: number; style: TerminalStyle; wrapPending: boolean }
type Screen = { lines: TerminalLine[]; cursor: Cursor; saved: Cursor; top: number; bottom: number }
type ParserState = 'text' | 'escape' | 'intermediate' | 'csi' | 'discardCSI' | 'osc' | 'oscEscape' | 'ignore' | 'ignoreEscape'

const defaultStyle: TerminalStyle = { foreground: DEFAULT_FOREGROUND, background: DEFAULT_BACKGROUND, attributes: 0 }
const defaultCell: TerminalCell = { text: ' ', width: 1, style: defaultStyle }
const scrollbackLimit = 5000
const esc = '\x1b'
const combiningMark = /\p{Mark}/u
const graphicCharacters: Record<string, string> = {
  '`': '◆', a: '▒', f: '°', g: '±', j: '┘', k: '┐', l: '┌', m: '└', n: '┼',
  o: '⎺', p: '⎻', q: '─', r: '⎼', s: '⎽', t: '├', u: '┤', v: '┴', w: '┬', x: '│',
  y: '≤', z: '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

export function clamp(value: number, min: number, max: number) {
  return Math.min(Math.max(value, min), max)
}

function rgb(red: number, green: number, blue: number) {
  return `#${[red, green, blue].map(value => clamp(value, 0, 255).toString(16).padStart(2, '0')).join('')}`
}

function indexedColor(index: number) {
  if (index >= 0 && index < 16) return STANDARD_PALETTE[index]
  if (index >= 16 && index < 232) {
    const levels = [0, 95, 135, 175, 215, 255]
    const color = index - 16
    return rgb(levels[Math.floor(color / 36)], levels[Math.floor(color / 6) % 6], levels[color % 6])
  }
  if (index >= 232 && index <= 255) {
    const gray = 8 + (index - 232) * 10
    return rgb(gray, gray, gray)
  }
  return DEFAULT_FOREGROUND
}

function characterWidth(text: string): 0 | 1 | 2 {
  const code = text.codePointAt(0)!
  if (code < 0x300) return 1
  if (combiningMark.test(text) || code === 0x200c || code === 0x200d || (code >= 0x1f3fb && code <= 0x1f3ff)) return 0
  return ((code >= 0x1100 && code <= 0x115f) || code === 0x2329 || code === 0x232a ||
    (code >= 0x2e80 && code <= 0xa4cf && code !== 0x303f) || (code >= 0xac00 && code <= 0xd7a3) ||
    (code >= 0xf900 && code <= 0xfaff) || (code >= 0xfe10 && code <= 0xfe19) ||
    (code >= 0xfe30 && code <= 0xfe6f) || (code >= 0xff01 && code <= 0xff60) ||
    (code >= 0xffe0 && code <= 0xffe6) || (code >= 0x1f300 && code <= 0x1faff) ||
    (code >= 0x20000 && code <= 0x3fffd)) ? 2 : 1
}

function cursor(): Cursor {
  return { row: 0, column: 0, style: defaultStyle, wrapPending: false }
}

function parseParameter(value: string) {
  return /^\d+$/.test(value) ? Math.min(Number(value), 32767) : 0
}

export class TerminalBuffer {
  private primary: Screen
  private alternate: Screen | null = null
  private history: TerminalLine[] = []
  private historyEnd = 0
  private historyStart = 0
  private state: ParserState = 'text'
  private sequence = ''
  private originMode = false
  private autoWrap = true
  private insertMode = false
  private cursorVisible = true
  private charsets = [false, false]
  private activeCharset = 0
  private tabs = new Set<number>()
  bracketedPasteEnabled = false
  applicationCursorKeys = false

  constructor(public columns: number, public rows: number) {
    this.primary = this.newScreen()
    this.addTabStops(0)
  }

  get isAlternateScreen() { return this.alternate !== null }
  get scrollbackCount() { return this.alternate ? 0 : Math.min(scrollbackLimit, this.historyEnd - this.historyStart) }
  get activeLine() { return this.alternate ? 0 : this.historyEnd }
  private get screen() { return this.alternate ?? this.primary }
  private get cursor() { return this.screen.cursor }

  private newLine(cell = defaultCell): TerminalLine {
    return { cells: Array<TerminalCell>(this.columns).fill(cell), wrapped: false }
  }

  private newScreen(): Screen {
    return { lines: Array.from({ length: this.rows }, () => this.newLine()), cursor: cursor(), saved: cursor(), top: 0, bottom: this.rows - 1 }
  }

  private addTabStops(from: number) {
    for (let column = Math.max(8, Math.ceil(from / 8) * 8); column < this.columns; column += 8) this.tabs.add(column)
  }

  private remember(line: TerminalLine) {
    this.history[this.historyEnd % scrollbackLimit] = line
    this.historyEnd++
  }

  private line(index: number) {
    if (index < this.activeLine) return this.history[index % scrollbackLimit]
    return this.screen.lines[index - this.activeLine]
  }

  frame(offset: number): TerminalFrame {
    const firstLine = this.activeLine - clamp(offset, 0, this.scrollbackCount)
    const cursorRow = this.activeLine + this.cursor.row - firstLine
    return {
      columns: this.columns,
      rows: this.rows,
      firstLine,
      lines: Array.from({ length: this.rows }, (_, row) => this.line(firstLine + row)),
      cursorRow,
      cursorColumn: this.cursor.column,
      cursorVisible: this.cursorVisible && cursorRow < this.rows,
    }
  }

  selectionText(start: TerminalPosition, end: TerminalPosition) {
    const first = Math.max(start.line, this.activeLine - this.scrollbackCount)
    const last = Math.min(end.line, this.activeLine + this.rows - 1)
    let text = ''
    for (let index = first; index <= last; index++) {
      const line = this.line(index)
      const maxColumns = Math.min(line.cells.length, this.columns)
      let from = index === start.line ? Math.min(start.column, maxColumns) : 0
      const to = index === end.line ? Math.min(end.column, maxColumns - 1) : maxColumns - 1
      if (line.cells[from]?.width === 0) from--
      const content = line.cells.slice(from, to + 1).map(cell => cell.text).join('')
      const joined = index < last && line.wrapped
      text += joined ? content : content.trimEnd()
      if (index < last && !joined) text += '\n'
    }
    return text
  }

  resize(columns: number, rows: number) {
    if (columns === this.columns && rows === this.rows) return
    const previousColumns = this.columns
    this.columns = columns
    this.rows = rows
    this.addTabStops(previousColumns)
    this.resizeScreen(this.primary, true)
    if (this.alternate) this.resizeScreen(this.alternate, false)
  }

  private resizeScreen(screen: Screen, keepHistory: boolean) {
    let usedRows = screen.lines.length
    while (usedRows > screen.cursor.row + 1 && screen.lines[usedRows - 1].cells.every(cell => cell.text === ' ' && cell.style.background === DEFAULT_BACKGROUND)) usedRows--
    const removed = Math.max(0, usedRows - this.rows)
    if (keepHistory) screen.lines.slice(0, removed).forEach(line => this.remember(line))
    screen.lines = screen.lines.slice(removed, usedRows).map(line => {
      const cells = line.cells.slice(0, this.columns)
      if (cells[cells.length - 1]?.width === 2) cells[cells.length - 1] = defaultCell
      while (cells.length < this.columns) cells.push(defaultCell)
      return { cells, wrapped: line.wrapped }
    })
    while (screen.lines.length < this.rows) screen.lines.push(this.newLine())
    for (const position of [screen.cursor, screen.saved]) {
      position.row = clamp(position.row - removed, 0, this.rows - 1)
      position.column = Math.min(position.column, this.columns - 1)
      position.wrapPending = false
    }
    screen.top = 0
    screen.bottom = this.rows - 1
  }

  process(text: string): { responses: string[]; title?: string } {
    const responses: string[] = []
    let title: string | undefined
    for (const char of text) {
      if (char === '\x18' || char === '\x1a') {
        this.state = 'text'
        this.sequence = ''
        continue
      }
      if (this.state === 'osc' || this.state === 'oscEscape') {
        if (char === '\x07' || char === '\x9c' || (this.state === 'oscEscape' && char === '\\')) {
          const separator = this.sequence.indexOf(';')
          const command = this.sequence.slice(0, separator)
          if (separator !== -1 && (command === '0' || command === '2')) title = this.sequence.slice(separator + 1)
          this.state = 'text'
          this.sequence = ''
        } else if (char === esc) {
          this.state = 'oscEscape'
        } else {
          if (this.sequence.length < 4096 && char >= ' ') this.sequence += char
          this.state = 'osc'
        }
        continue
      }
      if (this.state === 'ignore' || this.state === 'ignoreEscape') {
        if (char === '\x9c' || (this.state === 'ignoreEscape' && char === '\\')) this.state = 'text'
        else this.state = char === esc ? 'ignoreEscape' : 'ignore'
        continue
      }
      if (char === esc) {
        this.sequence = ''
        this.state = 'escape'
      } else if (char === '\x9b') {
        this.sequence = ''
        this.state = 'csi'
      } else if (char === '\x9d') {
        this.sequence = ''
        this.state = 'osc'
      } else if (char === '\x90' || char === '\x98' || char === '\x9e' || char === '\x9f') {
        this.state = 'ignore'
      } else if (char < ' ' || (char >= '\x7f' && char <= '\x9f')) {
        this.control(char)
      } else if (this.state === 'escape' || this.state === 'intermediate') {
        this.escape(char)
      } else if (this.state === 'csi' || this.state === 'discardCSI') {
        if (char >= '@' && char <= '~') {
          if (this.state === 'csi') this.csi(char, this.sequence, responses)
          this.sequence = ''
          this.state = 'text'
        } else if (this.sequence.length < 128 && this.state === 'csi') {
          this.sequence += char
        } else {
          this.state = 'discardCSI'
        }
      } else {
        this.put(this.charsets[this.activeCharset] ? graphicCharacters[char] ?? char : char)
      }
    }
    return { responses, title }
  }

  private control(char: string) {
    switch (char) {
      case '\r': this.move(this.cursor.row, 0); break
      case '\n': case '\x0b': case '\x0c': case '\x84': this.lineFeed(); break
      case '\x85': this.move(this.cursor.row, 0); this.lineFeed(); break
      case '\x8d': this.reverseIndex(); break
      case '\b': this.move(this.cursor.row, this.cursor.column - 1); break
      case '\t': this.tab(1); break
      case '\x0e': this.activeCharset = 1; break
      case '\x0f': this.activeCharset = 0; break
    }
  }

  private escape(char: string) {
    if (char >= ' ' && char <= '/') {
      if (this.sequence.length < 4) this.sequence += char
      this.state = 'intermediate'
      return
    }
    const intermediate = this.sequence
    this.state = 'text'
    this.sequence = ''
    if (intermediate) {
      if (intermediate === '(' || intermediate === ')') this.charsets[intermediate === '(' ? 0 : 1] = char === '0'
      return
    }
    switch (char) {
      case '[': this.state = 'csi'; break
      case ']': this.state = 'osc'; break
      case 'P': case 'X': case '^': case '_': this.state = 'ignore'; break
      case '7': this.saveCursor(); break
      case '8': this.restoreCursor(); break
      case 'D': this.lineFeed(); break
      case 'E': this.move(this.cursor.row, 0); this.lineFeed(); break
      case 'M': this.reverseIndex(); break
      case 'H': this.tabs.add(this.cursor.column); break
      case 'c': this.reset(); break
    }
  }

  private csi(command: string, raw: string, responses: string[]) {
    const privateMode = raw.startsWith('?')
    const secondary = raw.startsWith('>')
    const parameters = (privateMode || secondary ? raw.slice(1) : raw).split(';')
    if (command === 'm' && !privateMode && !secondary) { this.sgr(parameters); return }
    if (parameters.some(parameter => !/^\d*$/.test(parameter))) return
    const values = parameters.map(parseParameter)
    const count = values[0] || 1
    const { row, column } = this.cursor
    const { top, bottom } = this.screen
    const minRow = this.originMode || row >= top && row <= bottom ? top : 0
    const maxRow = this.originMode || row >= top && row <= bottom ? bottom : this.rows - 1
    const origin = this.originMode ? top : 0
    switch (command) {
      case 'A': this.move(Math.max(minRow, row - count), column); break
      case 'B': case 'e': this.move(Math.min(maxRow, row + count), column); break
      case 'C': case 'a': this.move(row, column + count); break
      case 'D': this.move(row, column - count); break
      case 'E': this.move(Math.min(maxRow, row + count), 0); break
      case 'F': this.move(Math.max(minRow, row - count), 0); break
      case 'G': case '`': this.move(row, count - 1); break
      case 'd': this.move(Math.min(this.originMode ? bottom : this.rows - 1, origin + count - 1), column); break
      case 'H': case 'f': this.move(Math.min(this.originMode ? bottom : this.rows - 1, origin + count - 1), (values[1] || 1) - 1); break
      case 'I': this.tab(count); break
      case 'Z': this.tab(-count); break
      case 'g':
        if (values[0] === 0) this.tabs.delete(column)
        else if (values[0] === 3) this.tabs.clear()
        break
      case 'J': this.eraseDisplay(values[0]); break
      case 'K': this.eraseLine(values[0]); break
      case '@': this.shiftCharacters(count); break
      case 'P': this.shiftCharacters(-count); break
      case 'X': this.erase(this.screen.lines[row], column, Math.min(this.columns, column + count)); break
      case 'L': case 'M':
        if (row >= top && row <= bottom) this.scroll(row, bottom, command === 'L' ? -count : count, false)
        break
      case 'S': this.scroll(top, bottom, count); break
      case 'T': this.scroll(top, bottom, -count); break
      case 'r':
        if (privateMode || secondary) break
        this.screen.top = clamp(count - 1, 0, this.rows - 1)
        this.screen.bottom = clamp((values[1] || this.rows) - 1, 0, this.rows - 1)
        if (this.screen.top >= this.screen.bottom) { this.screen.top = 0; this.screen.bottom = this.rows - 1 }
        this.move(this.originMode ? this.screen.top : 0, 0)
        break
      case 's': this.saveCursor(); break
      case 'u': this.restoreCursor(); break
      case 'n':
        if (values[0] === 5 && !privateMode) responses.push(`${esc}[0n`)
        if (values[0] === 6) responses.push(`${esc}[${privateMode ? '?' : ''}${row - origin + 1};${column + 1}R`)
        break
      case 'c':
        if (values[0] === 0) responses.push(secondary ? `${esc}[>0;0;0c` : `${esc}[?1;2c`)
        break
      case 'h': case 'l': this.setModes(values, command === 'h', privateMode); break
    }
  }

  private sgr(parameters: string[]) {
    let style = { ...this.cursor.style }
    for (let i = 0; i < parameters.length; i++) {
      const [code, ...sub] = parameters[i].split(':')
      const value = parseParameter(code)
      if (value === 38 || value === 48) {
        let color: string | undefined
        if (sub.length) {
          if (sub[0] === '5' && sub.length === 2) color = indexedColor(parseParameter(sub[1]))
          if (sub[0] === '2' && sub.length >= 4) {
            const [red, green, blue] = sub.slice(-3).map(parseParameter)
            color = rgb(red, green, blue)
          }
        } else if (parameters[i + 1] === '5' && i + 2 < parameters.length) {
          color = indexedColor(parseParameter(parameters[i + 2]))
          i += 2
        } else if (parameters[i + 1] === '2' && i + 4 < parameters.length) {
          color = rgb(parseParameter(parameters[i + 2]), parseParameter(parameters[i + 3]), parseParameter(parameters[i + 4]))
          i += 4
        }
        if (color) style[value === 38 ? 'foreground' : 'background'] = color
        continue
      }
      if (value >= 30 && value <= 37) style.foreground = STANDARD_PALETTE[value - 30]
      else if (value >= 40 && value <= 47) style.background = STANDARD_PALETTE[value - 40]
      else if (value >= 90 && value <= 97) style.foreground = STANDARD_PALETTE[value - 90 + 8]
      else if (value >= 100 && value <= 107) style.background = STANDARD_PALETTE[value - 100 + 8]
      else switch (value) {
        case 0: style = { ...defaultStyle }; break
        case 1: style.attributes |= CellAttribute.bold; break
        case 2: style.attributes |= CellAttribute.dim; break
        case 3: style.attributes |= CellAttribute.italic; break
        case 4: style.attributes |= CellAttribute.underline; break
        case 7: style.attributes |= CellAttribute.inverse; break
        case 8: style.attributes |= CellAttribute.hidden; break
        case 9: style.attributes |= CellAttribute.strike; break
        case 22: style.attributes &= ~(CellAttribute.bold | CellAttribute.dim); break
        case 23: style.attributes &= ~CellAttribute.italic; break
        case 24: style.attributes &= ~CellAttribute.underline; break
        case 27: style.attributes &= ~CellAttribute.inverse; break
        case 28: style.attributes &= ~CellAttribute.hidden; break
        case 29: style.attributes &= ~CellAttribute.strike; break
        case 39: style.foreground = DEFAULT_FOREGROUND; break
        case 49: style.background = DEFAULT_BACKGROUND; break
      }
    }
    this.cursor.style = style
  }

  private setModes(values: number[], enabled: boolean, privateMode: boolean) {
    for (const value of values) {
      if (!privateMode) {
        if (value === 4) this.insertMode = enabled
        continue
      }
      switch (value) {
        case 1: this.applicationCursorKeys = enabled; break
        case 6: this.originMode = enabled; this.move(enabled ? this.screen.top : 0, 0); break
        case 7: this.autoWrap = enabled; this.cursor.wrapPending = false; break
        case 25: this.cursorVisible = enabled; break
        case 47: case 1047: case 1049:
          if (enabled && !this.alternate) this.alternate = this.newScreen()
          else if (!enabled) this.alternate = null
          break
        case 1048:
          if (enabled) this.saveCursor()
          else this.restoreCursor()
          break
        case 2004: this.bracketedPasteEnabled = enabled; break
      }
    }
  }

  private reset() {
    this.alternate = null
    this.primary = this.newScreen()
    this.history = []
    this.historyStart = this.historyEnd
    this.cursorVisible = this.autoWrap = true
    this.applicationCursorKeys = this.bracketedPasteEnabled = this.originMode = this.insertMode = false
    this.charsets = [false, false]
    this.activeCharset = 0
    this.tabs.clear()
    this.addTabStops(0)
  }

  private saveCursor() { this.screen.saved = { ...this.cursor } }
  private restoreCursor() { this.screen.cursor = { ...this.screen.saved } }

  private move(row: number, column: number) {
    this.cursor.row = clamp(row, 0, this.rows - 1)
    this.cursor.column = clamp(column, 0, this.columns - 1)
    this.cursor.wrapPending = false
  }

  private tab(count: number) {
    const direction = Math.sign(count)
    let column = this.cursor.column
    for (let steps = Math.abs(count); steps > 0; steps--) {
      do { column += direction } while (column > 0 && column < this.columns - 1 && !this.tabs.has(column))
      if (column <= 0 || column >= this.columns - 1) break
    }
    this.move(this.cursor.row, column)
  }

  private blank(): TerminalCell {
    const { background, foreground, attributes } = this.cursor.style
    const color = attributes & CellAttribute.inverse ? foreground : background
    return color === DEFAULT_BACKGROUND ? defaultCell : { text: ' ', width: 1, style: { ...defaultStyle, background: color } }
  }

  private put(text: string) {
    const width = characterWidth(text)
    const current = this.cursor
    if (width === 0) {
      let line = this.screen.lines[current.row]
      let column = current.column - (current.wrapPending ? 0 : 1)
      if (column < 0 && current.row > 0 && this.screen.lines[current.row - 1].wrapped) {
        line = this.screen.lines[current.row - 1]
        column = this.columns - 1
      }
      if (line.cells[column]?.width === 0) column--
      const cell = line.cells[column]
      if (cell && cell.text.length < 64) line.cells[column] = { ...cell, text: cell.text + text }
      return
    }
    if (this.autoWrap && (current.wrapPending || current.column + width > this.columns)) {
      this.screen.lines[current.row].wrapped = true
      current.column = 0
      this.lineFeed()
    }
    if (current.column + width > this.columns) current.column = this.columns - width
    if (this.insertMode) this.shiftCharacters(width)
    const line = this.screen.lines[current.row]
    const column = current.column
    this.erase(line, column, column + width)
    line.cells[column] = { text, width, style: current.style }
    if (width === 2) line.cells[column + 1] = { text: '', width: 0, style: current.style }
    current.wrapPending = this.autoWrap && column + width === this.columns
    current.column = Math.min(column + width, this.columns - 1)
  }

  private lineFeed() {
    const { top, bottom } = this.screen
    if (this.cursor.row === bottom) this.scroll(top, bottom, 1)
    else this.cursor.row = Math.min(this.rows - 1, this.cursor.row + 1)
    this.cursor.wrapPending = false
  }

  private reverseIndex() {
    const { top, bottom } = this.screen
    if (this.cursor.row === top) this.scroll(top, bottom, -1)
    else this.cursor.row = Math.max(0, this.cursor.row - 1)
    this.cursor.wrapPending = false
  }

  private scroll(top: number, bottom: number, count: number, saveHistory = true) {
    const size = Math.min(Math.abs(count), bottom - top + 1)
    const lines = this.screen.lines
    const blank = this.blank()
    const added = Array.from({ length: size }, () => this.newLine(blank))
    if (count > 0) {
      const removed = lines.splice(top, size)
      lines.splice(bottom - size + 1, 0, ...added)
      if (saveHistory && !this.alternate && top === 0 && bottom === this.rows - 1) removed.forEach(line => this.remember(line))
    } else {
      lines.splice(bottom - size + 1, size)
      lines.splice(top, 0, ...added)
    }
  }

  private erase(line: TerminalLine, start: number, end: number) {
    const { cells } = line
    if (cells[start]?.width === 0) start--
    if (cells[end]?.width === 0) end++
    cells.fill(this.blank(), start, end)
    if (end >= this.columns) line.wrapped = false
  }

  private eraseLine(mode: number) {
    const { row, column } = this.cursor
    if (mode === 0) this.erase(this.screen.lines[row], column, this.columns)
    else if (mode === 1) this.erase(this.screen.lines[row], 0, column + 1)
    else if (mode === 2) this.erase(this.screen.lines[row], 0, this.columns)
    this.cursor.wrapPending = false
  }

  private eraseDisplay(mode: number) {
    const { row } = this.cursor
    if (mode === 0 || mode === 1) {
      this.eraseLine(mode)
      const from = mode === 0 ? row + 1 : 0
      const to = mode === 0 ? this.rows : row
      for (let i = from; i < to; i++) this.erase(this.screen.lines[i], 0, this.columns)
    } else if (mode === 2 || mode === 3) {
      for (const line of this.screen.lines) this.erase(line, 0, this.columns)
      this.cursor.wrapPending = false
      if (mode === 3) {
        this.history = []
        this.historyStart = this.historyEnd
      }
    }
  }

  private shiftCharacters(count: number) {
    const { row, column } = this.cursor
    const { cells } = this.screen.lines[row]
    const size = Math.min(Math.abs(count), this.columns - column)
    const blank = this.blank()
    if (count > 0) {
      cells.copyWithin(column + size, column, this.columns - size)
      cells.fill(blank, column, column + size)
    } else {
      cells.copyWithin(column, column + size)
      cells.fill(blank, this.columns - size)
    }
    for (let i = Math.max(0, column - 1); i < this.columns; i++) {
      if ((cells[i].width === 0 && cells[i - 1]?.width !== 2) || (cells[i].width === 2 && cells[i + 1]?.width !== 0)) cells[i] = blank
    }
    this.cursor.wrapPending = false
  }
}
