import { useState } from 'react'
import type { FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bot, HardDrive, KeyRound, Plug } from 'lucide-react'
import { api, errorText, settingsKey } from './api'
import type { Session, SystemSettings } from './api'
import { Panel } from './device-ui'

const sections = [
  { id: 'account', title: '账号', Icon: KeyRound },
  { id: 'webdav', title: 'WebDAV', Icon: HardDrive },
  { id: 'llm', title: '模型接口', Icon: Bot },
  { id: 'mcp', title: 'MCP', Icon: Plug },
] as const

type Section = typeof sections[number]['id']
type Feedback = { ok: boolean; text: string }

export function SettingsPanel({ session, close, onPasswordChanged }: {
  session: Session
  close: () => void
  onPasswordChanged: () => void
}) {
  const [section, setSection] = useState<Section>('account')
  const query = useQuery({
    queryKey: settingsKey,
    queryFn: ({ signal }) => api.settings(signal),
    refetchOnWindowFocus: false,
    gcTime: 0,
    enabled: section === 'webdav' || section === 'llm',
  })

  return (
    <Panel title="系统设置" className="settings-panel" close={close}>
      <div className="settings-nav" role="group" aria-label="设置分类">
        {sections.map(({ id, title, Icon }) => (
          <button key={id} className="settings-section-button" type="button" aria-pressed={section === id} onClick={() => setSection(id)}>
            <Icon size={16} />{title}
          </button>
        ))}
      </div>
      <div className="settings-content">
        <div hidden={section !== 'account'}>
          <PasswordForm token={session.csrf_token} onChanged={onPasswordChanged} />
        </div>
        {(section === 'webdav' || section === 'llm') && !query.data && (
          query.isPending
            ? <p className="settings-hint" role="status">正在读取设置…</p>
            : <p className="inline-error" role="alert">{errorText(query.error)} <button type="button" className="text-button" onClick={() => void query.refetch()}>重试</button></p>
        )}
        {section === 'mcp' && <MCPForm csrf={session.csrf_token} />}
        {query.data && (
          <>
            <div hidden={section !== 'webdav'}>
              <WebDAVForm initial={query.data.webdav} token={session.csrf_token} />
            </div>
            <div hidden={section !== 'llm'}>
              <ModelForm initial={query.data.llm} token={session.csrf_token} />
            </div>
          </>
        )}
      </div>
    </Panel>
  )
}

function MCPForm({ csrf }: { csrf: string }) {
  const key = ['mcp-token-status'] as const
  const client = useQueryClient()
  const status = useQuery({ queryKey: key, queryFn: ({ signal }) => api.mcpTokenStatus(signal), gcTime: 0 })
  const [issued, setIssued] = useState('')
  const change = useMutation({
    mutationFn: (action: 'issue' | 'revoke') => api.changeMCPToken(action, csrf),
    onMutate: () => setIssued(''),
    onSuccess: ({ created_at, token }) => {
      setIssued(token ?? '')
      client.setQueryData(key, { created_at })
    },
    gcTime: 0,
  })
  const active = Boolean(status.data?.created_at)

  return (
    <div className="settings-form">
      <h3>MCP 访问令牌</h3>
      <p className="settings-hint">外部 MCP 客户端可通过此令牌调用设备工具（包括状态读取、屏幕截图、键鼠控制、Shell/脚本执行及文件读写）。</p>
      <label>Streamable HTTP 地址<input readOnly value={new URL('/api/mcp', window.location.origin).href} /></label>
      <p className="settings-hint">在客户端设置 Authorization 请求头，值为 Bearer 加空格和令牌。加上 ?device=设备ID 可直连一台设备。</p>
      {status.isPending && <p className="settings-hint" role="status">正在读取令牌状态…</p>}
      {status.isError && <p className="inline-error" role="alert">{errorText(status.error)} <button type="button" className="text-button" onClick={() => void status.refetch()}>重试</button></p>}
      {status.isSuccess && <p className="settings-hint" role="status">{active ? '令牌已启用。重新生成会使旧令牌失效。' : '尚未生成令牌。'}</p>}
      {issued && (
        <label>新令牌（仅显示一次）
          <input value={issued} readOnly autoComplete="off" spellCheck={false} onFocus={event => event.currentTarget.select()} />
        </label>
      )}
      {change.isError && <p className="inline-error" role="alert">{errorText(change.error)}</p>}
      <div className="settings-model-row">
        <button className="primary-button" type="button" disabled={change.isPending || !status.isSuccess} onClick={() => change.mutate('issue')}>
          {active ? '重新生成令牌' : '生成令牌'}
        </button>
        {active && <button className="secondary-button" type="button" disabled={change.isPending} onClick={() => change.mutate('revoke')}>撤销令牌</button>}
      </div>
    </div>
  )
}

function PasswordForm({ token, onChanged }: { token: string; onChanged: () => void }) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    const fields = new FormData(event.currentTarget)
    const current = String(fields.get('current_password'))
    const password = String(fields.get('new_password'))
    if (password !== fields.get('confirm_password')) {
      setError('两次输入的新密码不一致。')
      return
    }

    setPending(true)
    setError('')
    try {
      await api.changePassword(current, password, token)
      onChanged()
    } catch (failure) {
      setError(errorText(failure))
    } finally {
      setPending(false)
    }
  }

  return (
    <form className="settings-form" onSubmit={submit}>
      <h3>修改管理员密码</h3>
      <p className="settings-hint">至少 12 个字符。修改后，所有会话都需要重新登录。</p>
      <fieldset disabled={pending}>
        <label>当前密码<input name="current_password" type="password" autoComplete="current-password" maxLength={256} required /></label>
        <label>新密码<input name="new_password" type="password" autoComplete="new-password" minLength={12} maxLength={256} required /></label>
        <label>确认新密码<input name="confirm_password" type="password" autoComplete="new-password" minLength={12} maxLength={256} required /></label>
      </fieldset>
      {error && <p className="inline-error" role="alert">{error}</p>}
      <button className="primary-button" type="submit" disabled={pending}>{pending ? '正在修改…' : '修改密码'}</button>
    </form>
  )
}

