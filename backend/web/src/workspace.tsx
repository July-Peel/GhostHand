import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Command } from 'cmdk'
import { ChevronRight, LoaderCircle, LogOut, Monitor, Plus, Search, Settings } from 'lucide-react'
import { create } from 'zustand'
import { api, devicesKey, errorText, messagesKey, sessionKey } from './api'
import type { Device, Session } from './api'
import { Conversation } from './conversation'
import { Battery, Connection, DeviceIcon, Panel } from './device-ui'
import { SettingsPanel } from './settings'
import { PairingPanel } from './agent-ui'
import { connectionKey, rdpKey } from './agent-api'

const useWorkspace = create<{
  selected: string | null
  select: (id: string | null) => void
}>(set => ({
  selected: null,
  select: selected => set({ selected }),
}))

function DeviceRow({ device, active, select }: { device: Device; active: boolean; select: () => void }) {
  return (
    <button
      className={'device-row' + (active ? ' is-active' : '')}
      type="button"
      aria-current={active ? 'true' : undefined}
      onClick={select}
    >
      <DeviceIcon device={device} />
      <span className="device-row-text">
        <strong>{device.name}</strong>
        <span>{device.os_version}</span>
      </span>
      <span className="device-row-state"><Connection state={device.conn_state} /><Battery device={device} /></span>
      <ChevronRight className="row-chevron" size={15} aria-hidden="true" />
    </button>
  )
}

function useEvents() {
  const client = useQueryClient()
  useEffect(() => {
    const stream = new EventSource('/api/events')
    const changedDevices = new Set<string>()
    let refreshAll = false
    let timer: ReturnType<typeof setTimeout> | undefined
    function scheduleRefresh() {
      if (timer !== undefined) return
      timer = setTimeout(() => {
        timer = undefined
        void client.invalidateQueries({ queryKey: devicesKey })
        if (refreshAll) {
          void client.invalidateQueries({ queryKey: ['messages'] })
          void client.invalidateQueries({ queryKey: ['connection'] })
          void client.invalidateQueries({ queryKey: ['rdp'] })
        } else {
          for (const id of changedDevices) {
            void client.invalidateQueries({ queryKey: messagesKey(id) })
            void client.invalidateQueries({ queryKey: connectionKey(id) })
            void client.invalidateQueries({ queryKey: rdpKey(id) })
          }
        }
        refreshAll = false
        changedDevices.clear()
      }, 60)
    }
    stream.onerror = () => {
      void client.invalidateQueries({ queryKey: sessionKey })
    }
    stream.addEventListener('refresh', () => {
      refreshAll = true
      scheduleRefresh()
    })
    stream.addEventListener('device.changed', event => {
      if (!(event instanceof MessageEvent)) return
      const { device_id: id } = JSON.parse(event.data) as { device_id: string }
      changedDevices.add(id)
      scheduleRefresh()
    })
    return () => {
      stream.close()
      clearTimeout(timer)
    }
  }, [client])
}

