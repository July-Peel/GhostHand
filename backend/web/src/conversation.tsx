import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ClipboardEvent, DragEvent, FormEvent, KeyboardEvent, ReactNode } from 'react'
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Activity, ArrowLeft, ArrowUp, BatteryCharging, Camera, Check, ChevronDown, CircleAlert, Copy, Download, Info, LoaderCircle, Plus, Send, Shield, Terminal, Trash2, X, Zap } from 'lucide-react'
import { api, devicesKey, errorText, FREE_CHAT_ID, messagesKey } from './api'
import type { Command, ConfirmCard, Device, MediaCard, Message, Session, StatusCard, TaskAction, TaskCard } from './api'
import { Battery, Connection, Details, DeviceIcon, Panel, desktops } from './device-ui'
import { detectMentionQuery, deviceMentionTokens, renderDeviceMentions } from './device-mentions'
import { BrowserExtensionSection, DeviceOperations, SSHSection } from './agent-ui'
import { agentAPI, connectionKey } from './agent-api'
import { RDPSection } from './rdp-ui'
import { MarkdownContent } from './markdown'
import { acceptedChatImages, maxChatImages, prepareChatImage } from './chat-images'
import type { PendingImage } from './chat-images'
import { TerminalModal, terminalShellNames } from './terminal-modal'
import type { TerminalShell } from './terminal-modal'

const timeFormat = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
const stateText: Record<TaskCard['state'], string> = {
  queued: '排队中', dispatched: '待接收', running: '执行中', paused: '已暂停', waiting_user: '等待用户',
  cancelling: '终止中', succeeded: '已完成', cancelled: '已取消', expired: '已过期', failed: '失败', unknown: '待核实',
}
const stepText: Record<string, string> = {
  running: '开始', waiting_user: '待确认', succeeded: '完成', failed: '失败', cancelled: '已取消', expired: '已过期',
}
const taskErrors: Record<string, string> = {
  E_DESKTOP_LOCKED: '请登录或解锁 Windows 桌面后重新截图。',
  E_HELPER_UNAVAILABLE: 'Windows 执行器暂时不可用。',
  E_UPLOAD: '截图上传失败。',
  E_CAP_UNSUPPORTED: '设备不支持此项任务。',
  E_BUSY: 'Windows 执行器正在处理另一项截图。',
  E_TIMEOUT: '等待设备操作超时。',
  E_INTERNAL: '任务执行时发生错误。',
}
const commandIcons = { 'device.battery': Zap, 'device.info': Info, 'screen.capture': Camera }
type PendingAttachment = PendingImage & { id: number }

