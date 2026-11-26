import { StrictMode, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowRight, Hand, LoaderCircle, Moon, Sun } from 'lucide-react'
import { api, errorText, sessionKey } from './api'
import type { Session } from './api'
import { Workspace } from './workspace'
import './styles.css'

if (import.meta.env.PROD && 'serviceWorker' in navigator) {
  void navigator.serviceWorker.register('/sw.js').catch(error => console.warn('GhostHand PWA', error))
}

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })

export function Brand() {
  return (
    <div className="brand">
      <span className="brand-mark"><Hand size={19} aria-hidden="true" /></span>
      <span>GhostHand</span>
    </div>
  )
}

type Theme = 'light' | 'dark'

function initialTheme(): Theme {
  const saved = localStorage.getItem('ghosthand-theme')
  if (saved === 'light' || saved === 'dark') return saved
  return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

function ThemeButton({ theme, toggle }: { theme: Theme; toggle: () => void }) {
  const label = theme === 'dark' ? '切换浅色模式' : '切换深色模式'
  const Icon = theme === 'dark' ? Sun : Moon
  return <button className="icon-button" type="button" aria-label={label} title={label} onClick={toggle}><Icon size={18} /></button>
}

function Login({ onLogin, theme, toggle, notice }: {
  onLogin: (session: Session) => void
  theme: Theme
  toggle: () => void
  notice: string
}) {
  const mutation = useMutation({
    mutationFn: ({ username, password }: { username: string; password: string }) => api.login(username, password),
    onSuccess: onLogin,
  })

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const fields = new FormData(event.currentTarget)
    mutation.mutate({ username: String(fields.get('username')), password: String(fields.get('password')) })
  }

  return (
    <div className="login-page">
      <header className="login-top"><Brand /><ThemeButton theme={theme} toggle={toggle} /></header>
      <main className="login-main">
        <form className="login-card" onSubmit={submit}>
          <h1>登录工作台</h1>
          <p>使用管理员账号访问你的设备。</p>
          {notice && <div className="login-notice" role="status">{notice}</div>}
          <label htmlFor="username">用户名</label>
          <input id="username" name="username" autoComplete="username" maxLength={64} required />
          <label htmlFor="password">密码</label>
          <input id="password" name="password" type="password" autoComplete="current-password" required />
          {mutation.isError && <p className="inline-error" role="alert">{errorText(mutation.error)}</p>}
          <button className="primary-button login-submit" type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? '登录中' : '登录'}
            {mutation.isPending ? <LoaderCircle size={16} className="spin" /> : <ArrowRight size={16} />}
          </button>
        </form>
      </main>
    </div>
  )
}

function App() {
  const client = useQueryClient()
  const [theme, setTheme] = useState(initialTheme)
  const [loginNotice, setLoginNotice] = useState('')
  const session = useQuery({ queryKey: sessionKey, queryFn: ({ signal }) => api.session(signal) })
  const toggle = () => setTheme(value => value === 'light' ? 'dark' : 'light')

  useEffect(() => {
    document.documentElement.dataset.theme = theme
    localStorage.setItem('ghosthand-theme', theme)
  }, [theme])

  useEffect(() => {
    function clearSession() {
      void client.cancelQueries({ queryKey: sessionKey })
      client.setQueryData(sessionKey, null)
      client.removeQueries({ predicate: query => query.queryKey[0] !== sessionKey[0] })
    }
    window.addEventListener('ghosthand:unauthorized', clearSession)
    return () => window.removeEventListener('ghosthand:unauthorized', clearSession)
  }, [client])

  if (session.isPending) {
    return <main className="centered-state" role="status"><LoaderCircle size={20} className="spin" /> 正在加载工作台</main>
  }
  if (session.isError && session.data === undefined) {
    return (
      <main className="centered-state">
        <p role="alert">{errorText(session.error)}</p>
        <button className="secondary-button" onClick={() => void session.refetch()}>重试</button>
      </main>
    )
  }
  if (!session.data) {
    return (
      <Login
        onLogin={value => {
          setLoginNotice('')
          client.setQueryData(sessionKey, value)
        }}
        theme={theme}
        toggle={toggle}
        notice={loginNotice}
      />
    )
  }
  return (
    <Workspace
      session={session.data}
      brand={<Brand />}
      themeButton={<ThemeButton theme={theme} toggle={toggle} />}
      onPasswordChanged={() => {
        setLoginNotice('管理员密码已更新，请使用新密码登录。')
        window.dispatchEvent(new Event('ghosthand:unauthorized'))
      }}
    />
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode><QueryClientProvider client={queryClient}><App /></QueryClientProvider></StrictMode>,
)
