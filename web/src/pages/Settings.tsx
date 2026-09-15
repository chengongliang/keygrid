import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import i18n from '@/i18n'
import { api, type AuditLog, type User } from '@/lib/api'

const PAGE_SIZE = 20

export default function Settings({ user, onLogout }: { user: User | null; onLogout: () => void }) {
  const { t } = useTranslation()
  const [logs, setLogs] = useState<AuditLog[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [err, setErr] = useState('')

  const load = (p: number) => {
    api.get<{ items: AuditLog[]; total: number }>(`/api/audit?page=${p}&size=${PAGE_SIZE}`)
      .then((d) => {
        setLogs(d.items ?? [])
        setTotal(d.total ?? 0)
      })
      .catch((e) => setErr(e.message))
  }

  useEffect(() => { load(1) }, [])

  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  return (
    <div className="space-y-6">
      <div className="card">
        <h2 className="mb-3 text-lg font-semibold">{t('settings.account')}</h2>
        <div className="space-y-1 text-sm text-muted">
          <div>{t('settings.email')}<span className="text-soft">{user?.email}</span></div>
          <div>{t('settings.role')}<span className="text-soft">{user?.role}</span></div>
        </div>
        <button className="btn-danger mt-4" onClick={onLogout}>{t('settings.logout')}</button>
      </div>

      <div>
        <h2 className="mb-3 text-lg font-semibold">{t('settings.auditLog')}</h2>
        {err && <div className="rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
        <div className="card overflow-x-auto p-0">
          <table className="w-full text-sm">
            <thead>
              <tr className="thead-row text-left text-xs">
                <th className="px-4 py-3">{t('settings.colTime')}</th><th className="px-4 py-3">{t('settings.colEvent')}</th>
                <th className="px-4 py-3">{t('settings.colDetail')}</th><th className="px-4 py-3">IP</th>
              </tr>
            </thead>
            <tbody>
              {logs.map((l) => (
                <tr key={l.id} className="tbody-row">
                  <td className="px-4 py-2.5 text-xs text-muted">{new Date(l.created_at).toLocaleString()}</td>
                  <td className="px-4 py-2.5">
                    <EventBadge event={l.event} />
                  </td>
                  <td className="max-w-md truncate px-4 py-2.5 text-xs text-muted" title={l.detail || undefined}>{translateDetail(l.event, l.detail)}</td>
                  <td className="px-4 py-2.5 font-mono text-xs text-muted">{l.ip}</td>
                </tr>
              ))}
              {logs.length === 0 && (
                <tr><td colSpan={4} className="px-4 py-10 text-center text-muted">{t('settings.noAuditLogs')}</td></tr>
              )}
            </tbody>
          </table>
        </div>
        <div className="mt-3 flex items-center justify-between text-sm text-muted">
          <span>{t('settings.totalRecords', { total })}</span>
          <div className="flex items-center gap-2">
            <button className="btn-ghost" disabled={page <= 1}
              onClick={() => { const p = page - 1; setPage(p); load(p) }}>{t('common.prev')}</button>
            <span>{page} / {pages}</span>
            <button className="btn-ghost" disabled={page >= pages}
              onClick={() => { const p = page + 1; setPage(p); load(p) }}>{t('common.next')}</button>
          </div>
        </div>
      </div>
    </div>
  )
}

// 事件 → 标签 key（字典 settings.event.*）+ 徽标配色（悬停可见原始事件名，便于与后端日志对照）
const EVENT_META: Record<string, { label: string; cls: string }> = {
  'user.login': { label: 'settings.event.login', cls: 'badge-green' },
  'user.register': { label: 'settings.event.register', cls: 'badge-green' },
  'user.login_fail': { label: 'settings.event.loginFail', cls: 'badge-red' },
  'user.login_rate_limited': { label: 'settings.event.loginRateLimited', cls: 'badge-red' },
  'user.oidc_login': { label: 'settings.event.oidcLogin', cls: 'badge-green' },
  'user.oidc_login_fail': { label: 'settings.event.oidcLoginFail', cls: 'badge-red' },
  'apikey.create': { label: 'settings.event.apiKeyCreate', cls: 'badge-yellow' },
  'apikey.update': { label: 'settings.event.apiKeyUpdate', cls: 'badge-zinc' },
  'apikey.reveal': { label: 'settings.event.apiKeyReveal', cls: 'badge-zinc' },
  'apikey.delete': { label: 'settings.event.apiKeyDelete', cls: 'badge-red' },
  'provider.create': { label: 'settings.event.providerCreate', cls: 'badge-yellow' },
  'provider.update': { label: 'settings.event.providerUpdate', cls: 'badge-zinc' },
  'provider.delete': { label: 'settings.event.providerDelete', cls: 'badge-red' },
  'provider.oauth_authorized': { label: 'settings.event.providerOauth', cls: 'badge-green' },
  'admin.role_change': { label: 'settings.event.roleChange', cls: 'badge-red' },
  'admin.user_disable': { label: 'settings.event.userDisable', cls: 'badge-red' },
  'admin.user_enable': { label: 'settings.event.userEnable', cls: 'badge-green' },
  'admin.reset_password': { label: 'settings.event.resetPassword', cls: 'badge-red' },
  'admin.settings_update': { label: 'settings.event.settingsUpdate', cls: 'badge-zinc' },
  'admin.invitation_create': { label: 'settings.event.invitationCreate', cls: 'badge-yellow' },
  'admin.price_upsert': { label: 'settings.event.priceUpsert', cls: 'badge-zinc' },
  'admin.price_delete': { label: 'settings.event.priceDelete', cls: 'badge-red' },
  'admin.price_sync': { label: 'settings.event.priceSync', cls: 'badge-yellow' },
}

function EventBadge({ event }: { event: string }) {
  const { t } = useTranslation()
  const meta = EVENT_META[event]
  return <span className={(meta?.cls ?? 'badge-zinc')} title={event}>{meta ? t(meta.label) : event}</span>
}

// ===== 详情列中文化：把后端记录的技术性 detail 翻译成直观描述 =====

const ROLE_LABELS: Record<string, string> = { admin: 'settings.roleNames.admin', user: 'settings.roleNames.user' }

const OAUTH_PROVIDER_LABELS: Record<string, string> = {
  openai: 'OpenAI',
  anthropic: 'Anthropic',
  claude: 'Claude',
  kimi: 'Kimi',
  gemini: 'Gemini',
}

// 价格同步来源 → 中文
const SOURCE_LABELS: Record<string, string> = {
  openrouter: 'OpenRouter',
}

// 渠道更新字段名 → 标签 key（字典 settings.field.*）
const PROVIDER_FIELD_LABELS: Record<string, string> = {
  name: 'settings.field.name',
  base_url: 'settings.field.baseUrl',
  protocol: 'settings.field.protocol',
  model_map: 'settings.field.modelMap',
  priority: 'settings.field.priority',
  enabled: 'settings.field.enabled',
  'credential(rotated)': 'settings.field.credentialRotated',
}

// 系统设置变更项 → 标签 key（字典 settings.setting.*；admin.settings_update 的 detail 为逗号分隔的多项）
const SETTING_LABELS: Record<string, string> = {
  'registration_policy=open': 'settings.setting.regOpen',
  'registration_policy=invite': 'settings.setting.regInvite',
  'registration_policy=closed': 'settings.setting.regClosed',
  'maintenance_mode=1': 'settings.setting.maintOn',
  'maintenance_mode=0': 'settings.setting.maintOff',
  'oidc settings': 'settings.setting.oidc',
  announcement: 'settings.setting.announcement',
}

function translateDetail(event: string, detail: string): string {
  const d = (detail ?? '').trim()
  if (!d) return '—'

  switch (event) {
    case 'user.register': // detail 为注册邮箱
      return i18n.t('settings.detail.newUser', { d })

    case 'user.login_fail': {
      if (d.startsWith('user not found: ')) return i18n.t('settings.detail.userNotFound', { d: d.slice('user not found: '.length) })
      if (d === 'wrong password') return i18n.t('settings.detail.wrongPassword')
      if (d === 'oidc-only user tried password login') return i18n.t('settings.detail.oidcOnlyLogin')
      return d
    }

    case 'user.oidc_login':
      if (d.startsWith('sub=')) return i18n.t('settings.detail.oidcSub', { d: d.slice(4) })
      return d

    case 'user.oidc_login_fail': {
      const prefixes: [string, string][] = [
        ['idp error: ', 'settings.detail.idpError'],
        ['token exchange: ', 'settings.detail.tokenExchange'],
        ['id_token verify: ', 'settings.detail.idTokenVerify'],
        ['resolve user: ', 'settings.detail.resolveUser'],
      ]
      for (const [p, label] of prefixes) {
        if (d.startsWith(p)) return i18n.t(label) + d.slice(p.length)
      }
      if (d === 'nonce mismatch') return i18n.t('settings.detail.nonceMismatch')
      return d
    }

    case 'apikey.create': {
      // 形如 "name=my-key prefix=sk-xxxx"
      const m = d.match(/^name=(.*?)\s+prefix=(\S+)$/)
      if (m) return i18n.t('settings.detail.apiKeyCreated', { name: m[1], prefix: m[2] })
      return d
    }

    case 'apikey.update':
    case 'apikey.reveal':
      if (d.startsWith('prefix=')) return i18n.t('settings.detail.apiKeyPrefix', { prefix: d.slice('prefix='.length) })
      return d

    case 'apikey.delete':
      return i18n.t('settings.detail.apiKeyDeleted', { d })

    case 'provider.create': {
      // 形如 "api_key/openai my-gateway base_url=https://..."
      const m = d.match(/^(\S+)\/(\S*)\s+(.*?)\s+base_url=(\S+)$/)
      if (m) {
        const kind = m[1] === 'oauth' ? i18n.t('settings.detail.kindOauth') : i18n.t('settings.detail.kindApiKey')
        const idp = OAUTH_PROVIDER_LABELS[m[2]] ?? m[2]
        return i18n.t('settings.detail.providerCreated', {
          kind: kind + (idp ? ` · ${idp}` : ''),
          name: m[3],
          url: m[4],
        })
      }
      return d
    }

    case 'provider.update': {
      // 形如 "my-gateway fields=name,base_url,..."
      const idx = d.indexOf(' fields=')
      if (idx >= 0) {
        const fields = d.slice(idx + ' fields='.length)
          .split(',').map((f) => i18n.t(PROVIDER_FIELD_LABELS[f.trim()] ?? f.trim())).join(i18n.t('settings.detail.fieldsJoiner'))
        return i18n.t('settings.detail.providerUpdated', { name: d.slice(0, idx), fields })
      }
      return d
    }

    case 'provider.delete':
      return i18n.t('settings.detail.providerDeleted', { d })

    case 'provider.oauth_authorized': {
      // 形如 "openai provider=名称"，可能带 " (pasted callback)"
      const pasted = d.includes('(pasted callback)')
      const m = d.replace(/\s*\(pasted callback\)/, '').match(/^(\S+)\s+provider=(.+)$/)
      if (m) {
        const idp = OAUTH_PROVIDER_LABELS[m[1]] ?? m[1]
        return i18n.t('settings.detail.oauthAuthorized', {
          name: m[2],
          idp,
          pasted: pasted ? i18n.t('settings.detail.pastedCallback') : '',
        })
      }
      return d
    }

    case 'admin.role_change': {
      // 形如 "user 3 role -> admin"
      const m = d.match(/^user (\d+) role -> (\S+)$/)
      if (m) return i18n.t('settings.detail.roleChanged', { id: m[1], role: i18n.t(ROLE_LABELS[m[2]] ?? m[2]) })
      return d
    }

    case 'admin.user_disable': {
      const m = d.match(/^user (\d+) disabled/)
      if (m) return i18n.t('settings.detail.userDisabled', { id: m[1] })
      return d
    }

    case 'admin.user_enable': {
      const m = d.match(/^user (\d+) enabled$/)
      if (m) return i18n.t('settings.detail.userEnabled', { id: m[1] })
      return d
    }

    case 'admin.reset_password': {
      const m = d.match(/^password reset for user (\d+)$/)
      if (m) return i18n.t('settings.detail.passwordReset', { id: m[1] })
      return d
    }

    case 'admin.invitation_create':
      if (d.startsWith('invitation ')) return i18n.t('settings.detail.invitationCreated', { d: d.slice('invitation '.length) })
      return d

    case 'admin.settings_update':
      // 逗号分隔的多项变更，逐项翻译
      return d.split(',')
        .map((s) => i18n.t(SETTING_LABELS[s.trim()] ?? s.trim()))
        .join(i18n.t('settings.detail.settingsJoiner'))

    case 'admin.price_upsert': {
      // 形如 "model=gpt-4o prompt=0 completion=0"（单价：USD / 1M tokens）
      const m = d.match(/^model=(.*?)\s+prompt=(\S+)\s+completion=(\S+)$/)
      if (m) return i18n.t('settings.detail.priceUpserted', { model: m[1], prompt: m[2], completion: m[3] })
      return d
    }

    case 'admin.price_delete': {
      // 形如 "model=claude-x"
      const m = d.match(/^model=(.+)$/)
      if (m) return i18n.t('settings.detail.priceDeleted', { model: m[1] })
      return d
    }

    case 'admin.price_sync': {
      // 形如 "count=431 source=openrouter"
      const m = d.match(/^count=(\d+)\s+source=(\S+)$/)
      if (m) return i18n.t('settings.detail.priceSynced', { n: m[1], source: SOURCE_LABELS[m[2]] ?? m[2] })
      return d
    }

    default:
      return d
  }
}