export function Conversation({ device, devices, commands, session, opened, back }: {
  device: Device
  devices: Device[]
  commands: Command[]
  session: Session
  opened: boolean
  back: () => void
}) {
  const isFreeChat = device.id === FREE_CHAT_ID
  const isWindowsDevice = !isFreeChat && !device.is_demo && device.platform === 'windows'
  const isLinuxDevice = !isFreeChat && !device.is_demo && device.platform === 'linux'
  const isAgentAICapable = isWindowsDevice || isLinuxDevice
  const terminalOnline = device.conn_state === 'online' || device.conn_state === 'unstable'
  const chatBusy = isFreeChat && device.task_state !== 'idle'
  const [activeTerminal, setActiveTerminal] = useState<TerminalShell | null>(null)
  const [details, setDetails] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)
  const [draft, setDraft] = useState('')
  const [slashDismissed, dismissSlash] = useState(false)
  const [mentionCursor, setMentionCursor] = useState<number | null>(null)
  const [mentionIndex, setMentionIndex] = useState(0)
  const [mentionDismissed, dismissMention] = useState(false)
  const mentionListID = useId()
  const activeMentionRef = useRef<HTMLButtonElement>(null)
  const mentionCaret = useRef<number | null>(null)
  const [pendingImages, setPendingImages] = useState<PendingAttachment[]>([])
  const nextImageID = useRef(0)
  const [imagePreview, setImagePreview] = useState<PendingImage | null>(null)
  const [imageError, setImageError] = useState('')
  const [preparing, setPreparing] = useState(false)
  const preparingRef = useRef(false)
  const [dragging, setDragging] = useState(false)
  const scroller = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const pinned = useRef(true)
  const olderPosition = useRef<{ height: number; top: number } | null>(null)
  const client = useQueryClient()
  const key = messagesKey(device.id)
  const history = useInfiniteQuery({
    queryKey: key,
    initialPageParam: 0,
    queryFn: ({ pageParam, signal }) => api.messages(device.id, pageParam, signal),
    getNextPageParam: page => page.has_older ? page.messages[0]?.id : undefined,
  })
  const messages = (history.data?.pages ?? []).slice().reverse().flatMap(page => page.messages)
  const physicalDevices = useMemo(() => devices.filter(d => !d.is_demo && !d.revoked && d.id !== FREE_CHAT_ID), [devices])
  const mentionTokens = useMemo(() => deviceMentionTokens(physicalDevices), [physicalDevices])
  const mention = mentionCursor === null || mentionDismissed ? null : detectMentionQuery(draft, mentionCursor)
  const mentionQuery = mention?.query.toLowerCase() ?? ''
  const mentionOptions = mention ? physicalDevices.filter(d => d.id !== device.id &&
    [d.name, d.group, d.os_version, d.id].some(field => field.toLowerCase().includes(mentionQuery)))
      .sort((a, b) => Number(b.conn_state === 'online') - Number(a.conn_state === 'online')) : []
  const activeMentionIndex = Math.min(mentionIndex, mentionOptions.length - 1)
  const activeMention = mentionOptions[activeMentionIndex]
  const availableCommands = isFreeChat ? [] : commands.filter(command => {
    if (device.is_demo) return command.capability !== 'screen.capture'
    return device.platform === 'windows' || command.capability === 'device.info'
  })
  const prefix = draft.trim().toLowerCase()
  const suggestions = !activeMention && !slashDismissed && prefix.startsWith('/')
    ? availableCommands.filter(command => command.slash.startsWith(prefix))
    : []

  const send = useMutation({
    mutationFn: ({ text, images }: { text: string; images: PendingAttachment[] }) =>
      api.send(device.id, text, session.csrf_token, images.map(image => image.file)),
    onSuccess: async (_, { text, images }) => {
      setDraft(current => current.trim() === text ? '' : current)
      setPendingImages(current => current.filter(image => !images.includes(image)))
      pinned.current = true
      void client.invalidateQueries({ queryKey: key })
      if (isFreeChat) await client.invalidateQueries({ queryKey: devicesKey })
    },
  })

  const policy = useMutation({
    mutationFn: (autoApprove: boolean) => api.setDevicePolicy(device.id, autoApprove, session.csrf_token),
    onSettled: () => {
      void client.invalidateQueries({ queryKey: devicesKey })
      void client.invalidateQueries({ queryKey: key })
    },
  })

  const clear = useMutation({
    mutationFn: () => api.clearMessages(device.id, session.csrf_token),
    onSuccess: async () => {
      setConfirmClear(false)
      if (isFreeChat) {
        setDraft('')
        setMentionCursor(null)
        dismissMention(true)
        setPendingImages([])
        setImagePreview(null)
        setImageError('')
        send.reset()
      }
      await client.cancelQueries({ queryKey: key })
      client.setQueryData(key, { pages: [{ messages: [], has_older: false }], pageParams: [0] })
      void client.invalidateQueries({ queryKey: key })
      void client.invalidateQueries({ queryKey: devicesKey })
    },
  })

  const removeDevice = useMutation({
    mutationFn: () => agentAPI.deleteDevice(device.id, session.csrf_token),
    onSuccess: () => {
      setDetails(false)
      client.removeQueries({ queryKey: key })
      client.removeQueries({ queryKey: connectionKey(device.id) })
      void client.invalidateQueries({ queryKey: devicesKey })
      back()
    },
  })

  useEffect(() => {
    const node = composerRef.current
    if (!node) return
    node.style.height = 'auto'
    node.style.height = Math.min(node.scrollHeight, 160) + 'px'
  }, [draft])

  useLayoutEffect(() => {
    const caret = mentionCaret.current
    const input = composerRef.current
    if (caret === null || !input) return
    mentionCaret.current = null
    input.focus()
    input.setSelectionRange(caret, caret)
  }, [draft, mentionCursor])

  const activeMentionID = activeMention?.id
  useEffect(() => {
    if (activeMentionID) activeMentionRef.current?.scrollIntoView({ block: 'nearest' })
  }, [activeMentionID])

  useLayoutEffect(() => {
    const node = scroller.current
    if (!node) return
    if (olderPosition.current) {
      if (history.isFetchingNextPage) return
      node.scrollTop = olderPosition.current.top + node.scrollHeight - olderPosition.current.height
      olderPosition.current = null
    } else if (pinned.current) {
      node.scrollTop = node.scrollHeight
    }
  }, [history.data, history.isFetchingNextPage, opened, pendingImages.length])

  function submit(text: string) {
    const value = text.trim()
    if ((!value && pendingImages.length === 0) || send.isPending || clear.isPending || chatBusy || preparingRef.current) return
    if (!isFreeChat && pendingImages.length > 0 && (value.startsWith('/') || commands.some(command => command.title === value))) {
      setImageError('快捷指令不支持携带图片，请直接发送文字指令或移除图片。')
      return
    }
    setImageError('')
    dismissSlash(true)
    dismissMention(true)
    send.mutate({ text: value, images: pendingImages })
  }

  async function addImages(files: File[]) {
    if (files.length === 0 || send.isPending || clear.isPending) return
    if (preparingRef.current) {
      setImageError('正在处理图片，请稍后再添加。')
      return
    }
    preparingRef.current = true
    setPreparing(true)
    setImageError('')
    const added: PendingAttachment[] = []
    const errors = new Set<string>()
    try {
      for (const file of files) {
        if (pendingImages.length + added.length >= maxChatImages) {
          errors.add('单条消息最多上传 4 张图片。')
          break
        }
        try {
          added.push({ ...await prepareChatImage(file), id: nextImageID.current++ })
        } catch (error) {
          errors.add(error instanceof Error ? error.message : '无法读取这张图片。')
        }
      }
      setPendingImages(current => [...current, ...added])
      if (errors.size > 0) setImageError(Array.from(errors).join(' '))
    } finally {
      preparingRef.current = false
      setPreparing(false)
    }
  }

  function pasteImages(event: ClipboardEvent<HTMLTextAreaElement>) {
    const files = Array.from(event.clipboardData.items).flatMap(item => {
      const file = item.kind === 'file' ? item.getAsFile() : null
      return file ? [file] : []
    })
    if (files.length === 0) return
    event.preventDefault()
    const text = event.clipboardData.getData('text/plain')
    if (text) {
      const input = event.currentTarget
      const selected = input.selectionEnd - input.selectionStart
      input.setRangeText(text.slice(0, input.maxLength - input.value.length + selected), input.selectionStart, input.selectionEnd, 'end')
      setDraft(input.value)
      dismissSlash(false)
      updateMention(input, true)
    }
    void addImages(files)
  }

  function dragImages(event: DragEvent<HTMLDivElement>) {
    if (!event.dataTransfer.types.includes('Files')) return
    event.preventDefault()
    const blocked = send.isPending || clear.isPending || preparing
    event.dataTransfer.dropEffect = blocked ? 'none' : 'copy'
    setDragging(!blocked)
  }

  function dropImages(event: DragEvent<HTMLDivElement>) {
    if (!event.dataTransfer.types.includes('Files')) return
    event.preventDefault()
    setDragging(false)
    void addImages(Array.from(event.dataTransfer.files))
  }

  function submitForm(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    submit(draft)
  }

  function composerKey(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.nativeEvent.isComposing || event.keyCode === 229) return
    if (activeMention) {
      switch (event.key) {
        case 'ArrowDown':
        case 'ArrowUp': {
          event.preventDefault()
          const offset = event.key === 'ArrowDown' ? 1 : -1
          setMentionIndex((activeMentionIndex + offset + mentionOptions.length) % mentionOptions.length)
          return
        }
        case 'Escape':
          event.preventDefault()
          dismissMention(true)
          return
        case 'Tab':
        case 'Enter':
          if (event.shiftKey) break
          event.preventDefault()
          completeMention(activeMention)
          return
      }
    }
    if (event.key === 'Escape') dismissSlash(true)
    if (event.key !== 'Enter' || event.shiftKey) return
    event.preventDefault()
    submit(suggestions.length === 1 ? suggestions[0].slash : draft)
  }

  function updateMention(input: HTMLTextAreaElement, edited = false) {
    const cursor = input.selectionStart === input.selectionEnd ? input.selectionStart : null
    if (edited || cursor !== mentionCursor) {
      setMentionCursor(cursor)
      setMentionIndex(0)
      dismissMention(false)
    }
  }

  function completeMention(target: Device) {
    const input = composerRef.current
    if (!input || !mention) return
    const name = mentionTokens.get(target.name)?.id === target.id ? target.name : target.id
    const replacement = '@' + name + ' '
    const suffix = draft.slice(mention.end)
    const next = draft.slice(0, mention.start) + replacement + (suffix.startsWith(' ') ? suffix.slice(1) : suffix)
    if (next.length > input.maxLength) return
    const caret = mention.start + replacement.length
    mentionCaret.current = caret
    setDraft(next)
    setMentionCursor(caret)
    setMentionIndex(0)
    dismissMention(true)
  }

  function loadOlder() {
    const node = scroller.current
    if (!node || history.isFetchingNextPage) return
    olderPosition.current = { height: node.scrollHeight, top: node.scrollTop }
    pinned.current = false
    void history.fetchNextPage()
  }

  return (
    <>
      <header className="chat-header">
        <button className="icon-button mobile-back" type="button" aria-label="返回设备列表" onClick={back}><ArrowLeft size={21} /></button>
        <DeviceIcon device={device} />
        <div className="chat-title">
          <div><h1>{device.name}</h1>{device.is_demo && <span className="demo-label">模拟设备</span>}</div>
          {isFreeChat ? <p className="chat-model" title={device.os_version}>直连大模型 · {device.os_version}</p>
            : <p><Connection state={device.conn_state} /><span className="title-divider" />{desktops[device.desktop_state]}</p>}
        </div>
        <div className="chat-header-actions">
          {device.task_state === 'running' && <span className="active-task"><LoaderCircle size={14} className="spin" /> {isFreeChat ? '思考中' : '执行中'}</span>}
          {isWindowsDevice && (['cmd', 'powershell'] as const).map(shell => (
            <button
              key={shell}
              type="button"
              className="terminal-launch-button"
              disabled={!terminalOnline}
              title={terminalOnline ? `打开远程 ${terminalShellNames[shell]} 终端` : `设备离线，无法打开 ${terminalShellNames[shell]} 终端`}
              onClick={() => setActiveTerminal(shell)}
            >
              <Terminal size={13} aria-hidden="true" />
              <span>{terminalShellNames[shell]}</span>
            </button>
          ))}
          {isLinuxDevice && (
            <button
              type="button"
              className="terminal-launch-button"
              disabled={!terminalOnline}
              title={terminalOnline ? '打开远程 Shell 终端' : '设备离线，无法打开 Shell 终端'}
              onClick={() => setActiveTerminal('sh')}
            >
              <Terminal size={13} aria-hidden="true" />
              <span>Shell</span>
            </button>
          )}
          {isAgentAICapable && (
            <button
              type="button"
              className={'policy-toggle' + (device.auto_approve ? ' is-auto' : '')}
              disabled={policy.isPending}
              title={device.auto_approve
                ? '已开启免确认：Agent 所有操作默认直接放行，点击可切换为需确认'
                : '当前敏感操作需手动确认，点击可开启免确认全自动执行'}
              onClick={() => policy.mutate(!device.auto_approve)}
            >
              {device.auto_approve ? <Zap size={13} /> : <Shield size={13} />}
              <span>{device.auto_approve ? '免确认' : '需确认'}</span>
            </button>
          )}
          {!isFreeChat && <Battery device={device} />}
          <button
            type="button"
            className="header-clear-button"
            title={isFreeChat ? '清空闲聊记录和图片，停止当前回复' : '清空当前设备的所有聊天记录并删除对应大模型会话'}
            aria-label="清空记录"
            disabled={clear.isPending}
            onClick={() => { clear.reset(); setConfirmClear(true) }}
          >
            <Trash2 size={14} />
            <span>清空记录</span>
          </button>
          {!isFreeChat && <button type="button" className="icon-button" title="设备详情" aria-label="设备详情" onClick={() => setDetails(true)}><Info size={19} /></button>}
        </div>
      </header>
      <div
        className="conversation-scroll"
        ref={scroller}
        onScroll={event => {
          const node = event.currentTarget
          pinned.current = node.scrollHeight - node.scrollTop - node.clientHeight < 56
        }}
      >
        <div className="message-list" aria-label="对话记录">
          {history.hasNextPage && (
            <button type="button" className="secondary-button older-button" onClick={loadOlder} disabled={history.isFetchingNextPage}>
              <ArrowUp size={15} />{history.isFetchingNextPage ? '加载中' : '加载更早消息'}
            </button>
          )}
          {history.isPending && <p className="message-notice" role="status"><LoaderCircle size={16} className="spin" /> 正在加载对话</p>}
          {history.isError && <p className="message-notice" role="alert">{errorText(history.error)} <button className="text-button" onClick={() => void history.refetch()}>重试</button></p>}
          {history.isSuccess && messages.length === 0 && <p className="message-notice">还没有消息。</p>}
          {messages.map(message => <MessageItem key={message.id} message={message} device={device} session={session} mentionTokens={mentionTokens} />)}
        </div>
      </div>
      <form className="composer" aria-label="发送消息" onSubmit={submitForm}>
        <div className="composer-inner">
          {availableCommands.length > 0 && (
            <div className="quick-commands" aria-label="快捷指令">
              {availableCommands.map(command => {
                const Icon = commandIcons[command.capability]
                return (
                  <button type="button" className="quick-command" key={command.slash} onClick={() => submit(command.slash)} disabled={send.isPending || preparing}>
                    <Icon size={15} />{command.title}
                  </button>
                )
              })}
            </div>
          )}
          {(imageError || send.isError) && <p className="inline-error" role="alert">{imageError || errorText(send.error)}</p>}
          <div
            className={'composer-field' + (dragging ? ' is-dragging' : '')}
            onDragOver={dragImages}
            onDragLeave={event => {
              if (!(event.relatedTarget instanceof Node) || !event.currentTarget.contains(event.relatedTarget)) setDragging(false)
            }}
            onDrop={dropImages}
          >
            {activeMention && (
              <div className="mention-menu">
                <div className="mention-heading">引用设备</div>
                <div id={mentionListID} className="mention-options" role="listbox" aria-label="引用设备">
                  {mentionOptions.map((target, index) => (
                    <button
                      key={target.id} id={`${mentionListID}-${index}`} type="button" role="option" tabIndex={-1}
                      ref={index === activeMentionIndex ? activeMentionRef : undefined}
                      aria-selected={index === activeMentionIndex}
                      onPointerDown={event => event.preventDefault()}
                      onClick={() => completeMention(target)}
                    >
                      <DeviceIcon device={target} />
                      <span className="mention-device">
                        <strong title={target.name}>{target.name}</strong>
                        {(target.group || mentionTokens.get(target.name)?.id !== target.id) &&
                          <small>{[target.group, mentionTokens.get(target.name)?.id !== target.id && target.id].filter(Boolean).join(' · ')}</small>}
                      </span>
                      <Connection state={target.conn_state} />
                    </button>
                  ))}
                </div>
              </div>
            )}
            {suggestions.length > 0 && (
              <ul className="slash-menu" aria-label="可用指令">
                {suggestions.map(command => (
                  <li key={command.slash}>
                    <button type="button" onClick={() => submit(command.slash)} disabled={send.isPending || preparing}>
                      <Terminal size={16} /><span>{command.title}</span><code>{command.slash}</code>
                    </button>
                  </li>
                ))}
              </ul>
            )}
            {pendingImages.length > 0 && (
              <div className="composer-attachments" aria-label="待发送图片">
                {pendingImages.map(image => (
                  <div className="composer-thumb" key={image.id}>
                    <button type="button" className="composer-thumb-preview" onClick={() => setImagePreview(image)} aria-label={'预览 ' + image.file.name}>
                      <img src={image.preview} alt={image.file.name} />
                    </button>
                    <button
                      type="button" className="composer-thumb-remove" disabled={send.isPending} aria-label={'移除 ' + image.file.name}
                      onClick={() => { setPendingImages(current => current.filter(item => item !== image)); setImageError('') }}
                    ><X size={13} /></button>
                  </div>
                ))}
              </div>
            )}
            <div className="composer-controls">
              <textarea
                ref={composerRef}
                aria-label="消息内容" rows={2} maxLength={2000} value={draft}
                aria-controls={activeMention ? mentionListID : undefined}
                aria-activedescendant={activeMention ? `${mentionListID}-${activeMentionIndex}` : undefined}
                placeholder={isFreeChat ? '输入消息，键入 @ 引用设备' : '描述设备任务，键入 @ 引用设备或 / 查看指令'}
                onChange={event => { setDraft(event.target.value); dismissSlash(false); updateMention(event.currentTarget, true); setImageError('') }}
                onSelect={event => updateMention(event.currentTarget)}
                onFocus={event => updateMention(event.currentTarget)}
                onBlur={() => setMentionCursor(null)}
                onKeyDown={composerKey}
                onPaste={pasteImages}
              />
              <div className="composer-toolbar">
                <input
                  ref={fileInputRef} type="file" accept={acceptedChatImages} multiple hidden
                  onChange={event => {
                    const files = Array.from(event.currentTarget.files ?? [])
                    event.currentTarget.value = ''
                    void addImages(files)
                  }}
                />
                <button
                  type="button" className="icon-button attach-button" disabled={send.isPending || clear.isPending || preparing}
                  title="添加图片（也支持直接 Ctrl+V 粘贴）" aria-label="添加图片" onClick={() => fileInputRef.current?.click()}
                >
                  {preparing ? <LoaderCircle size={17} className="spin" /> : <Plus size={18} />}
                </button>
                <button
                  type="submit" className="primary-button send-button" aria-label="发送消息"
                  disabled={(!draft.trim() && pendingImages.length === 0) || send.isPending || clear.isPending || chatBusy || preparing}
                >
                  {send.isPending ? <LoaderCircle size={16} className="spin" /> : <Send size={16} />}
                </button>
              </div>
            </div>
          </div>
          <p className="composer-hint">Enter 发送 · Shift + Enter 换行</p>
        </div>
      </form>
      {imagePreview && <ImagePreview src={imagePreview.preview} name={imagePreview.file.name} close={() => setImagePreview(null)} />}
      {confirmClear && (
        <Panel
          title="清空聊天记录"
          className="confirm-panel"
          close={() => { if (!clear.isPending) setConfirmClear(false) }}
        >
          <div className="confirm-panel-body">
            <p>确定要清空「{device.name}」的所有聊天记录吗？</p>
            <p className="confirm-panel-note">{isFreeChat
              ? '图片附件会一并删除，进行中的回复会停止。清空后无法恢复。'
              : '对应的大模型会话也会被一并删除（进行中的 AI 任务会立即停止），此操作无法恢复。'}</p>
            {clear.isError && <p className="inline-error" role="alert"><CircleAlert size={15} />{errorText(clear.error)}</p>}
            <div className="confirm-panel-actions">
              <button type="button" className="secondary-button" disabled={clear.isPending} onClick={() => setConfirmClear(false)}>取消</button>
              <button type="button" className="danger-button" disabled={clear.isPending || (isFreeChat && (send.isPending || preparing))} onClick={() => clear.mutate()}>
                {clear.isPending ? <><LoaderCircle size={15} className="spin" /> 正在清空…</> : <><Trash2 size={15} /> 确认清空</>}
              </button>
            </div>
          </div>
        </Panel>
      )}
      {!isFreeChat && details && <Details
        key={device.id}
        device={device}
        csrf={session.csrf_token}
        close={() => { if (!removeDevice.isPending) { removeDevice.reset(); setDetails(false) } }}
        onToggleAutoApprove={!device.is_demo && (device.platform === 'windows' || device.platform === 'linux') ? enabled => policy.mutate(enabled) : undefined}
        policyPending={policy.isPending}
        onDelete={() => removeDevice.mutate()}
        deletePending={removeDevice.isPending}
        deleteError={removeDevice.isError ? errorText(removeDevice.error) : undefined}
      >
        {!device.is_demo && device.platform === 'windows' && <BrowserExtensionSection device={device} session={session} />}
        {!device.is_demo && device.platform === 'windows' && <RDPSection device={device} session={session} />}
        {!device.is_demo && device.ssh_managed && <SSHSection device={device} devices={devices} session={session} />}
        {!device.is_demo && !device.ssh_managed && <DeviceOperations device={device} session={session} />}
      </Details>}
      {activeTerminal && <TerminalModal
        deviceID={device.id}
        deviceName={device.name}
        shell={activeTerminal}
        onClose={() => setActiveTerminal(null)}
      />}
    </>
  )
}

function CopyMessageButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1600)
    } catch {
      // Ignore clipboard permission errors in restricted contexts.
    }
  }
  return (
    <button type="button" className="copy-message-button" onClick={() => void copy()} aria-label="复制消息内容" title="复制 Markdown 原文">
      {copied ? <><Check size={12} /> 已复制</> : <><Copy size={12} /> 复制</>}
    </button>
  )
}

function MessageItem({ message, device, session, mentionTokens }: {
  message: Message; device: Device; session: Session; mentionTokens: ReadonlyMap<string, Device | null>
}) {
  const time = timeFormat.format(message.created_at)
  if (message.type === 'system') {
    return <p className="system-message"><time>{time}</time><span>{message.text}</span></p>
  }
  if (message.type === 'task_card') return <TaskMessage card={message.payload} device={device} session={session} />
  if (message.type === 'status_card') return <StatusMessage card={message.payload} />
  if (message.type === 'media') return <ScreenshotMessage card={message.payload} />
  if (message.type === 'confirm_card') return <ConfirmationMessage card={message.payload} device={device} session={session} />
  const text = message.sender === 'user' ? renderDeviceMentions(message.text, mentionTokens, device.id) : message.text
  if (message.sender === 'user' && message.payload) {
    return <UserImageMessage text={text} time={time} images={message.payload.attachments} />
  }
  if (message.sender === 'agent') {
    return (
      <div className="text-message agent">
        <div className="markdown-body">
          <MarkdownContent text={message.text} />
        </div>
        <div className="message-meta">
          <time>{time}</time>
          <CopyMessageButton text={message.text} />
        </div>
      </div>
    )
  }
  return (
    <div className={'text-message ' + message.sender}>
      <p className="plain-text">{text}</p><time>{time}</time>
    </div>
  )
}