function WebDAVForm({ initial, token }: { initial: SystemSettings['webdav']; token: string }) {
  const [url, setURL] = useState(initial.url)
  const [username, setUsername] = useState(initial.username)
  const [password, setPassword] = useState(initial.password)
  const [pending, setPending] = useState(false)
  const [feedback, setFeedback] = useState<Feedback | null>(null)

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    const endpoint = url.trim()
    setPending(true)
    setFeedback(null)
    try {
      await api.saveWebDAV(endpoint, endpoint ? username.trim() : '', endpoint ? password : '', token)
      setURL(endpoint)
      setUsername(endpoint ? username.trim() : '')
      if (!endpoint) setPassword('')
      setFeedback({ ok: true, text: '设置已保存。' })
    } catch (failure) {
      setFeedback({ ok: false, text: errorText(failure) })
    } finally {
      setPending(false)
    }
  }

  return (
    <form className="settings-form" onSubmit={submit}>
      <h3>WebDAV</h3>
      <fieldset disabled={pending}>
        <label>
          WebDAV 地址
          <input type="url" value={url} onChange={event => setURL(event.target.value)}
            placeholder="https://nas.example.com/remote.php/dav/files/user/" spellCheck={false} maxLength={2048} />
        </label>
        <label>用户名<input value={username} onChange={event => setUsername(event.target.value)} autoComplete="off" maxLength={256} /></label>
        <label>
          密码
          <input type="text" value={password} onChange={event => setPassword(event.target.value)}
            autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={4096} />
        </label>
      </fieldset>
      <p className="settings-hint">保存时会自动创建缺失的目录。清空地址后保存可移除 WebDAV 配置。</p>
      {feedback && <p className={feedback.ok ? 'settings-saved' : 'inline-error'} role={feedback.ok ? 'status' : 'alert'}>{feedback.text}</p>}
      <button className="primary-button" type="submit" disabled={pending}>{pending ? '保存中…' : '保存设置'}</button>
    </form>
  )
}

function ModelForm({ initial, token }: { initial: SystemSettings['llm']; token: string }) {
  const [url, setURL] = useState(initial.base_url)
  const [key, setKey] = useState(initial.api_key)
  const [model, setModel] = useState(initial.model)
  const [models, setModels] = useState<string[] | null>(null)
  const [pending, setPending] = useState<'list' | 'save' | null>(null)
  const [feedback, setFeedback] = useState<Feedback | null>(null)

  async function fetchModels() {
    const endpoint = url.trim()
    if (!endpoint || pending) return
    setPending('list')
    setFeedback(null)
    try {
      const { models: available } = await api.listModels(endpoint, key, token)
      setModels(available)
      setModel(previous => available.includes(previous) ? previous : '')
      setFeedback(available.length === 0
        ? { ok: false, text: '模型接口未返回可选模型。' }
        : { ok: true, text: '已获取 ' + available.length + ' 个模型。' })
    } catch (failure) {
      setFeedback({ ok: false, text: errorText(failure) })
    } finally {
      setPending(null)
    }
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    const endpoint = url.trim()
    setPending('save')
    setFeedback(null)
    try {
      await api.saveLLM(endpoint, endpoint ? model : '', endpoint ? key : '', token)
      setURL(endpoint)
      if (!endpoint) {
        setKey('')
        setModel('')
        setModels(null)
      }
      setFeedback({ ok: true, text: '设置已保存。' })
    } catch (failure) {
      setFeedback({ ok: false, text: errorText(failure) })
    } finally {
      setPending(null)
    }
  }

  return (
    <form className="settings-form" onSubmit={submit}>
      <h3>OpenAI Responses</h3>
      <fieldset disabled={pending !== null}>
        <label>
          API Base URL
          <input type="url" value={url} placeholder="https://api.openai.com/v1"
            onChange={event => { setURL(event.target.value); setModels(null); setFeedback(null) }}
            spellCheck={false} maxLength={2048} />
        </label>
        <label>
          API Key
          <input type="text" value={key} onChange={event => { setKey(event.target.value); setModels(null); setFeedback(null) }}
            autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={4096} />
        </label>
        <div className="settings-model-row">
          <label>
            模型
            <select value={model} required={url.trim() !== ''} onChange={event => setModel(event.target.value)}>
              <option value="">{models ? '请选择模型' : '先获取模型列表'}</option>
              {models === null && model && <option value={model}>{model}</option>}
              {models?.map(id => <option key={id} value={id}>{id}</option>)}
            </select>
          </label>
          <button type="button" className="secondary-button" disabled={!url.trim() || pending !== null} onClick={() => void fetchModels()}>
            {pending === 'list' ? '获取中…' : '获取模型'}
          </button>
        </div>
      </fieldset>
      <p className="settings-hint">获取模型后选定一个并保存。清空地址后保存可移除配置。</p>
      {feedback && <p className={feedback.ok ? 'settings-saved' : 'inline-error'} role={feedback.ok ? 'status' : 'alert'}>{feedback.text}</p>}
      <button className="primary-button" type="submit" disabled={pending !== null}>{pending === 'save' ? '保存中…' : '保存设置'}</button>
    </form>
  )
}