export function Workspace({ session, brand, themeButton, onPasswordChanged }: {
  session: Session
  brand: ReactNode
  themeButton: ReactNode
  onPasswordChanged: () => void
}) {
  const client = useQueryClient()
  const [filter, setFilter] = useState('')
  const [palette, setPalette] = useState(false)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [pairingOpen, setPairingOpen] = useState(false)
  const selected = useWorkspace(state => state.selected)
  const select = useWorkspace(state => state.select)
  const query = useQuery({ queryKey: devicesKey, queryFn: ({ signal }) => api.devices(signal) })
  const devices = query.data?.devices ?? []
  const chosen = devices.find(device => device.id === selected)
  const active = chosen ?? devices[0]
  const online = devices.filter(device => device.conn_state === 'online').length
  const needle = filter.trim().toLowerCase()
  const groups = new Map<string, Device[]>()
  for (const device of devices) {
    const group = device.group || '未分组'
    if (![device.name, device.os_version, group].some(text => text.toLowerCase().includes(needle))) continue
    const groupDevices = groups.get(group)
    if (groupDevices) groupDevices.push(device)
    else groups.set(group, [device])
  }

  const logout = useMutation({
    mutationFn: () => api.logout(session.csrf_token),
    onSuccess: () => {
      select(null)
      client.setQueryData(sessionKey, null)
      client.removeQueries({ predicate: query => query.queryKey[0] !== sessionKey[0] })
    },
  })

  useEvents()
  useEffect(() => {
    function openPalette(event: KeyboardEvent) {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
        if (settingsOpen || pairingOpen) return
        event.preventDefault()
        setPalette(value => !value)
      }
    }
    window.addEventListener('keydown', openPalette)
    return () => window.removeEventListener('keydown', openPalette)
  }, [settingsOpen, pairingOpen])

  return (
    <div className="workspace">
      <header className="topbar">
        <div className="topbar-brand">{brand}<span className="app-section">工作台</span></div>
        <div className="topbar-actions">
          {themeButton}
          <button type="button" className="icon-button" aria-label="接入设备" title="接入设备"
            onClick={() => setPairingOpen(true)}><Plus size={18} /></button>
          <button
            type="button" className="icon-button" aria-label="系统设置" title="系统设置"
            onClick={() => setSettingsOpen(true)}
          ><Settings size={18} /></button>
          <span className="user-name" title={session.username}>{session.username}</span>
          <button
            type="button" className="icon-button" aria-label="退出登录" title="退出登录"
            onClick={() => logout.mutate()} disabled={logout.isPending}
          ><LogOut size={17} /></button>
        </div>
      </header>
      {logout.isError && <div className="global-error" role="alert">{errorText(logout.error)}</div>}
      <div className={'workspace-grid' + (chosen ? ' show-chat' : '')}>
        <aside className="sidebar" aria-label="设备列表">
          <div className="sidebar-heading">
            <div className="sidebar-title">
              <h1>设备</h1>
              <span className="device-count">{devices.length}</span>
            </div>
            <p>{online} 台在线</p>
          </div>
          <div className="device-search">
            <Search size={16} aria-hidden="true" />
            <input aria-label="搜索设备" placeholder="搜索设备" value={filter} onChange={event => setFilter(event.target.value)} />
            <button type="button" title="快速切换设备" aria-label="快速切换设备" onClick={() => setPalette(true)}><kbd>Ctrl K</kbd></button>
          </div>
          <div className="device-groups">
            {query.isPending && <p role="status" className="sidebar-note"><LoaderCircle size={16} className="spin" /> 正在加载设备</p>}
            {query.isError && <p className="sidebar-note" role="alert">{errorText(query.error)} <button className="text-button" onClick={() => void query.refetch()}>重试</button></p>}
            {query.isSuccess && groups.size === 0 && <p className="sidebar-note">{devices.length ? '没有匹配的设备。' : '暂无设备。'}</p>}
            {[...groups].map(([group, items]) => (
              <section key={group} className="device-group">
                <h2>{group}</h2>
                {items.map(device => (
                  <DeviceRow key={device.id} device={device} active={active?.id === device.id} select={() => select(device.id)} />
                ))}
              </section>
            ))}
          </div>
        </aside>
        <main className="chat-pane">
          {active ? (
            <Conversation
              key={active.id}
              device={active}
              commands={query.data?.commands ?? []}
              session={session}
              opened={chosen !== undefined}
              back={() => select(null)}
            />
          ) : (
            <div className="centered-state">
              <Monitor size={27} />
              <p>{query.isPending ? '正在加载设备' : '暂无可用对话'}</p>
            </div>
          )}
        </main>
      </div>
      {pairingOpen && <PairingPanel session={session} close={() => setPairingOpen(false)} />}
      {settingsOpen && (
        <SettingsPanel session={session} close={() => setSettingsOpen(false)} onPasswordChanged={onPasswordChanged} />
      )}
      {palette && (
        <Panel title="切换设备" className="command-panel" close={() => setPalette(false)}>
          <Command label="搜索设备">
            <div className="command-search"><Search size={18} /><Command.Input placeholder="输入设备名称或分组…" autoFocus /></div>
            <Command.List>
              <Command.Empty>没有匹配的设备。</Command.Empty>
              {devices.map(device => (
                <Command.Item key={device.id} value={device.id} keywords={[device.name, device.group, device.os_version]}
                  onSelect={() => { select(device.id); setPalette(false) }}>
                  <DeviceIcon device={device} /><span>{device.name}</span><Connection state={device.conn_state} />
                </Command.Item>
              ))}
            </Command.List>
          </Command>
        </Panel>
      )}
    </div>
  )
}
