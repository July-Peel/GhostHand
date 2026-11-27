import { useId, useRef, useState } from 'react'
import { useIsMutating, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { LoaderCircle, RefreshCw } from 'lucide-react'
import { agentAPI, browserExtensionKey, connectionKey, windowsInstallerNames } from './agent-api'
import type { OperationKind, WindowsInstallerFile, WindowsInstallerName } from './agent-api'
import { errorText } from './api'
import type { Device, Session } from './api'
import { Panel } from './device-ui'

const formatDate = (time: number) => new Date(time).toLocaleString('zh-CN', { dateStyle: 'short', timeStyle: 'medium' })

export function BrowserExtensionSection({ device, session }: { device: Device; session: Session }) {
  const client = useQueryClient()
  const key = browserExtensionKey(device.id)
  const online = device.conn_state === 'online' || device.conn_state === 'unstable'
  const [confirm, setConfirm] = useState(false)
  const status = useQuery({
    queryKey: key, queryFn: ({ signal }) => agentAPI.browserExtension(device.id, signal),
    enabled: online, gcTime: 0, retry: false, refetchOnMount: 'always',
  })
  const change = useMutation({
    mutationFn: (action: 'install' | 'uninstall') => agentAPI.changeBrowserExtension(device.id, action, session.csrf_token),
    onSuccess: data => client.setQueryData(key, data),
    onSettled: () => {
      setConfirm(false)
      return client.invalidateQueries({ queryKey: key })
    },
  })
  const data = status.data
  const installed = !!(data?.installed || data?.connected)
  const leftovers = !!(data?.registered || data?.bundle_present)
  const busy = !online || !data || status.isFetching || status.isError || change.isPending
  const locked = device.desktop_state !== 'unlocked'
  const summary = !online ? '设备离线'
    : status.isFetching ? '正在检测插件状态…'
    : status.isError || !data ? '插件状态不可用'
    : data.connected ? `已安装并连接（v${data.version}）`
    : installed ? '已安装（Chrome 未启动或扩展未连接）'
    : data.registered ? '存在注册表残留（未挂载扩展）'
    : data.bundle_present ? '存在扩展文件残留' : '未安装'
  const tone = online && !status.isFetching && !status.isError
    ? data?.connected ? 'online' : installed || leftovers ? 'unstable' : 'offline' : 'offline'

  return <section className="agent-section">
    <h3>Chrome 插件</h3>
    <div className="agent-actions">
      <span className={'connection ' + tone} role="status"><span aria-hidden="true" />{summary}</span>
      <button type="button" className="icon-button" disabled={!online || status.isFetching || change.isPending}
        onClick={() => void status.refetch()} title="刷新状态" aria-label="刷新 Chrome 插件状态"><RefreshCw size={16} /></button>
    </div>
    <div className="agent-actions">
      <button type="button" className="secondary-button" title={installed ? '插件已安装，无需重复安装' : undefined}
        disabled={busy || installed || locked || device.helper_state !== 'ready'} onClick={() => change.mutate('install')}>
        {change.isPending && change.variables === 'install'
          ? <><LoaderCircle size={15} className="spin" /> 正在自动安装…</> : installed ? '已安装' : '安装 Chrome 插件'}
      </button>
      <button type="button" className="danger-outline-button" disabled={busy || (!installed && !leftovers)}
        onClick={() => setConfirm(true)}>
        {change.isPending && change.variables === 'uninstall' ? '正在卸载…' : '卸载 Chrome 插件'}
      </button>
    </div>
    {confirm && <div className="agent-confirm">
      <p>确认卸载该设备上的 GhostHand Chrome 扩展并清理注册表？</p>
      <div className="agent-actions">
        <button type="button" className="danger-button" disabled={busy || (!installed && !leftovers)}
          onClick={() => change.mutate('uninstall')}>确认卸载</button>
        <button type="button" className="text-button" disabled={change.isPending} onClick={() => setConfirm(false)}>取消</button>
      </div>
    </div>}
    {online && !installed && locked && <p>请先在下方使用远程解锁，或在设备上解锁桌面后再安装。</p>}
    {online && !installed && !locked && device.helper_state !== 'ready' && <p>Windows 执行器就绪后即可安装。</p>}
    {online && status.isError && <p role="alert">{errorText(status.error)}</p>}
    {data?.error && online && !status.isFetching && !status.isError && <p>{data.error}</p>}
    {change.isError && <p role="alert">{errorText(change.error)}</p>}
    {change.isSuccess && <p role="status">{change.data.message}</p>}
  </section>
}

const pairingPlatforms = ['Windows', 'Linux', 'Android'] as const
const windowsInstallerFilesKey = ['pairing', 'windows-files'] as const

function WindowsInstallerFileRow({ name, saved, statusLoaded, csrf }: {
  name: WindowsInstallerName
  saved: WindowsInstallerFile | undefined
  statusLoaded: boolean
  csrf: string
}) {
  const client = useQueryClient()
  const input = useRef<HTMLInputElement>(null)
  const [progress, setProgress] = useState(0)
  const mutationKey = ['pairing', 'windows-upload', name] as const
  const upload = useMutation({
    mutationKey,
    mutationFn: (file: File) => agentAPI.uploadWindowsInstallerFile(name, file, csrf, setProgress),
    onSuccess: async savedFile => {
      await client.cancelQueries({ queryKey: windowsInstallerFilesKey, exact: true })
      client.setQueryData<WindowsInstallerFile[]>(windowsInstallerFilesKey, files =>
        files && [...files.filter(file => file.name !== name), savedFile])
      void client.invalidateQueries({ queryKey: windowsInstallerFilesKey, exact: true })
    },
  })
  const uploading = useIsMutating({ mutationKey, exact: true }) > 0
  const percent = upload.isPending ? progress : undefined
  const action = saved ? '替换' : '上传'

  return <li>
    {uploading && <div className="pairing-file-fill" aria-hidden="true" style={{ width: `${percent ?? 0}%` }} />}
    <div className="pairing-file-label">
      <strong>{name}</strong>
      <span aria-live="polite">{saved ? `已上传 · ${(saved.size / (1024 * 1024)).toFixed(1)} MiB` : statusLoaded ? '未上传' : '—'}</span>
    </div>
    <input type="file" ref={input} accept=".exe" hidden onChange={event => {
      const file = event.currentTarget.files?.[0]
      event.currentTarget.value = ''
      if (!file || uploading) return
      setProgress(0)
      upload.mutate(file)
    }} />
    {uploading ? <span className="pairing-file-progress" role="progressbar" aria-label={`${name} 上传进度`}
      aria-valuenow={percent} aria-valuetext={percent === 100 ? '100%，正在保存' : undefined}>
      {percent === undefined ? '上传中…' : percent === 100 ? '100% · 保存中' : `${percent}%`}
    </span> : <button type="button" className="secondary-button" aria-label={`${action} ${name}`}
      onClick={() => input.current?.click()}>{action}</button>}
    {upload.isError && <p role="alert">{errorText(upload.error)}</p>}
  </li>
}

export function PairingPanel({ session, close }: { session: Session; close: () => void }) {
  const client = useQueryClient()
  const panelId = useId()
  const [platform, setPlatform] = useState<typeof pairingPlatforms[number]>('Windows')
  const keyQuery = ['pairing'] as const
  const pairing = useQuery({ queryKey: keyQuery, queryFn: ({ signal }) => agentAPI.getPairing(signal), gcTime: 0 })
  const files = useQuery({ queryKey: windowsInstallerFilesKey, queryFn: ({ signal }) => agentAPI.windowsInstallerFiles(signal) })
  const [confirmRotate, setConfirmRotate] = useState(false)
  const [copied, setCopied] = useState<'key' | 'svc' | null>(null)
  const rotate = useMutation({
    mutationFn: () => agentAPI.rotatePairingKey(session.csrf_token),
    onSuccess: data => {
      setConfirmRotate(false)
      client.setQueryData(keyQuery, data)
    },
  })
  const details = pairing.data
  const svcCmd = details?.windows_install_command
  const installerReady = files.isSuccess && windowsInstallerNames.every(name => files.data.some(file => file.name === name))

  async function copyText(kind: 'key' | 'svc', text: string) {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(kind)
      window.setTimeout(() => setCopied(current => current === kind ? null : current), 1600)
    } catch {
      // Ignore clipboard permission errors.
    }
  }

  return (
    <Panel title="接入设备" className="settings-panel" close={close}>
      <div className="settings-nav" role="tablist" aria-label="设备平台">
        {pairingPlatforms.map((name, index) => <button key={name} type="button" role="tab"
          className="settings-section-button" id={panelId + '-tab-' + name} aria-controls={panelId + '-' + name}
          aria-selected={platform === name} tabIndex={platform === name ? 0 : -1} onClick={() => setPlatform(name)}
          onKeyDown={event => {
            const last = pairingPlatforms.length - 1
            const next = event.key === 'ArrowRight' ? (index + 1) % pairingPlatforms.length
              : event.key === 'ArrowLeft' ? (index + last) % pairingPlatforms.length
              : event.key === 'Home' ? 0 : event.key === 'End' ? last : -1
            if (next < 0) return
            event.preventDefault()
            setPlatform(pairingPlatforms[next])
            document.getElementById(panelId + '-tab-' + pairingPlatforms[next])?.focus()
          }}>{name}</button>)}
      </div>
      <div role="tabpanel" id={panelId + '-Windows'} aria-labelledby={panelId + '-tab-Windows'} hidden={platform !== 'Windows'}>
        <section className="agent-section pairing-install">
          <h3>Windows 安装文件</h3>
          <p>分别上传三个 Windows x64 EXE。Helper 请使用自包含版本。</p>
          {files.isPending && <p role="status">正在读取上传状态…</p>}
          {files.isError && <p role="alert">{errorText(files.error)} <button type="button" className="text-button" onClick={() => void files.refetch()}>重试</button></p>}
          <ul className="pairing-files">
            {windowsInstallerNames.map(name => <WindowsInstallerFileRow key={name} name={name}
              saved={files.data?.find(file => file.name === name)} statusLoaded={files.isSuccess} csrf={session.csrf_token} />)}
          </ul>
          {pairing.isPending && <p role="status">正在读取服务端接入密钥…</p>}
          {pairing.isError && <p role="alert">{errorText(pairing.error)} <button type="button" className="text-button" onClick={() => void pairing.refetch()}>重试</button></p>}
          <div className="agent-actions">
            <h3>安装为 Windows 服务</h3>
            {installerReady && svcCmd && <button type="button" className="text-button" onClick={() => void copyText('svc', svcCmd)}>
              {copied === 'svc' ? '已复制' : '复制命令'}
            </button>}
          </div>
          {installerReady && svcCmd ? <>
            <p>在新设备上以管理员身份打开 64 位 PowerShell，粘贴并运行这一条命令。</p>
            <pre className="agent-code">{svcCmd}</pre>
          </> : details && !svcCmd ? <p>请将控制端地址配置为 HTTPS，以生成 Windows 安装命令。</p>
            : files.isSuccess && <p>上传全部三个文件后即可复制安装命令。</p>}
        </section>
        <section className="agent-section">
          {details && <>
            <div className="agent-kv">
              <label htmlFor={panelId + '-key'} className="pairing-key-label">服务端固定接入密钥</label>
              <div className="settings-model-row">
                <input id={panelId + '-key'} value={details.key} readOnly spellCheck={false} onFocus={event => event.currentTarget.select()} />
                <button type="button" className="secondary-button" onClick={() => void copyText('key', details.key)}>
                  {copied === 'key' ? '已复制' : '复制密钥'}
                </button>
              </div>
            </div>
            <img className="pairing-qr" alt="设备接入二维码" src={'data:image/png;base64,' + details.qr_png} />
            <dl className="agent-kv">
              <dt>控制端地址</dt><dd><code>{details.server}</code></dd>
              {details.fingerprint && <><dt>证书指纹</dt><dd><code>{details.fingerprint}</code></dd></>}
            </dl>
            {!details.managed_by_env && (
              confirmRotate ? (
                <div className="agent-confirm">
                  <p>重置固定密钥后，旧密钥将无法用于新设备接入或手动重连（已在线连接会保持至下次重启）。确定重置？</p>
                  <div className="agent-actions">
                    <button type="button" className="secondary-button" disabled={rotate.isPending} onClick={() => rotate.mutate()}>
                      {rotate.isPending ? '正在重置…' : '确认重置'}
                    </button>
                    <button type="button" className="text-button" disabled={rotate.isPending} onClick={() => setConfirmRotate(false)}>取消</button>
                  </div>
                </div>
              ) : (
                <button type="button" className="secondary-button" onClick={() => setConfirmRotate(true)}>重置固定密钥</button>
              )
            )}
            {details.managed_by_env && <p>当前固定密钥由环境变量 GH_NODE_KEY 管理。</p>}
            {rotate.isError && <p role="alert">{errorText(rotate.error)}</p>}
          </>}
        </section>
      </div>
      {pairingPlatforms.slice(1).map(name => <div key={name} className="pairing-empty" role="tabpanel"
        id={panelId + '-' + name} aria-labelledby={panelId + '-tab-' + name} hidden={platform !== name} tabIndex={0} />)}
    </Panel>
  )
}

