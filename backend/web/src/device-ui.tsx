import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { BatteryCharging, LoaderCircle, Monitor, Server, Smartphone, Trash2, X } from 'lucide-react'
import type { Device } from './api'

export const connections = { online: '在线', unstable: '连接不稳', sleeping: '休眠中', offline: '离线' }
export const desktops = { unlocked: '桌面可用', locked: '已锁屏', screen_off: '屏幕关闭', no_session: '未登录', headless: '无界面' }
const helpers = { ready: '就绪', restarting: '恢复中', unavailable: '不可用', none: '无需执行器' }
const dateFormat = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' })

export function DeviceIcon({ device }: { device: Device }) {
  const Icon = { windows: Monitor, android: Smartphone, linux: Server }[device.platform]
  return <span className={'device-icon ' + device.platform}><Icon size={19} aria-hidden="true" /></span>
}

export function Connection({ state }: { state: Device['conn_state'] }) {
  return <span className={'connection ' + state}><span aria-hidden="true" />{connections[state]}</span>
}

export function Battery({ device }: { device: Pick<Device, 'battery_pct' | 'charging'> }) {
  if (device.battery_pct === null) return null
  return (
    <span className="battery" title={device.charging ? '充电中' : '当前电量'}>
      {device.charging && <BatteryCharging size={14} aria-label="充电中" />}
      {device.battery_pct}%
    </span>
  )
}

export function Panel({ title, className, close, children }: {
  title: string
  className: string
  close: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    if (ref.current && !ref.current.open) ref.current.showModal()
  }, [])
  return (
    <dialog
      ref={ref}
      className={className}
      aria-label={title}
      onClose={close}
      onClick={event => { if (event.currentTarget === event.target) close() }}
    >
      <div className="panel-body">
        <header className="panel-header">
          <h2>{title}</h2>
          <button type="button" className="icon-button" onClick={close} aria-label="关闭"><X size={19} /></button>
        </header>
        {children}
      </div>
    </dialog>
  )
}

export function Details({ device, close, onToggleAutoApprove, policyPending, onDelete, deletePending, deleteError, children }: {
  device: Device
  close: () => void
  onToggleAutoApprove?: (enabled: boolean) => void
  policyPending?: boolean
  onDelete?: () => void
  deletePending?: boolean
  deleteError?: string
  children?: ReactNode
}) {
  const [confirmDelete, setConfirmDelete] = useState(false)
  const canDelete = device.conn_state !== 'online'

  return (
    <Panel title="设备详情" className="details-panel" close={close}>
      <div className="detail-device">
        <DeviceIcon device={device} />
        <div><strong>{device.name}</strong><p>{device.os_version}</p></div>
      </div>
      <dl className="details-list">
        <div><dt>连接</dt><dd><Connection state={device.conn_state} /></dd></div>
        <div><dt>电量</dt><dd><Battery device={device} />{device.battery_pct === null && '不适用'}</dd></div>
        <div><dt>桌面</dt><dd>{desktops[device.desktop_state]}</dd></div>
        <div><dt>执行器</dt><dd>{helpers[device.helper_state]}</dd></div>
        <div><dt>架构</dt><dd>{device.arch}</dd></div>
        <div><dt>分组</dt><dd>{device.group}</dd></div>
        <div><dt>最后在线</dt><dd>{dateFormat.format(device.last_seen_at)}</dd></div>
        {onToggleAutoApprove && (
          <div>
            <dt>执行策略</dt>
            <dd>
              <button
                type="button"
                className={'policy-toggle' + (device.auto_approve ? ' is-auto' : '')}
                disabled={policyPending}
                onClick={() => onToggleAutoApprove(!device.auto_approve)}
              >
                {device.auto_approve ? '免确认全自动执行' : '敏感操作需确认'}
              </button>
            </dd>
          </div>
        )}
        {device.is_demo && <div><dt>设备来源</dt><dd>模拟设备</dd></div>}
      </dl>
      {children}
      {onDelete && (
        <section className="agent-section">
          <h3>设备管理</h3>
          {!canDelete && (
            <p>设备当前在线，请先断开连接或吊销凭证后再删除。</p>
          )}
          {confirmDelete ? (
            <div className="agent-confirm">
              <p>确定要永久删除「{device.name}」吗？该设备的所有聊天记录、任务历史、连接记录与本地附件将被彻底清除，无法恢复。</p>
              {deleteError && <p role="alert">{deleteError}</p>}
              <div className="agent-actions">
                <button type="button" className="danger-button" disabled={deletePending || !canDelete} onClick={onDelete}>
                  {deletePending ? <><LoaderCircle size={14} className="spin" /> 正在删除…</> : <><Trash2 size={14} /> 确认删除</>}
                </button>
                <button type="button" className="text-button" disabled={deletePending} onClick={() => setConfirmDelete(false)}>取消</button>
              </div>
            </div>
          ) : (
            <button
              type="button"
              className="danger-outline-button"
              disabled={!canDelete || deletePending}
              title={canDelete ? '彻底删除此设备及其所有数据' : '仅可删除已断线的设备'}
              onClick={() => setConfirmDelete(true)}
            >
              <Trash2 size={14} />
              <span>删除设备</span>
            </button>
          )}
        </section>
      )}
    </Panel>
  )
}
