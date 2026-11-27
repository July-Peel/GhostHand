import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { FormEvent, KeyboardEvent } from 'react'
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Activity, ArrowLeft, ArrowUp, BatteryCharging, Camera, Check, ChevronDown, CircleAlert, Copy, Download, Info, LoaderCircle, Send, Shield, Terminal, Trash2, Zap } from 'lucide-react'
import { api, devicesKey, errorText, messagesKey } from './api'
import type { Command, ConfirmCard, Device, MediaCard, Message, Session, StatusCard, TaskAction, TaskCard } from './api'
import { Battery, Connection, Details, DeviceIcon, Panel, desktops } from './device-ui'
import { BrowserExtensionSection, DeviceOperations } from './agent-ui'
import { agentAPI, connectionKey } from './agent-api'
import { RDPSection } from './rdp-ui'
import { MarkdownContent } from './markdown'

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

export function Conversation({ device, commands, session, opened, back }: {
  device: Device
  commands: Command[]
  session: Session
  opened: boolean
  back: () => void
}) {
  const [details, setDetails] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)
  const [draft, setDraft] = useState('')
  const [slashDismissed, dismissSlash] = useState(false)
  const scroller = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)
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
  const availableCommands = commands.filter(command => {
    if (device.is_demo) return command.capability !== 'screen.capture'
    return device.platform === 'windows' || command.capability === 'device.info'
  })
  const prefix = draft.trim().toLowerCase()
  const suggestions = !slashDismissed && prefix.startsWith('/')
    ? availableCommands.filter(command => command.slash.startsWith(prefix))
    : []

  const send = useMutation({
    mutationFn: (text: string) => api.send(device.id, text, session.csrf_token),
    onSuccess: (_, text) => {
      setDraft(current => current.trim() === text ? '' : current)
      pinned.current = true
      void client.invalidateQueries({ queryKey: key })
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
    onSuccess: () => {
      setConfirmClear(false)
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
    const node = scroller.current
    if (!node) return
    if (olderPosition.current) {
      if (history.isFetchingNextPage) return
      node.scrollTop = olderPosition.current.top + node.scrollHeight - olderPosition.current.height
      olderPosition.current = null
    } else if (pinned.current) {
      node.scrollTop = node.scrollHeight
    }
  }, [history.data, history.isFetchingNextPage, opened])

  function submit(text: string) {
    const value = text.trim()
    if (!value || send.isPending) return
    dismissSlash(true)
    send.mutate(value)
  }

  function submitForm(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    submit(draft)
  }

  function composerKey(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === 'Escape') dismissSlash(true)
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing || event.keyCode === 229) return
    event.preventDefault()
    submit(suggestions.length === 1 ? suggestions[0].slash : draft)
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
          <p><Connection state={device.conn_state} /><span className="title-divider" />{desktops[device.desktop_state]}</p>
        </div>
        <div className="chat-header-actions">
          {device.task_state === 'running' && <span className="active-task"><LoaderCircle size={14} className="spin" /> 执行中</span>}
          {!device.is_demo && device.platform === 'windows' && (
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
          <Battery device={device} />
          <button
            type="button"
            className="header-clear-button"
            title="清空当前设备的所有聊天记录并删除对应大模型会话"
            aria-label="清空记录"
            disabled={clear.isPending}
            onClick={() => { clear.reset(); setConfirmClear(true) }}
          >
            <Trash2 size={14} />
            <span>清空记录</span>
          </button>
          <button type="button" className="icon-button" title="设备详情" aria-label="设备详情" onClick={() => setDetails(true)}><Info size={19} /></button>
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
          {messages.map(message => <MessageItem key={message.id} message={message} device={device} session={session} />)}
        </div>
      </div>
      <form className="composer" aria-label="发送消息" onSubmit={submitForm}>
        <div className="composer-inner">
          {availableCommands.length > 0 && (
            <div className="quick-commands" aria-label="快捷指令">
              {availableCommands.map(command => {
                const Icon = commandIcons[command.capability]
                return (
                  <button type="button" className="quick-command" key={command.slash} onClick={() => submit(command.slash)} disabled={send.isPending}>
                    <Icon size={15} />{command.title}
                  </button>
                )
              })}
            </div>
          )}
          {send.isError && <p className="inline-error" role="alert">{errorText(send.error)}</p>}
          <div className="composer-field">
            {suggestions.length > 0 && (
              <ul className="slash-menu" aria-label="可用指令">
                {suggestions.map(command => (
                  <li key={command.slash}>
                    <button type="button" onClick={() => submit(command.slash)} disabled={send.isPending}>
                      <Terminal size={16} /><span>{command.title}</span><code>{command.slash}</code>
                    </button>
                  </li>
                ))}
              </ul>
            )}
            <textarea
              ref={composerRef}
              aria-label="消息内容" rows={2} maxLength={2000} value={draft}
              placeholder="描述想在设备上完成的任务，或键入 / 查看指令"
              onChange={event => { setDraft(event.target.value); dismissSlash(false) }}
              onKeyDown={composerKey}
            />
            <button type="submit" className="primary-button send-button" aria-label="发送消息" disabled={!draft.trim() || send.isPending}>
              {send.isPending ? <LoaderCircle size={18} className="spin" /> : <Send size={18} />}
            </button>
          </div>
          <p className="composer-hint">Enter 发送 · Shift + Enter 换行</p>
        </div>
      </form>
      {confirmClear && (
        <Panel
          title="清空聊天记录"
          className="confirm-panel"
          close={() => { if (!clear.isPending) setConfirmClear(false) }}
        >
          <div className="confirm-panel-body">
            <p>确定要清空「{device.name}」的所有聊天记录吗？</p>
            <p className="confirm-panel-note">对应的大模型会话也会被一并删除（进行中的 AI 任务会立即停止），此操作无法恢复。</p>
            {clear.isError && <p className="inline-error" role="alert"><CircleAlert size={15} />{errorText(clear.error)}</p>}
            <div className="confirm-panel-actions">
              <button type="button" className="secondary-button" disabled={clear.isPending} onClick={() => setConfirmClear(false)}>取消</button>
              <button type="button" className="danger-button" disabled={clear.isPending} onClick={() => clear.mutate()}>
                {clear.isPending ? <><LoaderCircle size={15} className="spin" /> 正在清空…</> : <><Trash2 size={15} /> 确认清空</>}
              </button>
            </div>
          </div>
        </Panel>
      )}
      {details && <Details
        key={device.id}
        device={device}
        csrf={session.csrf_token}
        close={() => { if (!removeDevice.isPending) { removeDevice.reset(); setDetails(false) } }}
        onToggleAutoApprove={!device.is_demo && device.platform === 'windows' ? enabled => policy.mutate(enabled) : undefined}
        policyPending={policy.isPending}
        onDelete={() => removeDevice.mutate()}
        deletePending={removeDevice.isPending}
        deleteError={removeDevice.isError ? errorText(removeDevice.error) : undefined}
      >
        {!device.is_demo && device.platform === 'windows' && <BrowserExtensionSection device={device} session={session} />}
        {!device.is_demo && device.platform === 'windows' && <RDPSection device={device} session={session} />}
        {!device.is_demo && <DeviceOperations device={device} session={session} />}
      </Details>}
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

function MessageItem({ message, device, session }: { message: Message; device: Device; session: Session }) {
  const time = timeFormat.format(message.created_at)
  if (message.type === 'system') {
    return <p className="system-message"><time>{time}</time><span>{message.text}</span></p>
  }
  if (message.type === 'task_card') return <TaskMessage card={message.payload} device={device} session={session} />
  if (message.type === 'status_card') return <StatusMessage card={message.payload} />
  if (message.type === 'media') return <ScreenshotMessage card={message.payload} />
  if (message.type === 'confirm_card') return <ConfirmationMessage card={message.payload} device={device} session={session} />
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
      <p className="plain-text">{message.text}</p><time>{time}</time>
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
  const awaitingForce = !device.is_demo && card.lane === 'ui' && card.control !== 'force' && ticking &&
    (card.stalled || (card.state === 'cancelling' && card.cancel_at !== null && now >= card.cancel_at + 5000))
  const canCancel = !device.is_demo && ['dispatched', 'running', 'paused', 'waiting_user'].includes(card.state)
  const offline = device.conn_state === 'offline' || device.conn_state === 'sleeping'
  let queueText = '等待执行'
  if (offline) {
    queueText = '设备离线 · ' + timeFormat.format(card.expires_at) + ' 到期'
  } else if (!device.is_demo && !device.task_protocol) {
    queueText = '等待设备接入聊天任务'
  }
  const subtitle = card.state === 'queued'
    ? queueText
    : card.step_name
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
        <progress aria-label="任务进度" max={card.step_total} value={card.step_index} />
        <div>
          <span>{card.step_index}/{card.step_total}{subtitle && ' · ' + subtitle}</span>
          {elapsed !== null && <span>{elapsed.toFixed(1)} s</span>}
        </div>
      </div>
      <footer className="task-actions">
        <details>
          <summary>步骤记录 <ChevronDown size={14} /></summary>
          <ol className="timeline">
            {card.timeline.map(event => (
              <li key={event.seq}>
                <span>{[event.step_name, event.detail !== event.step_name && event.detail,
                  event.step_name && stepText[event.status]].filter(Boolean).join(' · ')}</span>
                <time>{event.attempt > 0 && event.status !== 'running' ? (event.elapsed_ms / 1000).toFixed(2) + ' s' : timeFormat.format(event.created_at)}</time>
              </li>
            ))}
          </ol>
        </details>
        <div className="task-controls">
          {!device.is_demo && !card.control && card.state === 'running' &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('pause')}>暂停</button>}
          {!device.is_demo && !card.control && card.state === 'paused' &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('resume')}>继续</button>}
          {(card.state === 'queued' || canCancel) &&
            <button className="text-button" type="button" disabled={control.isPending} onClick={() => control.mutate('cancel')}>{card.state === 'queued' ? '撤回' : '终止'}</button>}
          {awaitingForce &&
            <button className="text-button task-force" type="button" disabled={control.isPending} onClick={() => control.mutate('force')}>强制终止</button>}
          {card.source !== 'mcp' && (card.state === 'failed' || card.state === 'expired') &&
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
  const [zoomed, setZoomed] = useState(false)
  const full = api.attachment(card.id, 'content')
  return (
    <article className="message-card media-card">
      <button type="button" className="media-thumbnail" aria-label="预览截图" onClick={() => { setZoomed(false); setPreview(true) }}>
        <img src={api.attachment(card.id, 'thumb')} alt="主显示器截图" loading="lazy" />
      </button>
      <footer>
        <span>截图 · {Math.max(1, Math.round(card.size / 1024))} KB</span>
        <a className="text-button" href={full + '?download=1'} download={card.name}><Download size={14} />下载</a>
      </footer>
      {preview && (
        <Panel title="截图预览" className="media-preview" close={() => setPreview(false)}>
          <div className={'media-preview-image' + (zoomed ? ' zoomed' : '')}>
            <img src={full} alt="主显示器截图" />
          </div>
          <footer>
            <button type="button" className="text-button" onClick={() => setZoomed(value => !value)}>{zoomed ? '适应窗口' : '放大'}</button>
            <a className="text-button" href={full + '?download=1'} download={card.name}><Download size={14} />下载</a>
          </footer>
        </Panel>
      )}
    </article>
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
  return (
    <article className="message-card confirmation-card" aria-label="操作确认">
      <header className="card-header">
        <span className="card-icon"><CircleAlert size={18} /></span>
        <div><h2>操作确认</h2></div>
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
          <button type="button" className="secondary-button" disabled={busy} onClick={() => approveAll.mutate()}>确认并开启本机免确认</button>
          <button type="button" className="primary-button" disabled={busy} onClick={() => decision.mutate(true)}>确认执行</button>
        </footer>
      )}
      {(decision.isError || approveAll.isError) && (
        <p className="inline-error" role="alert">{errorText(decision.error ?? approveAll.error)}</p>
      )}
    </article>
  )
}
