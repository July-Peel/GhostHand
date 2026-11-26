import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { LoaderCircle } from 'lucide-react'
import { agentAPI, rdpKey } from './agent-api'
import type { RDPAction, RDPStatus } from './agent-api'
import { devicesKey, errorText } from './api'
import type { Device, Session } from './api'
import { desktops } from './device-ui'

const rdpErrors: Record<string, string> = {
  E_RDP_NOT_SUPPORTED: '当前 Windows 版本不支持远程桌面服务端。',
  E_RDP_DISABLED: '请启用 Windows 远程桌面并确认 TermService 正在运行。',
  E_RDP_PORT_CLOSED: '本机回环端口无法连接，请核对 RDP 监听端口。',
  E_RDP_NOT_SERVICE_MODE: '请以 Windows SYSTEM 服务方式运行 Core。',
  E_RDP_USER_MISMATCH: '配置的账号与当前 Windows 控制台用户不一致，或无法解析该账号。',
  E_RDP_AUTH_FAILED: 'Windows 账号或密码错误。请更新解锁凭据。',
  E_RDP_BLANK_PASSWORD_RESTRICTED: '当前账号或 Windows 安全策略不允许空密码远程登录。请改用 Windows 账户密码。',
  E_RDP_ACCOUNT_LOCKED: 'Windows 账号已锁定、禁用或密码已过期。',
  E_RDP_ACCESS_DENIED: 'Windows 拒绝了远程桌面访问，或无法复用现有会话。请检查账号权限和单会话策略。',
  E_RDP_TSCON_FAILED: 'Windows 未能将远程会话交还物理控制台。',
  E_RDP_TICKET_EXPIRED: '解锁票据已过期，请重试。',
  E_RDP_TIMEOUT: '等待远程桌面登录或执行器就绪超时。',
  E_RDP_CLIENT_UNAVAILABLE: '未找到兼容的 FreeRDP Windows 客户端。',
  E_RDP_CERTIFICATE: '无法验证本机远程桌面证书，请检查监听器的证书绑定。',
  E_RDP_FIREWALL: '检测到入站 RDP 规则或无法确认防火墙状态，请检查远程桌面的入站规则。',
  E_RDP_CONNECTION: '远程桌面连接失败。请确认节点使用 HTTPS 接入并检查环境检测结果。',
  E_HELPER_UNAVAILABLE: '当前控制台的 Windows 执行器不可用，请确认 Helper 已更新。',
  E_BUSY: '设备正在执行其他桌面操作。',
}
const resultText = (code?: string) => rdpErrors[code ?? ''] ?? '远程桌面操作失败，请检查设备状态。'
const stages: Record<string, string> = {
  preflight: '正在检测本机环境…',
  login: '正在登录 Windows…',
  transfer: '正在交还物理屏…',
  helper: '正在等待 Windows 执行器就绪…',
}

