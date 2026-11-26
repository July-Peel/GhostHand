import { post, request } from './api'

export type Pairing = { server: string; key: string; fingerprint: string; managed_by_env: boolean; qr_png: string }
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

export type Release = {
  id: string
  version: string
  platform: 'windows'
  arch: 'amd64'
  component: 'core' | 'helper_win'
  size: number
  sha256: string
}
export type Rollout = { device_id: string } | { group: string } | { all: true }

const devicePath = (id: string) => '/api/devices/' + encodeURIComponent(id)
export const connectionKey = (id: string) => ['connection', id] as const
export const browserExtensionKey = (id: string) => ['browser-extension', id] as const
export const rdpKey = (id: string) => ['rdp', id] as const
export const releasesKey = ['releases'] as const

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
  releases: (signal: AbortSignal) => request<Release[]>('/api/releases', { signal }),
  upload: async (version: string, component: Release['component'], file: File, csrf: string) => {
    if (file.size > 96 * 1024 * 1024) throw new Error('版本包不能超过 96 MiB。')
    const digest = await crypto.subtle.digest('SHA-256', await file.arrayBuffer())
    const sha256 = [...new Uint8Array(digest)].map(byte => byte.toString(16).padStart(2, '0')).join('')
    const query = new URLSearchParams({ version, component, sha256 })
    return request<Release>('/api/releases?' + query.toString(), {
      method: 'POST',
      headers: { 'Content-Type': 'application/zip', 'X-CSRF-Token': csrf },
      body: file,
    })
  },
  rollout: (id: string, scope: Rollout, csrf: string) =>
    request<{ count: number }>('/api/releases/' + encodeURIComponent(id) + '/rollout', post(scope, csrf)),
}
