import { post, request, requestError } from './api'

export type Pairing = {
  server: string; key: string; fingerprint: string; managed_by_env: boolean; qr_png: string
  windows_install_command: string; linux_install_command: string
}
export const windowsInstallerNames = ['GhostHand.Helper.exe', 'ghosthand-core.exe', 'ghosthand-launcher.exe'] as const
export type WindowsInstallerName = typeof windowsInstallerNames[number]
export type WindowsInstallerFile = { name: WindowsInstallerName; size: number }
export const linuxInstallerNames = ['ghosthand-core'] as const
export type LinuxInstallerName = typeof linuxInstallerNames[number]
export type LinuxInstallerFile = { name: LinuxInstallerName; size: number }
export type SSHAuthKind = 'password' | 'private_key'
export type SSHCreateInput = {
  name: string
  group: string
  host: string
  port: number
  username: string
  auth_kind: SSHAuthKind
  secret: string
  host_agent_id: string
}
export type SSHCreateResult = {
  device_id: string
  name: string
  conn_state: 'online' | 'unstable' | 'sleeping' | 'offline'
  os_version: string
  arch: string
  last_error?: string
}
export type SSHConfigStatus = {
  configured: boolean
  host: string
  port: number
  username: string
  auth_kind: SSHAuthKind
  host_agent_id: string
  host_agent_name?: string
  host_key_fp?: string
  enabled: boolean
  last_error?: string
}
export type SSHUpdateInput = {
  host: string
  port: number
  username: string
  auth_kind: SSHAuthKind
  secret: string
  host_agent_id: string
  enabled?: boolean
}
export type OperationKind = 'log.collect' | 'agent.restart' | 'helper.restart' | 'agent.update' | 'agent.rollback' | 'agent.uninstall'
export type ConnectionHistory = {
  version: string
  revoked: boolean
  can_uninstall: boolean
  sessions: {
    generation: number
    connected_at: number
    disconnected_at: number | null
    reason: string | null
    remote_ip: string
    agent_ver: string
  }[]
  operations: {
    id: string
    kind: OperationKind
    state: 'queued' | 'succeeded' | 'failed'
    detail: string
    created_at: number
    finished_at: number | null
    log_available: boolean
  }[]
}

const devicePath = (id: string) => '/api/devices/' + encodeURIComponent(id)
export const connectionKey = (id: string) => ['connection', id] as const
export const browserExtensionKey = (id: string) => ['browser-extension', id] as const
export const rdpKey = (id: string) => ['rdp', id] as const
export const sshKey = (id: string) => ['ssh', id] as const

export type BrowserExtensionStatus = {
  browser: 'chrome'
  extension_id: string
  version: string
  connected: boolean
  installed: boolean
  registered: boolean
  bundle_present: boolean
  error?: string
  message?: string
}

export type RDPAction = 'preflight' | 'unlock' | 'lock'
type RDPPreflight = {
  edition: string
  os_version: string
  rdp_supported: boolean
  rdp_enabled: boolean
  termservice_running: boolean
  nla_required: boolean
  port_reachable: boolean
  service_mode: boolean
  console_session: { id: number; state: 'locked' | 'unlocked' | 'none' | 'unknown'; username: string }
  sleep_ac_minutes: number | null
  sleep_dc_minutes: number | null
  firewall_rdp_inbound_open: boolean | null
  client_available: boolean
  certificate_available: boolean
  single_session: boolean
  blank_password_restricted?: boolean
}

export type RDPResult = {
  operation_id: string
  kind: `rdp.${RDPAction}`
  ok: boolean
  code?: string
}

export type RDPStatus = {
  configured: boolean
  username: string
  domain: string
  blank_password: boolean
  rdp_port: number
  auto_unlock: boolean
  auto_relock: boolean
  keep_system_awake: boolean
  suspended: boolean
  last_result: string
  last_unlock_at: number
  operation?: RDPResult['kind']
  stage?: string
  preflight?: RDPResult & { detail?: RDPPreflight }
}

type RDPConfig = { clear: true } | {
  username: string
  domain: string
  password: string
  blank_password: boolean
  rdp_port: number
  auto_unlock: boolean
  auto_relock: boolean
  keep_system_awake: boolean
}

