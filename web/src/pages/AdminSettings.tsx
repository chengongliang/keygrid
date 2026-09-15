import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, type Invitation, type OidcSettings, type PlatformSettings } from '@/lib/api'

const DEFAULT_OIDC: OidcSettings = {
  enabled: false,
  display_name: '',
  issuer: '',
  client_id: '',
  client_secret: '',
  scopes: 'openid email profile',
  callback_url: '',
}

export default function AdminSettings() {
  const { t } = useTranslation()
  const [s, setS] = useState<PlatformSettings | null>(null)
  const [announcement, setAnnouncement] = useState('')
  const [proxyURL, setProxyURL] = useState('')
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [invs, setInvs] = useState<Invitation[]>([])
  const [newCode, setNewCode] = useState('')
  const [copied, setCopied] = useState(false)
  const [oidc, setOidc] = useState<OidcSettings>(DEFAULT_OIDC)
  const [cbCopied, setCbCopied] = useState(false)
  const [oidcSecretVisible, setOidcSecretVisible] = useState(false)

  const flash = (m: string) => { setMsg(m); setErr(''); setTimeout(() => setMsg(''), 3000) }

  const load = () => {
    api.get<PlatformSettings>('/api/admin/settings')
      .then((d) => { setS(d); setAnnouncement(d.announcement ?? ''); setProxyURL(d.proxy_url ?? ''); if (d.oidc) setOidc({ ...DEFAULT_OIDC, ...d.oidc }) })
      .catch((e) => setErr(e.message))
    api.get<Invitation[]>('/api/admin/invitations').then((d) => setInvs(d ?? [])).catch(() => {})
  }
  useEffect(load, [])

  const save = async (body: Record<string, unknown>, okMsg: string) => {
    setBusy(true); setErr('')
    try {
      const d = await api.put<PlatformSettings>('/api/admin/settings', body)
      setS(d)
      setAnnouncement(d.announcement ?? '')
      setProxyURL(d.proxy_url ?? '')
      if (d.oidc) setOidc({ ...DEFAULT_OIDC, ...d.oidc })
      flash(okMsg)
    } catch (e: any) { setErr(e.message) } finally { setBusy(false) }
  }

  const saveOidc = () => save({
    oidc: {
      enabled: oidc.enabled,
      display_name: oidc.display_name.trim(),
      issuer: oidc.issuer.trim(),
      client_id: oidc.client_id.trim(),
      client_secret: oidc.client_secret.trim(),
      scopes: oidc.scopes.trim(),
    },
  }, t('adminSettings.oidc.saved'))

  const createInvite = async () => {
    setBusy(true); setErr('')
    try {
      const inv = await api.post<Invitation>('/api/admin/invitations', {})
      setNewCode(inv.code)
      setCopied(false)
      // 只刷新邀请码列表：整体 load() 会用服务端值重置公告/OIDC 表单，丢掉未保存的编辑
      api.get<Invitation[]>('/api/admin/invitations').then((d) => setInvs(d ?? [])).catch(() => {})
    } catch (e: any) { setErr(e.message) } finally { setBusy(false) }
  }

  if (!s) {
    return (
      <div>
        <h2 className="mb-4 text-lg font-semibold">{t('adminSettings.title')}</h2>
        {err && <div className="rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
        <div className="text-sm text-muted">{t('common.loading')}</div>
      </div>
    )
  }

  return (
    <div className="max-w-3xl space-y-6">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">{t('adminSettings.title')}</h2>
      </div>
      {err && <div className="rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
      {msg && <div className="rounded-lg px-3 py-2 text-sm badge-green w-fit">{msg}</div>}

      {/* 公告 banner */}
      <div className="card">
        <h3 className="mb-1 text-sm font-semibold">{t('adminSettings.announcement.title')}</h3>
        <p className="mb-3 text-xs text-muted">{t('adminSettings.announcement.desc')}</p>
        <textarea className="input min-h-20" maxLength={2000} value={announcement}
          placeholder={t('adminSettings.announcement.placeholder')}
          onChange={(e) => setAnnouncement(e.target.value)} />
        <div className="mt-3 flex justify-end">
          <button className="btn-primary" disabled={busy}
            onClick={() => save({ announcement: announcement.trim() }, t('adminSettings.announcement.saved'))}>
            {t('adminSettings.announcement.save')}
          </button>
        </div>
      </div>

      {/* 维护模式 */}
      <div className="card">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold">{t('adminSettings.maintenance.title')}</h3>
            <p className="mt-1 text-xs text-muted">
              {t('adminSettings.maintenance.desc')}
            </p>
          </div>
          {s.maintenance_mode
            ? <span className="badge-red shrink-0">{t('adminSettings.maintenance.active')}</span>
            : <span className="badge-green shrink-0">{t('adminSettings.maintenance.normal')}</span>}
        </div>
        <div className="mt-3 flex justify-end">
          {s.maintenance_mode ? (
            <button className="btn-primary" disabled={busy}
              onClick={() => save({ maintenance_mode: false }, t('adminSettings.maintenance.exitedMsg'))}>
              {t('adminSettings.maintenance.closeBtn')}
            </button>
          ) : (
            <button className="btn-danger" disabled={busy}
              onClick={() => { if (confirm(t('adminSettings.maintenance.confirmMsg'))) save({ maintenance_mode: true }, t('adminSettings.maintenance.openedMsg')) }}>
              {t('adminSettings.maintenance.openBtn')}
            </button>
          )}
        </div>
      </div>

      {/* 注册策略 */}
      <div className="card">
        <h3 className="text-sm font-semibold">{t('adminSettings.register.title')}</h3>
        <p className="mb-3 mt-1 text-xs text-muted">{t('adminSettings.register.desc')}</p>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
          {[
            { v: 'open', label: t('adminSettings.register.open'), desc: t('adminSettings.register.openDesc') },
            { v: 'invite', label: t('adminSettings.register.invite'), desc: t('adminSettings.register.inviteDesc') },
            { v: 'closed', label: t('adminSettings.register.closed'), desc: t('adminSettings.register.closedDesc') },
          ].map((opt) => (
            <button key={opt.v} disabled={busy}
              className={'rounded-xl p-3 text-left transition ' + (s.registration_policy === opt.v ? 'tab-active' : '')}
              style={{ border: '1px solid var(--line)' }}
              onClick={() => save({ registration_policy: opt.v }, t('adminSettings.register.saved', { name: opt.label }))}>
              <div className="text-sm font-medium">{opt.label}</div>
              <div className="mt-0.5 text-xs text-muted">{opt.desc}</div>
            </button>
          ))}
        </div>
      </div>

      {/* 网络代理（出口统一由管理员配置；用户在渠道上只勾选是否启用） */}
      <div className="card">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold">{t('adminSettings.proxy.title')}</h3>
          {proxyURL.trim()
            ? <span className="badge-green shrink-0">{t('adminSettings.proxy.configured')}</span>
            : <span className="badge-zinc shrink-0">{t('adminSettings.proxy.unconfigured')}</span>}
        </div>
        <p className="mb-3 mt-1 text-xs text-muted">
          {t('adminSettings.proxy.desc')}
        </p>
        <div className="flex gap-2">
          <input className="input font-mono text-xs" value={proxyURL}
            placeholder={t('adminSettings.proxy.placeholder')}
            spellCheck={false} autoComplete="off"
            onChange={(e) => setProxyURL(e.target.value)} />
          <button className="btn-primary-lg shrink-0" disabled={busy}
            onClick={() => save({ proxy_url: proxyURL.trim() }, t('adminSettings.proxy.saved'))}>
            {t('adminSettings.proxy.save')}
          </button>
        </div>
      </div>

      {/* OIDC 单点登录（参考 new-api OidcSetting：表单整体保存，保存即热生效） */}
      <div className="card">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold">{t('adminSettings.oidc.title')}</h3>
          {oidc.enabled
            ? <span className="badge-green shrink-0">{t('common.enabled')}</span>
            : <span className="badge-zinc shrink-0">{t('adminSettings.oidc.off')}</span>}
        </div>
        <p className="mb-3 mt-1 text-xs text-muted">
          {t('adminSettings.oidc.desc')}
        </p>

        <div className="mono-well mb-3 flex items-center justify-between gap-2 rounded-lg px-3 py-2 font-mono text-xs">
          <span className="truncate">{oidc.callback_url || '…'}</span>
          <button className="btn-ghost shrink-0"
            onClick={async () => { await navigator.clipboard.writeText(oidc.callback_url); setCbCopied(true); setTimeout(() => setCbCopied(false), 1500) }}>
            {cbCopied ? '✅ ' + t('common.copied') : t('adminSettings.oidc.copyCallback')}
          </button>
        </div>
        <p className="mb-3 text-xs text-muted">{t('adminSettings.oidc.callbackTip')}</p>

        <label className="mb-3 flex items-center gap-2 text-sm">
          <input type="checkbox" className="accent-violet-500" checked={oidc.enabled}
            onChange={(e) => setOidc({ ...oidc, enabled: e.target.checked })} />
          {t('adminSettings.oidc.enableLabel')}
        </label>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div>
            <label className="text-xs text-muted">{t('adminSettings.oidc.issuerLabel')}</label>
            <input className="input mt-1" placeholder="https://sso.example.com/realms/main" value={oidc.issuer}
              spellCheck={false} autoComplete="off"
              onChange={(e) => setOidc({ ...oidc, issuer: e.target.value })} />
          </div>
          <div>
            <label className="text-xs text-muted">{t('adminSettings.oidc.displayNameLabel')}</label>
            <input className="input mt-1" placeholder={t('adminSettings.oidc.displayNamePlaceholder')} value={oidc.display_name}
              onChange={(e) => setOidc({ ...oidc, display_name: e.target.value })} />
          </div>
          <div>
            <label className="text-xs text-muted">Client ID</label>
            <input className="input mt-1" placeholder="keygrid" value={oidc.client_id}
              spellCheck={false} autoComplete="off"
              onChange={(e) => setOidc({ ...oidc, client_id: e.target.value })} />
          </div>
          <div>
            <label className="text-xs text-muted">Client Secret</label>
            <div className="mt-1 flex gap-1">
              <input className="input" type={oidcSecretVisible ? 'text' : 'password'} placeholder="******" value={oidc.client_secret}
                spellCheck={false} autoComplete="off"
                onChange={(e) => setOidc({ ...oidc, client_secret: e.target.value })} />
              <button className="btn-ghost-lg shrink-0" type="button"
                onClick={() => setOidcSecretVisible((v) => !v)}>
                {oidcSecretVisible ? t('adminSettings.oidc.hide') : t('adminSettings.oidc.show')}
              </button>
            </div>
          </div>
          <div className="sm:col-span-2">
            <label className="text-xs text-muted">{t('adminSettings.oidc.scopesLabel')}</label>
            <input className="input mt-1" placeholder="openid email profile" value={oidc.scopes}
              spellCheck={false} autoComplete="off"
              onChange={(e) => setOidc({ ...oidc, scopes: e.target.value })} />
          </div>
        </div>

        <div className="mt-3 flex justify-end">
          <button className="btn-primary" disabled={busy} onClick={saveOidc}>{t('adminSettings.oidc.save')}</button>
        </div>
      </div>

      {/* 邀请码 */}
      <div className="card">
        <div className="mb-3 flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold">{t('adminSettings.invite.title')}</h3>
            <p className="mt-1 text-xs text-muted">{t('adminSettings.invite.desc')}</p>
          </div>
          <button className="btn-primary" disabled={busy} onClick={createInvite}>{t('adminSettings.invite.generate')}</button>
        </div>
        {newCode && (
          <div className="mono-well mb-3 flex items-center justify-between rounded-lg px-3 py-2 font-mono text-sm">
            <span>{newCode}</span>
            <button className="btn-ghost"
              onClick={async () => { await navigator.clipboard.writeText(newCode); setCopied(true); setTimeout(() => setCopied(false), 1500) }}>
              {copied ? '✅ ' + t('common.copied') : t('common.copy')}
            </button>
          </div>
        )}
        {invs.length > 0 && (
          <table className="w-full text-sm">
            <thead>
              <tr className="thead-row text-left text-xs">
                <th className="px-2 py-2">{t('adminSettings.invite.colCode')}</th><th className="px-2 py-2">{t('common.status')}</th>
                <th className="px-2 py-2">{t('adminSettings.invite.colExpires')}</th>
              </tr>
            </thead>
            <tbody>
              {invs.slice(0, 8).map((inv) => (
                <tr key={inv.id} className="tbody-row">
                  <td className="px-2 py-2 font-mono text-xs">{inv.code}</td>
                  <td className="px-2 py-2">
                    {inv.used_by
                      ? <span className="badge-zinc">{t('adminSettings.invite.used')}</span>
                      : new Date(inv.expires_at) < new Date()
                        ? <span className="badge-red">{t('adminSettings.invite.expired')}</span>
                        : <span className="badge-green">{t('adminSettings.invite.available')}</span>}
                  </td>
                  <td className="px-2 py-2 text-xs text-muted">{new Date(inv.expires_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