function UserImageMessage({ text, time, images }: { text: ReactNode; time: string; images: MediaCard[] }) {
  const [previewID, setPreviewID] = useState<string | null>(null)
  const selected = images.find(image => image.id === previewID)
  const full = selected ? api.attachment(selected.id, 'content') : ''
  return (
    <div className="text-message user">
      <div className="user-media-bubble">
        {text && <p>{text}</p>}
        {images.length > 0 ? (
          <div className="user-message-images" aria-label="消息图片">
            {images.map(image => (
              <button type="button" key={image.id} aria-label={'预览 ' + image.name} onClick={() => setPreviewID(image.id)}>
                <img src={api.attachment(image.id, 'thumb')} alt={image.name} loading="lazy" />
              </button>
            ))}
          </div>
        ) : !text && <span className="expired-image">图片已过期</span>}
      </div>
      <time>{time}</time>
      {selected && <ImagePreview src={full} name={selected.name} download={full + '?download=1'} close={() => setPreviewID(null)} />}
    </div>
  )
}

function TaskMessage({ card, device, session }: { card: TaskCard; device: Device; session: Session }) {
  const client = useQueryClient()
  const control = useMutation({
    mutationFn: (action: TaskAction) => api.taskControl(device.id, card.id, action, session.csrf_token),
    onSettled: () => {
      void client.invalidateQueries({ queryKey: messagesKey(device.id) })
      void client.invalidateQueries({ queryKey: devicesKey })
    },
  })
  const [now, setNow] = useState(Date.now)
  const ticking = ['running', 'paused', 'waiting_user', 'cancelling'].includes(card.state)
  useEffect(() => {
    if (!ticking) return
    const timer = window.setInterval(() => setNow(Date.now()), 500)
    return () => window.clearInterval(timer)
  }, [ticking])
  const controllable = !device.is_demo && card.source !== 'chat'
  const awaitingForce = controllable && card.lane === 'ui' && card.control !== 'force' && ticking &&
    (card.stalled || (card.state === 'cancelling' && card.cancel_at !== null && now >= card.cancel_at + 5000))
  const canCancel = controllable && ['queued', 'dispatched', 'running', 'paused', 'waiting_user'].includes(card.state)
  const offline = device.conn_state === 'offline' || device.conn_state === 'sleeping'
  let queueText = '等待执行'
  if (offline) {
    queueText = '设备离线 · ' + timeFormat.format(card.expires_at) + ' 到期'
  } else if (!device.is_demo && !device.task_protocol) {
    queueText = '等待设备接入聊天任务'
  }
  const remoteSteps = card.timeline.filter(event => event.step_key === 'remote_tool')
  const runningRemoteSteps = remoteSteps.filter(event => event.status === 'running')
  const showLocalSteps = remoteSteps.length === 0 || card.timeline.some(event => event.attempt > 0)
  const stepIndex = (showLocalSteps ? card.step_index : 0) + remoteSteps.length - runningRemoteSteps.length
  const stepTotal = (showLocalSteps ? card.step_total : 0) + remoteSteps.length
  const remoteLabel = runningRemoteSteps.map(event => event.step_name).join(' · ')
  const subtitle = card.state === 'queued' ? queueText : remoteLabel ||
    (showLocalSteps ? card.step_name : remoteSteps.at(-1)?.step_name)
  const elapsed = card.started_at === null ? null : Math.max(card.elapsed_ms, (card.finished_at ?? now) - card.started_at) / 1000
  const pendingText = { pause: '正在暂停', resume: '正在继续', cancel: '终止中', force: '强制终止中' }
  const error = card.error_code && card.state === 'failed' ? taskErrors[card.error_code] ?? card.error_code : null
  return (
    <article className="message-card task-card" aria-label={card.title}>
      <header className="card-header">
        <span className="card-icon"><Activity size={18} /></span>
        <div><h2>{card.title}</h2>{device.is_demo && <p>模拟任务</p>}</div>
        <span className={'task-state ' + card.state} role="status">
          {card.state === 'succeeded' && <Check size={13} />}{card.control ? pendingText[card.control] : stateText[card.state]}
        </span>
      </header>
      <div className="task-progress">
        <progress aria-label="任务进度" max={stepTotal} value={stepIndex} />
        <div>
          <span>{stepIndex}/{stepTotal}{subtitle && ' · ' + subtitle}</span>
          {elapsed !== null && <span>{elapsed.toFixed(1)} s</span>}
        </div>
      </div>
      <footer className="task-actions">
        <details open={remoteSteps.length > 0 || undefined}>
          <summary>步骤记录 <ChevronDown size={14} /></summary>
          <ol className="timeline">
            {card.timeline.map(event => (
              <li key={event.seq}>
                <span>{[event.step_name, event.detail !== event.step_name && event.detail,
                  event.step_name && stepText[event.status]].filter(Boolean).join(' · ')}</span>
                <time>{(event.attempt > 0 || event.step_key === 'remote_tool') && event.status !== 'running'
                  ? (event.elapsed_ms / 1000).toFixed(2) + ' s' : timeFormat.format(event.created_at)}</time>
              </li>
            ))}
          </ol>
        </details>
        <div className="task-controls">
          {controllable && !card.control && card.state === 'running' &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('pause')}>暂停</button>}
          {controllable && !card.control && card.state === 'paused' &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('resume')}>继续</button>}
          {(canCancel || (device.is_demo && card.state === 'queued')) &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('cancel')}>{card.state === 'queued' ? '撤回' : '终止'}</button>}
          {awaitingForce &&
            <button className="text-button task-force" type="button" disabled={control.isPending} onClick={() => control.mutate('force')}>强制终止</button>}
          {card.source !== 'mcp' && card.source !== 'chat' && (card.state === 'failed' || card.state === 'expired') &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('retry')}>重试</button>}
        </div>
      </footer>
      {card.stalled && <p className="inline-error" role="status"><CircleAlert size={15} />疑似卡住，暂未收到任务心跳。</p>}
      {(control.isError || error) && <p className="inline-error" role="alert"><CircleAlert size={15} />{control.isError ? errorText(control.error) : error}</p>}
    </article>
  )
}

