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
export const releasesKey = ['releases'] as const

export const agentAPI = {
  getPairing: (signal: AbortSignal) => request<Pairing>('/api/pairing', { signal }),
  rotatePairingKey: (csrf: string) => request<Pairing>('/api/pairing/rotate', post({}, csrf)),
  history: (id: string, signal: AbortSignal) =>
    request<ConnectionHistory>(devicePath(id) + '/connection', { signal }),
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
