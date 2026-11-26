export type Session = { username: string; csrf_token: string; expires_at: number }
export type SystemSettings = {
  webdav: { url: string; username: string; password: string }
  llm: { base_url: string; model: string; api_key: string }
}

export type Command = { slash: string; title: string; capability: 'device.info' | 'device.battery' | 'screen.capture' }
export type Device = {
  id: string
  name: string
  platform: 'windows' | 'android' | 'linux'
  arch: string
  os_version: string
  group: string
  conn_state: 'online' | 'unstable' | 'sleeping' | 'offline'
  helper_state: 'ready' | 'restarting' | 'unavailable' | 'none'
  desktop_state: 'unlocked' | 'locked' | 'screen_off' | 'no_session' | 'headless'
  task_state: 'idle' | 'running' | 'waiting_user'
  battery_pct: number | null
  charging: boolean | null
  last_seen_at: number
  task_protocol: boolean
  is_demo: boolean
  auto_approve: boolean
}

export type TaskCard = {
  id: string
  source?: 'quick' | 'brain' | 'mcp'
  title: string
  state: 'queued' | 'dispatched' | 'running' | 'paused' | 'waiting_user' | 'cancelling' | 'succeeded' | 'cancelled' | 'expired' | 'failed' | 'unknown'
  step_index: number
  step_total: number
  step_name: string
  started_at: number | null
  finished_at: number | null
  expires_at: number
  lane: 'ui' | 'bg'
  elapsed_ms: number
  error_code?: string
  control?: Exclude<TaskAction, 'retry'>
  cancel_at: number | null
  stalled: boolean
  timeline: { seq: number; step_name: string; detail: string; status: string; elapsed_ms: number; attempt: number; created_at: number }[]
}

export type TaskAction = 'pause' | 'resume' | 'cancel' | 'force' | 'retry'
export type MediaCard = { id: string; name: string; size: number }
export type ConfirmCard = {
  id: string
  task_id: string
  summary: string
  risk_action: string
  attachment_id?: string
  decision: 'pending' | 'approved' | 'denied' | 'cancelled'
}

export type StatusCard = Pick<Device, 'battery_pct' | 'charging' | 'os_version' | 'arch' | 'desktop_state'> & {
  title: string
}

type MessageBase = { id: number; sender: 'user' | 'agent' | 'system'; text: string; created_at: number }
export type Message = MessageBase & (
  | { type: 'text' | 'system' }
  | { type: 'task_card'; payload: TaskCard }
  | { type: 'status_card'; payload: StatusCard }
  | { type: 'media'; payload: MediaCard }
  | { type: 'confirm_card'; payload: ConfirmCard }
)
export type MessagePage = { messages: Message[]; has_older: boolean }
type DevicesResponse = { devices: Device[]; commands: Command[] }

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, init)
  if (!response.ok) {
    if (response.status === 401 && path !== '/api/login' && path !== '/api/session') {
      window.dispatchEvent(new Event('ghosthand:unauthorized'))
    }
    const body = await response.json().catch(() => null) as { error?: string } | null
    throw new ApiError(response.status, body?.error ?? '无法完成请求，请稍后重试。')
  }
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

export function post(body: object, token?: string): RequestInit {
  return {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { 'X-CSRF-Token': token } : {}),
    },
    body: JSON.stringify(body),
  }
}

const devicePath = (id: string) => '/api/devices/' + encodeURIComponent(id)

export const sessionKey = ['session'] as const
export const settingsKey = ['settings'] as const
export const devicesKey = ['devices'] as const
export const messagesKey = (id: string) => ['messages', id] as const

export const api = {
  async session(signal: AbortSignal): Promise<Session | null> {
    try {
      return await request<Session>('/api/session', { signal })
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) return null
      throw error
    }
  },
  login: (username: string, password: string) => request<Session>('/api/login', post({ username, password })),
  logout: (csrf: string) => request<void>('/api/logout', post({}, csrf)),
  settings: (signal: AbortSignal) => request<SystemSettings>('/api/settings', { signal }),
  mcpTokenStatus: (signal: AbortSignal) => request<{ created_at: number }>('/api/settings/mcp', { signal }),
  changeMCPToken: (action: 'issue' | 'revoke', csrf: string) => request<{ created_at: number; token?: string }>(
    '/api/settings/mcp', post({ action }, csrf),
  ),
  changePassword: (current: string, password: string, csrf: string) => request<void>(
    '/api/settings/password', post({ current_password: current, new_password: password }, csrf),
  ),
  saveWebDAV: (url: string, username: string, password: string, csrf: string) => request<void>(
    '/api/settings/webdav', post({ url, username, password }, csrf),
  ),
  saveLLM: (baseURL: string, model: string, key: string, csrf: string) => request<void>(
    '/api/settings/llm', post({ base_url: baseURL, model, api_key: key }, csrf),
  ),
  listModels: (baseURL: string, key: string, csrf: string) => request<{ models: string[] }>(
    '/api/settings/llm/models', post({ base_url: baseURL, api_key: key }, csrf),
  ),
  devices: (signal: AbortSignal) => request<DevicesResponse>('/api/devices', { signal }),
  messages: (id: string, before: number, signal: AbortSignal) => request<MessagePage>(
    devicePath(id) + '/messages' + (before ? '?before=' + before : ''), { signal },
  ),
  send: (id: string, text: string, csrf: string) => request<void>(devicePath(id) + '/messages', post({ text }, csrf)),
  clearMessages: (id: string, csrf: string) => request<void>(devicePath(id) + '/messages/clear', post({}, csrf)),
  taskControl: (id: string, task: string, action: TaskAction, csrf: string) => request<void>(
    devicePath(id) + '/tasks/' + encodeURIComponent(task) + '/' + action, post({}, csrf),
  ),
  attachment: (id: string, variant: 'thumb' | 'content') => '/api/attachments/' + encodeURIComponent(id) + '/' + variant,
  confirm: (device: string, id: string, approve: boolean, csrf: string) => request<void>(
    devicePath(device) + '/confirmations/' + encodeURIComponent(id) + '/decision', post({ approve }, csrf),
  ),
  setDevicePolicy: (device: string, autoApprove: boolean, csrf: string) => request<void>(
    devicePath(device) + '/policy', post({ auto_approve: autoApprove }, csrf),
  ),
}

export const errorText = (error: unknown) =>
  error instanceof ApiError ? error.message : '连接中断，请检查网络后重试。'