function StatusMessage({ card }: { card: StatusCard }) {
  return (
    <article className="message-card status-card">
      <header className="card-header">
        <span className="card-icon status-icon">{card.battery_pct === null ? <Info size={18} /> : <BatteryCharging size={18} />}</span>
        <div><h2>{card.title}</h2></div>
      </header>
      <dl>
        <div><dt>电量</dt><dd>{card.battery_pct === null ? '不适用' : card.battery_pct + '%'}</dd></div>
        {card.charging !== null && <div><dt>充电状态</dt><dd>{card.charging ? '充电中' : '未充电'}</dd></div>}
        <div><dt>系统</dt><dd>{card.os_version} · {card.arch}</dd></div>
        <div><dt>桌面</dt><dd>{desktops[card.desktop_state]}</dd></div>
      </dl>
    </article>
  )
}

function ScreenshotMessage({ card }: { card: MediaCard }) {
  const [preview, setPreview] = useState(false)
  const full = api.attachment(card.id, 'content')
  return (
    <article className="message-card media-card">
      <button type="button" className="media-thumbnail" aria-label="预览截图" onClick={() => setPreview(true)}>
        <img src={api.attachment(card.id, 'thumb')} alt="主显示器截图" loading="lazy" />
      </button>
      <footer>
        <span>{card.device_name ? `[${card.device_name}] ` : ''}截图 · {Math.max(1, Math.round(card.size / 1024))} KB</span>
        <a className="text-button" href={full + '?download=1'} download={card.name}><Download size={14} />下载</a>
      </footer>
      {preview && <ImagePreview src={full} name={card.name} title="截图预览" download={full + '?download=1'} close={() => setPreview(false)} />}
    </article>
  )
}