export function RDPSection({ device, session }: { device: Device; session: Session }) {
  const client = useQueryClient()
  const queryKey = rdpKey(device.id)
  const online = device.conn_state === 'online' || device.conn_state === 'unstable'
  const [confirm, setConfirm] = useState(false)
  const status = useQuery({
    queryKey, queryFn: ({ signal }) => agentAPI.rdpStatus(device.id, signal), retry: false,
  })
  const action = useMutation({
    mutationFn: ({ kind, confirmed = false }: { kind: RDPAction; confirmed?: boolean }) =>
      agentAPI.rdpOperate(device.id, kind, confirmed, session.csrf_token),
    onSettled: () => {
      setConfirm(false)
      void client.invalidateQueries({ queryKey: devicesKey })
      return client.invalidateQueries({ queryKey })
    },
  })
  const data = status.data
  const preflight = data?.preflight?.detail
  const busy = action.isPending || !!data?.operation
  const unlocking = data?.operation === 'rdp.unlock' || (action.isPending && action.variables.kind === 'unlock')
  const actionSuccess = action.isSuccess && action.data.ok && action.data.kind !== 'rdp.preflight'

  return <section className="agent-section">
    <h3>远程解锁（RDP）</h3>
    <p>设备平时可以正常锁屏。启用自动解锁后，AI 或 MCP 需要操作桌面时才会解锁。请确保设备不会进入睡眠。</p>
    <div className="agent-actions">
      <span>{desktops[device.desktop_state]}</span>
      {data?.preflight && <span className={'connection ' + (data.preflight.ok ? 'online' : 'unstable')}>
        <span aria-hidden="true" />{data.preflight.ok ? '环境检测通过' : '环境需要调整'}
      </span>}
    </div>
    {preflight && preflight.firewall_rdp_inbound_open !== false && <p>
      {preflight.firewall_rdp_inbound_open
        ? '检测到 RDP 端口可能允许外部入站。请将入站规则限制为本机回环。'
        : '无法确认活动网络的 RDP 入站规则，请检查 Windows 防火墙。'}
    </p>}
    {data?.blank_password && preflight?.blank_password_restricted && <p>
      Windows 安全策略限制本机账号使用空密码远程登录。请为该账号设置密码，再更新解锁凭据。
    </p>}
    {!!data?.last_unlock_at && <p>
      最近一次解锁 · {new Date(data.last_unlock_at).toLocaleString('zh-CN')} · {data.last_result === 'ok' ? '成功' : resultText(data.last_result)}
    </p>}
    {data?.suspended && <p role="alert">自动解锁已暂停。Windows 连续两次拒绝凭据，请更新密码。</p>}
    {!online && <p>设备上线后可检测环境、解锁或锁屏。</p>}
    <div className="agent-actions">
      <button type="button" className="secondary-button" disabled={!online || !data?.configured || busy}
        onClick={() => data?.suspended ? setConfirm(true) : action.mutate({ kind: 'unlock' })}>
        {unlocking && <LoaderCircle size={14} className="spin" />}远程解锁桌面
      </button>
      {device.desktop_state === 'unlocked' && <button type="button" className="secondary-button" disabled={!online || busy}
        onClick={() => action.mutate({ kind: 'lock' })}>立即锁屏</button>}
      <button type="button" className="secondary-button" disabled={!online || !data || busy}
        onClick={() => action.mutate({ kind: 'preflight' })}>环境检测</button>
    </div>
    {unlocking && <p role="status">{stages[data?.stage ?? 'preflight'] ?? stages.preflight}</p>}
    {action.isPending && action.variables.kind !== 'unlock' && <p role="status">
      {action.variables.kind === 'preflight' ? '正在检测远程桌面环境…' : '正在锁屏…'}
    </p>}
    {confirm && <div className="agent-confirm">
      <p>自动解锁已暂停。确认使用当前凭据进行一次手动解锁？</p>
      <div className="agent-actions">
        <button className="secondary-button" type="button" disabled={busy}
          onClick={() => action.mutate({ kind: 'unlock', confirmed: true })}>确认解锁</button>
        <button className="text-button" type="button" onClick={() => setConfirm(false)}>取消</button>
      </div>
    </div>}
    {action.isError && <p role="alert">{errorText(action.error)}</p>}
    {action.isSuccess && !action.data.ok && <p role="alert">{resultText(action.data.code)}</p>}
    {actionSuccess && <p role="status">{action.data.kind === 'rdp.lock' ? '桌面已锁定。' : '桌面已解锁，Windows 执行器已就绪。'}</p>}
    {status.isError && <p role="alert">{errorText(status.error)} <button className="text-button" type="button"
      onClick={() => void status.refetch()}>重试</button></p>}
    {preflight && <RDPPreflightReport preflight={preflight} />}
    {data && <RDPCredentials key={device.id} device={device.id} csrf={session.csrf_token} data={data} busy={busy} />}
  </section>
}

function RDPPreflightReport({ preflight: p }: { preflight: NonNullable<NonNullable<RDPStatus['preflight']>['detail']> }) {
  const time = (minutes: number | null) => minutes === null ? '未知' : minutes === 0 ? '从不' : `${minutes} 分钟`
  return <details className="rdp-disclosure">
    <summary>环境检测结果</summary>
    <dl className="agent-kv">
      <dt>Windows</dt><dd>{p.os_version} {p.edition} · {p.rdp_supported ? '支持 RDP' : '不支持 RDP'}</dd>
      <dt>远程桌面</dt><dd>{p.rdp_enabled ? '已启用' : '未启用'} · {p.termservice_running ? '服务运行中' : '服务未运行'}</dd>
      <dt>回环端口</dt><dd>{p.port_reachable ? '可连接' : '无法连接'}</dd>
      <dt>网络级别身份验证</dt><dd>{p.nla_required ? '要求 NLA' : '未强制 NLA'}</dd>
      {p.blank_password_restricted !== undefined && <>
        <dt>本机账号空密码</dt><dd>{p.blank_password_restricted ? '系统策略限制远程登录' : '登录时由 Windows 校验'}</dd>
      </>}
      <dt>监听器证书</dt><dd>{p.certificate_available ? '已找到用于校验的证书' : '未找到可用于校验的证书'}</dd>
      <dt>Core</dt><dd>{p.service_mode ? 'SYSTEM 服务' : '请以 SYSTEM 服务运行'}</dd>
      <dt>RDP 客户端</dt><dd>{p.client_available ? '已找到 FreeRDP' : '未找到 FreeRDP'}</dd>
      <dt>物理控制台</dt><dd>{p.console_session.state === 'none' ? '未登录' :
        p.console_session.state === 'unknown' ? '无法读取' : `${p.console_session.username} · ${desktops[p.console_session.state]}`}</dd>
      <dt>睡眠超时</dt><dd>电源 {time(p.sleep_ac_minutes)} · 电池 {time(p.sleep_dc_minutes)}</dd>
      <dt>入站防火墙</dt><dd>{p.firewall_rdp_inbound_open === null ? '无法确认' : p.firewall_rdp_inbound_open ? '允许外部入站' : '检测未发现非回环允许规则'}</dd>
    </dl>
    {(!!p.sleep_ac_minutes || !!p.sleep_dc_minutes) && <p>系统可能在空闲时进入睡眠。可在配置中启用「保持系统不睡眠」。</p>}
    {!p.single_session && <p>Windows Server 请检查「将每个远程桌面服务用户限制到单独的远程桌面服务会话」策略。</p>}
  </details>
}

