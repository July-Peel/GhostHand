import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { agentAPI, connectionKey, releasesKey } from './agent-api'
import type { OperationKind, Release, Rollout } from './agent-api'
import { errorText } from './api'
import type { Device, Session } from './api'
import { Panel } from './device-ui'

const formatDate = (time: number) => new Date(time).toLocaleString('zh-CN', { dateStyle: 'short', timeStyle: 'medium' })

export function PairingPanel({ session, close }: { session: Session; close: () => void }) {
  const client = useQueryClient()
  const keyQuery = ['pairing'] as const
  const pairing = useQuery({ queryKey: keyQuery, queryFn: ({ signal }) => agentAPI.getPairing(signal), gcTime: 0 })
  const [confirmRotate, setConfirmRotate] = useState(false)
  const [copied, setCopied] = useState<'key' | 'core' | 'svc' | null>(null)
  const rotate = useMutation({
    mutationFn: () => agentAPI.rotatePairingKey(session.csrf_token),
    onSuccess: data => {
      setConfirmRotate(false)
      client.setQueryData(keyQuery, data)
    },
  })
  const details = pairing.data
  const fpArg = details?.fingerprint ? ' --fingerprint ' + details.fingerprint : ''
  const coreCmd = details ? `& "$env:ProgramFiles\\GhostHand\\ghosthand-core.exe" --server "${details.server}" --key "${details.key}"${fpArg}` : ''
  const svcCmd = details ? `& "$env:ProgramFiles\\GhostHand\\ghosthand-launcher.exe" install --server "${details.server}" --key "${details.key}"${fpArg}` : ''

  async function copyText(kind: 'key' | 'core' | 'svc', text: string) {
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
      <section className="agent-section">
        <p>多个设备均可使用下面的服务端固定密钥接入。节点会根据硬件机器码生成唯一设备识别码，更新或手动重新执行时使用同一密钥即可自动关联原设备上线；节点端不保存任何日志或配置文件。</p>
        {pairing.isPending && <p role="status">正在读取服务端接入密钥…</p>}
        {pairing.isError && <p role="alert">{errorText(pairing.error)} <button type="button" className="text-button" onClick={() => void pairing.refetch()}>重试</button></p>}
        {details && <>
          <label className="agent-kv">
            <dt>服务端固定接入密钥</dt>
            <div className="settings-model-row">
              <input value={details.key} readOnly spellCheck={false} onFocus={event => event.currentTarget.select()} />
              <button type="button" className="secondary-button" onClick={() => void copyText('key', details.key)}>
                {copied === 'key' ? '已复制' : '复制密钥'}
              </button>
            </div>
          </label>
          <img className="pairing-qr" alt="设备接入二维码" src={'data:image/png;base64,' + details.qr_png} />
          <dl className="agent-kv">
            <dt>控制端地址</dt><dd><code>{details.server}</code></dd>
            {details.fingerprint && <><dt>证书指纹</dt><dd><code>{details.fingerprint}</code></dd></>}
          </dl>
          <div className="agent-actions">
            <strong>直接运行（无服务、不产生任何本地文件）</strong>
            <button type="button" className="text-button" onClick={() => void copyText('core', coreCmd)}>
              {copied === 'core' ? '已复制' : '复制命令'}
            </button>
          </div>
          <pre className="agent-code">{coreCmd}</pre>
          <div className="agent-actions">
            <strong>安装为 Windows 服务（无配置文件/日志文件）</strong>
            <button type="button" className="text-button" onClick={() => void copyText('svc', svcCmd)}>
              {copied === 'svc' ? '已复制' : '复制命令'}
            </button>
          </div>
          <pre className="agent-code">{svcCmd}</pre>
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

export function ReleasesPanel({ session, devices, close }: { session: Session; devices: Device[]; close: () => void }) {
  const client = useQueryClient()
  const releases = useQuery({ queryKey: releasesKey, queryFn: ({ signal }) => agentAPI.releases(signal) })
  const [file, setFile] = useState<File | null>(null)
  const [version, setVersion] = useState('')
  const [component, setComponent] = useState<Release['component']>('core')
  const [selected, setSelected] = useState<Release | null>(null)
  const [scope, setScope] = useState<'device' | 'group' | 'all'>('device')
  const [target, setTarget] = useState('')
  const [count, setCount] = useState<number | null>(null)
  const windows = devices.filter(device => !device.is_demo && device.platform === 'windows' && device.arch === 'amd64')
  const groups = [...new Set(windows.map(device => device.group).filter(Boolean))]

  const upload = useMutation({
    mutationFn: (packageFile: File) => agentAPI.upload(version.trim(), component, packageFile, session.csrf_token),
    onSuccess: () => {
      setVersion('')
      void client.invalidateQueries({ queryKey: releasesKey })
    },
  })
  const rollout = useMutation({
    mutationFn: ({ release, group }: { release: Release; group: Rollout }) =>
      agentAPI.rollout(release.id, group, session.csrf_token),
    onSuccess: result => { setCount(result.count); setSelected(null) },
  })
  return (
    <Panel title="版本管理" className="settings-panel" close={close}>
      <section className="agent-section">
        <form className="settings-form" onSubmit={event => { event.preventDefault(); if (file) upload.mutate(file) }}>
          <h3>上传 Windows x64 版本</h3>
          <label>组件<select value={component} onChange={event => setComponent(event.target.value as Release['component'])}>
            <option value="core">Core，可附带执行器</option><option value="helper_win">执行器</option>
          </select></label>
          <label>版本号<input required value={version} maxLength={64} pattern={'[A-Za-z0-9][A-Za-z0-9._+\\-]{0,63}'}
            onChange={event => setVersion(event.target.value)} /></label>
          <label>ZIP 版本包<input required type="file" accept=".zip" onChange={event => setFile(event.target.files?.[0] ?? null)} /></label>
          <p>{component === 'core' ? 'ZIP 根目录需要 ghosthand-core.exe，可同时放入 GhostHand.Helper.exe。' : '将单文件发布的 GhostHand.Helper.exe 放在 ZIP 根目录。'}</p>
          <button type="submit" className="primary-button" disabled={upload.isPending}>{upload.isPending ? '正在校验并上传…' : '上传并签名'}</button>
          {upload.isError && <p role="alert">{file && file.size > 96 * 1024 * 1024 ? '版本包不能超过 96 MiB。' : errorText(upload.error)}</p>}
        </form>
      </section>
      <section className="agent-section">
        <h3>已签名版本</h3>
        {releases.isError && <p role="alert">{errorText(releases.error)}</p>}
        {releases.data?.length === 0 && <p>暂无版本。</p>}
        {count !== null && <p role="status">已向 {count} 台设备下发升级。</p>}
        <ul className="agent-list">{releases.data?.map(release => (
          <li key={release.id}>
            <strong>{release.component === 'core' ? 'Core' : '执行器'} {release.version}</strong>
            <code title={release.sha256}>{release.sha256}</code>
            <button type="button" className="secondary-button" onClick={() => {
              setSelected(release); setScope('device'); setTarget(''); setCount(null); rollout.reset()
            }}>部署版本</button>
          </li>
        ))}</ul>
        {selected && (
          <form className="settings-form agent-confirm" onSubmit={event => {
            event.preventDefault()
            const rolloutScope: Rollout = scope === 'all' ? { all: true } : scope === 'group' ? { group: target } : { device_id: target }
            rollout.mutate({ release: selected, group: rolloutScope })
          }}>
            <h3>部署 {selected.version}</h3>
            <label>范围<select value={scope} onChange={event => {
              const next = event.target.value
              if (next === 'device' || next === 'group' || next === 'all') setScope(next)
              setTarget('')
            }}><option value="device">单台设备</option><option value="group">指定分组</option><option value="all">全部匹配的设备</option></select></label>
            {scope === 'device' && <label>设备<select required value={target} onChange={event => setTarget(event.target.value)}>
              <option value="">选择设备</option>{windows.map(device => <option key={device.id} value={device.id}>{device.name}</option>)}
            </select></label>}
            {scope === 'group' && <label>分组<select required value={target} onChange={event => setTarget(event.target.value)}>
              <option value="">选择分组</option>{groups.map(group => <option key={group}>{group}</option>)}
            </select></label>}
            <div className="agent-actions">
              <button type="submit" className="primary-button" disabled={rollout.isPending || (scope !== 'all' && !target)}>确认部署</button>
              <button type="button" className="text-button" onClick={() => setSelected(null)}>取消</button>
            </div>
            {rollout.isError && <p role="alert">{errorText(rollout.error)}</p>}
          </form>
        )}
      </section>
    </Panel>
  )
}