function ImagePreview({ src, name, title = '图片预览', download, close }: {
  src: string
  name: string
  title?: string
  download?: string
  close: () => void
}) {
  const [zoomed, setZoomed] = useState(false)
  return (
    <Panel title={title} className="media-preview" close={close}>
      <div className={'media-preview-image' + (zoomed ? ' zoomed' : '')}>
        <img src={src} alt={name} />
      </div>
      <footer>
        <button type="button" className="text-button" onClick={() => setZoomed(value => !value)}>{zoomed ? '适应窗口' : '放大'}</button>
        {download && <a className="text-button" href={download} download={name}><Download size={14} />下载原图</a>}
      </footer>
    </Panel>
  )
}

function ConfirmationMessage({ card, device, session }: { card: ConfirmCard; device: Device; session: Session }) {
  const client = useQueryClient()
  const decision = useMutation({
    mutationFn: (approve: boolean) => api.confirm(device.id, card.id, approve, session.csrf_token),
    onSettled: () => {
      void client.invalidateQueries({ queryKey: messagesKey(device.id) })
      void client.invalidateQueries({ queryKey: devicesKey })
    },
  })
  const approveAll = useMutation({
    mutationFn: () => api.setDevicePolicy(device.id, true, session.csrf_token),
    onSettled: () => {
      void client.invalidateQueries({ queryKey: messagesKey(device.id) })
      void client.invalidateQueries({ queryKey: devicesKey })
    },
  })
  const busy = decision.isPending || approveAll.isPending
  const status = { pending: '待确认', approved: '已确认', denied: '已拒绝', cancelled: '已关闭' }[card.decision]
  const title = card.device_name ? `[${card.device_name}] 请审核这一次操作` : '操作确认'
  return (
    <article className="message-card confirmation-card" aria-label={title}>
      <header className="card-header">
        <span className="card-icon"><CircleAlert size={18} /></span>
        <div><h2>{title}</h2></div>
        <span className={'task-state ' + (card.decision === 'pending' ? 'waiting_user' : '')} role="status">{status}</span>
      </header>
      <p>{card.summary}</p>
      {card.attachment_id && (
        <a className="confirmation-image" href={api.attachment(card.attachment_id, 'content')} target="_blank" rel="noreferrer">
          <img src={api.attachment(card.attachment_id, 'thumb')} alt="待确认操作的截图" loading="lazy" />
        </a>
      )}
      {card.decision === 'pending' && (
        <footer>
          <button type="button" className="secondary-button" disabled={busy} onClick={() => decision.mutate(false)}>拒绝</button>
          {!card.device_name && device.id !== FREE_CHAT_ID &&
            <button type="button" className="secondary-button" disabled={busy} onClick={() => approveAll.mutate()}>确认并开启本机免确认</button>}
          <button type="button" className="primary-button" disabled={busy} onClick={() => decision.mutate(true)}>确认执行</button>
        </footer>
      )}
      {(decision.isError || approveAll.isError) && (
        <p className="inline-error" role="alert">{errorText(decision.error ?? approveAll.error)}</p>
      )}
    </article>
  )
}