function RDPCredentials({ device, csrf, data, busy }: { device: string; csrf: string; data: RDPStatus; busy: boolean }) {
  const client = useQueryClient()
  const form = useRef<HTMLFormElement>(null)
  const password = useRef<HTMLInputElement>(null)
  const [clear, setClear] = useState(false)
  const [blankEdit, setBlankEdit] = useState<{ version: string; checked: boolean } | null>(null)
  const version = JSON.stringify([data.configured, data.username, data.domain, data.blank_password, data.rdp_port,
    data.auto_unlock, data.auto_relock, data.keep_system_awake])
  const blankPassword = blankEdit?.version === version ? blankEdit.checked : data.blank_password
  const passwordHint = !data.configured ? '无密码账号可留空'
    : data.blank_password ? '当前已配置为无密码账号' : '已加密保存，留空则保留'
  const save = useMutation({
    mutationFn: (remove: boolean) => {
      if (remove) return agentAPI.rdpConfig(device, { clear: true }, csrf)
      const fields = new FormData(form.current!)
      const explicitBlank = fields.has('blank_password')
      const secret = explicitBlank ? '' : (password.current?.value ?? '')
      if (password.current) password.current.value = ''
      return agentAPI.rdpConfig(device, {
        username: String(fields.get('username') ?? ''), domain: String(fields.get('domain') ?? ''),
        rdp_port: Number(fields.get('port')), password: secret, blank_password: explicitBlank || (!data.configured && secret === ''),
        auto_unlock: fields.has('auto_unlock'), auto_relock: fields.has('auto_relock'),
        keep_system_awake: fields.has('keep_system_awake'),
      }, csrf)
    },
    onSuccess: () => {
      setClear(false)
      setBlankEdit(null)
      return client.invalidateQueries({ queryKey: rdpKey(device) })
    },
  })
  return <details className="rdp-disclosure">
    <summary>配置解锁凭据</summary>
    <form key={version} ref={form} className="rdp-form" onSubmit={event => { event.preventDefault(); save.mutate(false) }}>
      <label>Windows 用户名<input required name="username" maxLength={200} defaultValue={data.username} autoComplete="off" /></label>
      <label>域或计算机名<input name="domain" maxLength={200} defaultValue={data.domain} autoComplete="off" placeholder="本机账号可留空" /></label>
      <label className="rdp-option"><input name="blank_password" type="checkbox" checked={blankPassword} onChange={event => {
        const checked = event.currentTarget.checked
        if (checked && password.current) password.current.value = ''
        setBlankEdit({ version, checked })
      }} /> 该 Windows 本机账号无密码</label>
      <label>Windows 密码<input ref={password} type="password" autoComplete="new-password" disabled={blankPassword}
        placeholder={blankPassword ? (data.blank_password ? passwordHint : '将保存为空密码') : passwordHint} /></label>
      <p>空密码远程登录取决于 Windows 安全策略。切换为有密码时，请取消勾选并输入 Windows 账户密码。</p>
      <label>RDP 端口<input name="port" type="number" required min={1} max={65535} defaultValue={data.rdp_port} /></label>
      <label className="rdp-option"><input name="auto_unlock" type="checkbox" defaultChecked={data.auto_unlock} /> AI / MCP 遇到锁屏时自动解锁</label>
      <label className="rdp-option"><input name="auto_relock" type="checkbox" defaultChecked={data.auto_relock} /> 自动解锁的任务结束后自动锁屏</label>
      <label className="rdp-option"><input name="keep_system_awake" type="checkbox" defaultChecked={data.keep_system_awake} /> 保持系统不睡眠（不影响锁屏）</label>
      <div className="agent-actions">
        <button className="secondary-button" disabled={busy || save.isPending}>保存配置</button>
        {data.configured && <button className="danger-outline-button" type="button" disabled={busy || save.isPending}
          onClick={() => setClear(true)}>清除凭据</button>}
      </div>
    </form>
    {clear && <div className="agent-confirm">
      <p>清除这台设备保存的 Windows 解锁凭据？</p>
      <div className="agent-actions">
        <button className="danger-button" type="button" disabled={busy || save.isPending} onClick={() => save.mutate(true)}>确认清除</button>
        <button className="text-button" type="button" onClick={() => setClear(false)}>取消</button>
      </div>
    </div>}
    {save.isError && <p role="alert">{errorText(save.error)}</p>}
    {save.isSuccess && <p role="status">{save.variables ? '已清除解锁凭据。' : '已保存解锁配置。'}</p>}
  </details>
}
