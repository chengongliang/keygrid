import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, type User } from '@/lib/api'
import { ThemeToggle } from '@/lib/theme'
import { LanguageToggle } from '@/i18n/LanguageToggle'

// /api/auth/config 的 SSO 子集
interface OidcConfig {
  oidc_enabled: boolean
  sso_text: string
}

export default function Login({ onLoggedIn, expired }: {
  onLoggedIn: (token: string, user: User) => void
  expired?: boolean
}) {
  const { t } = useTranslation()
  const [mode, setMode] = useState<'login' | 'register'>('login')
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [sso, setSso] = useState<OidcConfig | null>(null)
  // 注册策略：默认 open（避免加载中闪烁；请求失败时保持向后兼容，后端仍会拦截）
  const [canRegister, setCanRegister] = useState(true)

  // 登录页加载时拉 SSO 配置（仅 enabled 时渲染按钮）+ 注册策略（非 open 时隐藏注册入口）
  useEffect(() => {
    api.get<OidcConfig & { announcement?: string; maintenance_mode?: boolean; registration_policy?: string }>('/api/auth/config')
      .then((c) => {
        setSso({ oidc_enabled: !!c.oidc_enabled, sso_text: c.sso_text || '' })
        const open = (c.registration_policy || 'open') === 'open'
        setCanRegister(open)
        if (!open) setMode('login') // 策略关闭时若已停在注册模式，切回登录
      })
      .catch(() => {})
  }, [])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    setBusy(true)
    try {
      if (mode === 'register') {
        await api.post('/api/auth/register', { email, name, password })
      }
      const d = await api.post<{ token: string; user: User }>('/api/auth/login', { email, password })
      onLoggedIn(d.token, d.user)
    } catch (e2: any) {
      setErr(e2.message ?? t('login.requestFailed'))
    } finally {
      setBusy(false)
    }
  }

  // SSO 跳转 —— 当前 hash 作为登录后落地页带给后端
  const ssoLogin = () => {
    const next = location.hash && location.hash !== '#' ? location.hash : '#/'
    location.href = `/api/auth/oidc/login?next=${encodeURIComponent(next)}`
  }

  return (
    <div className="flex min-h-screen items-center justify-center p-4 relative">
      <div className="absolute right-4 top-4 flex items-center gap-2">
        <LanguageToggle />
        <ThemeToggle />
      </div>
      <div className="card w-full max-w-sm">
        <div className="mb-6">
          <img src="/logo.png" alt="KeyGrid" className="h-10 w-auto" draggable={false} />
        </div>
        {expired && (
          <div className="badge-yellow mb-4 w-full justify-center">{t('login.expired')}</div>
        )}
        <form onSubmit={submit} className="space-y-4">
          <div>
            <label className="label">{t('login.email')}</label>
            <input className="input" type="email" required value={email} onChange={(e) => setEmail(e.target.value)} placeholder="you@company.com" />
          </div>
          {mode === 'register' && (
            <div>
              <label className="label">{t('login.nameOptional')}</label>
              <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('login.namePlaceholder')} />
            </div>
          )}
          <div>
            <label className="label">{mode === 'register' ? t('login.passwordMin') : t('login.password')}</label>
            <input className="input" type="password" required minLength={mode === 'register' ? 8 : 1} value={password} onChange={(e) => setPassword(e.target.value)} placeholder="••••••••" />
          </div>
          {err && <div className="text-sm text-red-400">{err}</div>}
          <button className="btn-primary w-full" disabled={busy}>
            {busy ? '…' : mode === 'login' ? t('login.submitLogin') : t('login.submitRegister')}
          </button>
        </form>
        {sso?.oidc_enabled && mode === 'login' && (
          <>
            <div className="my-4 flex items-center gap-3 text-xs text-muted">
              <span className="h-px flex-1" style={{ background: 'var(--line)' }} />
              {t('login.or')}
              <span className="h-px flex-1" style={{ background: 'var(--line)' }} />
            </div>
            <button type="button" onClick={ssoLogin} className="btn-ghost w-full justify-center">
              🔑 {sso.sso_text || t('login.ssoDefaultText')}
            </button>
          </>
        )}
        {canRegister && (
          <button
            className="mt-4 w-full text-center text-sm text-muted hover:opacity-80"
            onClick={() => { setMode(mode === 'login' ? 'register' : 'login'); setErr('') }}
          >
            {mode === 'login' ? t('login.toRegister') : t('login.toLogin')}
          </button>
        )}
      </div>
    </div>
  )
}