const operationNames: Record<OperationKind, string> = {
  'log.collect': '拉取日志', 'agent.restart': '重启 Core', 'helper.restart': '重启执行器',
  'agent.update': '升级', 'agent.rollback': '回滚', 'agent.uninstall': '卸载 GhostHand',
}
const operationStates = { queued: '待处理', succeeded: '已完成', failed: '失败' }

export function DeviceOperations({ device, session }: { device: Device; session: Session }) {
  const client = useQueryClient()
  const key = connectionKey(device.id)
  const [hours, setHours] = useState(4)
  const [confirm, setConfirm] = useState<'revoke' | 'agent.rollback' | 'agent.uninstall' | null>(null)
  const history = useQuery({
    queryKey: key, queryFn: ({ signal }) => agentAPI.history(device.id, signal),
    refetchInterval: query => query.state.data?.operations.some(operation => operation.state === 'queued') ? 5000 : false,
  })
  const action = useMutation({
    mutationFn: async (kind: OperationKind | 'revoke' | 'restore') => {
      if (kind === 'revoke') {
        await agentAPI.revoke(device.id, session.csrf_token)
      } else if (kind === 'restore') {
        await agentAPI.restore(device.id, session.csrf_token)
      } else {
        await agentAPI.operate(device.id, kind, kind === 'log.collect' ? hours : undefined, session.csrf_token)
      }
    },
    onSuccess: () => {
      setConfirm(null)
      void client.invalidateQueries({ queryKey: key })
    },
  })
  const data = history.data
  const outstanding = (kind: OperationKind) => data?.operations.some(op => op.kind === kind && op.state === 'queued')
  const uninstallPending = outstanding('agent.uninstall')
  const busy = action.isPending || !data || data.revoked || uninstallPending

  return (
    <>
      <section className="agent-section">
        <h3>远程运维</h3>
        {data?.version && <p>Core {data.version}</p>}
        {data?.revoked && (
          <div className="agent-actions">
            <p>设备凭证已吊销。</p>
            <button type="button" className="secondary-button" disabled={action.isPending} onClick={() => action.mutate('restore')}>恢复接入</button>
          </div>
        )}
        {history.isPending && <p role="status">正在加载运维记录…</p>}
        {history.isError && <p role="alert">{errorText(history.error)} <button className="text-button" onClick={() => void history.refetch()}>重试</button></p>}
        <form className="agent-actions" onSubmit={event => { event.preventDefault(); action.mutate('log.collect') }}>
          <label>最近 <input type="number" value={Number.isNaN(hours) ? '' : hours} onChange={event => setHours(event.target.valueAsNumber)}
            min={1} max={168} required aria-label="日志小时数" /> 小时</label>
          <button className="secondary-button" disabled={busy || outstanding('log.collect')}>拉取日志</button>
        </form>
        <div className="agent-actions">
          <button type="button" className="secondary-button" disabled={busy || outstanding('agent.restart')} onClick={() => action.mutate('agent.restart')}>重启 Core</button>
          {device.platform === 'windows' && <>
            <button type="button" className="secondary-button" disabled={busy || outstanding('helper.restart')} onClick={() => action.mutate('helper.restart')}>重启执行器</button>
            <button type="button" className="secondary-button" disabled={busy || outstanding('agent.rollback')} onClick={() => setConfirm('agent.rollback')}>回滚版本</button>
          </>}
          <button type="button" className="text-button" disabled={busy} onClick={() => setConfirm('revoke')}>吊销凭证</button>
          {device.platform === 'windows' && <button type="button" className="secondary-button"
            disabled={busy || !data?.can_uninstall || data.operations.some(op => op.state === 'queued')}
            onClick={() => setConfirm('agent.uninstall')}>卸载节点</button>}
        </div>
        {device.platform === 'windows' && data && !data.revoked && !data.can_uninstall &&
          <p>此设备尚未上报远程卸载能力。</p>}
        {confirm && (
          <div className="agent-confirm">
            {confirm === 'agent.uninstall'
              ? <p>卸载「{device.name}」上的 GhostHand？节点收到指令后会删除服务、程序及本地配置和日志。重新接入需要安装并配对。</p>
              : <p>{confirm === 'revoke' ? '吊销会立即断开设备。再次连接需要重新配对。' : '回滚会重启 Core 并恢复上一个可用版本。'}</p>}
            <div className="agent-actions">
              <button type="button" className="secondary-button" disabled={busy} onClick={() => action.mutate(confirm)}>
                {confirm === 'agent.uninstall' ? '确认卸载' : '确认'}
              </button>
              <button type="button" className="text-button" onClick={() => setConfirm(null)}>取消</button>
            </div>
          </div>
        )}
        {action.isError && <p role="alert">{errorText(action.error)}</p>}
        {data && data.operations.length > 0 && <ul className="agent-list">
          {data.operations.map(op => <li key={op.id}>
            <strong>{operationNames[op.kind]} · {
              op.kind === 'agent.uninstall' && op.state === 'succeeded' ? '节点已接管清理' : operationStates[op.state]
            }</strong>
            <time>{formatDate(op.finished_at ?? op.created_at)}</time>
            {op.kind === 'agent.uninstall' && op.state === 'succeeded' && <span>节点退出后无法回报最终清理结果。</span>}
            {op.state === 'failed' && <span>{op.detail}</span>}
            {op.log_available && <a className="text-button" href={agentAPI.logURL(device.id, op.id)}>下载日志</a>}
          </li>)}
        </ul>}
      </section>
    </>
  )
}