export const agentAPI = {
  getPairing: (signal: AbortSignal) => request<Pairing>('/api/pairing', { signal }),
  rotatePairingKey: (csrf: string) => request<Pairing>('/api/pairing/rotate', post({}, csrf)),
  windowsInstallerFiles: (signal: AbortSignal) => request<WindowsInstallerFile[]>('/api/pairing/windows/files', { signal }),
  uploadWindowsInstallerFile: async (name: WindowsInstallerName, file: File, csrf: string, onProgress: (percent: number) => void) => {
    if (file.name.toLowerCase() !== name.toLowerCase()) throw new Error(`请选择 ${name}。`)
    if (file.size > 256 * 1024 * 1024) throw new Error('单个 EXE 不能超过 256 MiB。')
    const path = '/api/pairing/windows/files/' + encodeURIComponent(name)
    return new Promise<WindowsInstallerFile>((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.upload.onprogress = ({ loaded }) => {
        if (file.size > 0) onProgress(Math.min(100, Math.floor(loaded / file.size * 100)))
      }
      xhr.upload.onload = () => onProgress(100)
      xhr.onload = () => {
        const body = xhr.response as (Partial<WindowsInstallerFile> & { error?: string }) | null
        if (xhr.status >= 200 && xhr.status < 300 && body?.name === name && typeof body.size === 'number') {
          resolve({ name, size: body.size })
        }
        else reject(requestError(path, xhr.status, body))
      }
      xhr.onerror = () => reject(requestError(path, xhr.status, null))
      xhr.open('POST', path)
      xhr.responseType = 'json'
      xhr.setRequestHeader('Content-Type', 'application/octet-stream')
      xhr.setRequestHeader('X-CSRF-Token', csrf)
      xhr.send(file)
    })
  },
  linuxInstallerFiles: (signal: AbortSignal) => request<LinuxInstallerFile[]>('/api/pairing/linux/files', { signal }),
  uploadLinuxInstallerFile: async (name: LinuxInstallerName, file: File, csrf: string, onProgress: (percent: number) => void) => {
    if (file.size > 256 * 1024 * 1024) throw new Error('单个二进制文件不能超过 256 MiB。')
    const path = '/api/pairing/linux/files/' + encodeURIComponent(name)
    return new Promise<LinuxInstallerFile>((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.upload.onprogress = ({ loaded }) => {
        if (file.size > 0) onProgress(Math.min(100, Math.floor(loaded / file.size * 100)))
      }
      xhr.upload.onload = () => onProgress(100)
      xhr.onload = () => {
        const body = xhr.response as (Partial<LinuxInstallerFile> & { error?: string }) | null
        if (xhr.status >= 200 && xhr.status < 300 && body?.name === name && typeof body.size === 'number') {
          resolve({ name, size: body.size })
        }
        else reject(requestError(path, xhr.status, body))
      }
      xhr.onerror = () => reject(requestError(path, xhr.status, null))
      xhr.open('POST', path)
      xhr.responseType = 'json'
      xhr.setRequestHeader('Content-Type', 'application/octet-stream')
      xhr.setRequestHeader('X-CSRF-Token', csrf)
      xhr.send(file)
    })
  },
  createSSH: (input: SSHCreateInput, csrf: string) =>
    request<SSHCreateResult>('/api/agents/ssh', post(input, csrf)),
  sshStatus: (id: string, signal: AbortSignal) =>
    request<SSHConfigStatus>(devicePath(id) + '/ssh', { signal }),
  updateSSH: (id: string, input: SSHUpdateInput, csrf: string) =>
    request<SSHConfigStatus>(devicePath(id) + '/ssh/config', post(input, csrf)),
  reconnectSSH: (id: string, csrf: string) =>
    request<void>(devicePath(id) + '/ssh/reconnect', post({}, csrf)),
  disconnectSSH: (id: string, csrf: string) =>
    request<void>(devicePath(id) + '/ssh/disconnect', post({}, csrf)),
  history: (id: string, signal: AbortSignal) =>
    request<ConnectionHistory>(devicePath(id) + '/connection', { signal }),
  browserExtension: (id: string, signal: AbortSignal) =>
    request<BrowserExtensionStatus>(devicePath(id) + '/browser-extension?browser=chrome', { signal }),
  changeBrowserExtension: (id: string, action: 'install' | 'uninstall', csrf: string) =>
    request<BrowserExtensionStatus>(devicePath(id) + '/browser-extension', post({ action, browser: 'chrome' }, csrf)),
  rdpStatus: (id: string, signal: AbortSignal) =>
    request<RDPStatus>(devicePath(id) + '/rdp', { signal }),
  rdpConfig: (id: string, input: RDPConfig, csrf: string) =>
    request<void>(devicePath(id) + '/rdp/config', post(input, csrf)),
  rdpOperate: (id: string, action: RDPAction, confirm: boolean, csrf: string) =>
    request<RDPResult>(devicePath(id) + '/rdp/' + action, post({ confirm_suspended: confirm }, csrf)),
  operate: (id: string, kind: OperationKind, hours: number | undefined, csrf: string) =>
    request<{ id: string }>(devicePath(id) + '/operations', post({ kind, hours }, csrf)),
  revoke: (id: string, csrf: string) =>
    request<void>(devicePath(id) + '/revoke', post({}, csrf)),
  restore: (id: string, csrf: string) =>
    request<void>(devicePath(id) + '/restore', post({}, csrf)),
  deleteDevice: (id: string, csrf: string) =>
    request<void>(devicePath(id) + '/delete', post({}, csrf)),
  logURL: (id: string, op: string) => devicePath(id) + '/logs/' + encodeURIComponent(op),
}
