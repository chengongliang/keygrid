import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowUpCircle, ArrowDownCircle, Ban, CircleCheck, KeyRound } from 'lucide-react'
import { fmtCompact } from '@/components/usage-charts'
import { api, type AdminUserRow } from '@/lib/api'
import { Modal } from './Providers'

const PAGE_SIZE = 20

// 最近活跃：精确到分钟即可，完整含秒格式会把列撑到折行
const fmtTime = (s: string) =>
  new Date(s).toLocaleString([], { year: 'numeric', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })

export default function AdminUsers() {
  const { t } = useTranslation()
  const [rows, setRows] = useState<AdminUserRow[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [search, setSearch] = useState('')
  const [query, setQuery] = useState('')
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [loading, setLoading] = useState(true)
  const [resetTarget, setResetTarget] = useState<AdminUserRow | null>(null)
  const [resetPass, setResetPass] = useState('')
  const [copied, setCopied] = useState(false)
  const [busyId, setBusyId] = useState(0)

  const load = (p = page, q = query) => {
    setLoading(true)
    api.get<{ items: AdminUserRow[]; total: number }>(`/api/admin/users?search=${encodeURIComponent(q)}&page=${p}&size=${PAGE_SIZE}`)
      .then((d) => {
        setRows(d.items ?? [])
        setTotal(d.total ?? 0)
      })
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }
  useEffect(() => { load(1, '') /* eslint-disable-line react-hooks/exhaustive-deps */ }, [])

  const flash = (m: string) => { setMsg(m); setErr(''); setTimeout(() => setMsg(''), 3000) }
  const fail = (e: any) => { setErr(e.message ?? t('adminUsers.actionFailed')); setMsg('') }

  const patchUser = async (u: AdminUserRow, body: Record<string, unknown>, okMsg: string) => {
    setBusyId(u.id); setErr('')
    try {
      await api.patch(`/api/admin/users/${u.id}`, body)
      flash(okMsg)
      load()
    } catch (e: any) { fail(e) } finally { setBusyId(0) }
  }

  const doReset = async (u: AdminUserRow) => {
    setBusyId(u.id); setErr('')
    try {
      const r = await api.post<{ password: string }>(`/api/admin/users/${u.id}/reset_password`)
      setResetPass(r.password)
      setCopied(false)
      load()
    } catch (e: any) { fail(e) } finally { setBusyId(0) }
  }

  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  return (
    <div>
      <div className="mb-4 flex items-center justify-between gap-3">
        <h2 className="text-lg font-semibold">{t('adminUsers.title')}</h2>
        <form
          className="flex gap-2"
          onSubmit={(e) => { e.preventDefault(); setPage(1); setQuery(search); load(1, search) }}
        >
          <input className="input-sm w-40 sm:w-56" placeholder={t('adminUsers.searchPlaceholder')} value={search}
            onChange={(e) => setSearch(e.target.value)} />
          <button className="btn-ghost shrink-0">{t('common.search')}</button>
        </form>
      </div>
      {err && <div className="mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
      {msg && <div className="mb-3 rounded-lg px-3 py-2 text-sm badge-green w-fit">{msg}</div>}

      <div className="card overflow-x-auto p-0">
        {/* min-w：内容区被侧边栏压缩时优先横向滚动，不挤压列宽导致折行 */}
        <table className="w-full min-w-[900px] text-sm">
          <thead>
            <tr className="thead-row whitespace-nowrap text-left text-xs">
              <th className="px-4 py-3">ID</th><th className="px-4 py-3">{t('adminUsers.colEmail')}</th><th className="px-4 py-3">{t('adminUsers.colRole')}</th>
              <th className="px-4 py-3">{t('common.status')}</th><th className="px-4 py-3">Keys</th><th className="px-4 py-3">{t('adminUsers.colProviders')}</th>
              <th className="px-4 py-3">{t('adminUsers.colRequests')}</th><th className="px-4 py-3">Tokens</th><th className="px-4 py-3">{t('adminUsers.colLastActive')}</th>
              <th className="sticky-col px-4 py-3">{t('common.actions')}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((u) => (
              <tr key={u.id} className="tbody-row">
                <td className="px-4 py-2.5 text-muted">{u.id}</td>
                <td className="max-w-[240px] px-4 py-2.5">
                  <div className="truncate">{u.email}</div>
                  {u.name && <div className="truncate text-xs text-muted">{u.name}</div>}
                </td>
                <td className="whitespace-nowrap px-4 py-2.5">
                  {u.role === 'admin' ? <span className="badge-yellow">admin</span> : <span className="badge-zinc">user</span>}
                </td>
                <td className="whitespace-nowrap px-4 py-2.5">
                  {u.status === 'active' ? <span className="badge-green">active</span> : <span className="badge-red">disabled</span>}
                </td>
                <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">{u.api_key_count}</td>
                <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">{u.provider_count}</td>
                <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">{u.total_requests}</td>
                <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums" title={u.total_tokens.toLocaleString()}>{fmtCompact(u.total_tokens)}</td>
                <td className="whitespace-nowrap px-4 py-2.5 text-xs text-muted">
                  {u.last_activity_at ? fmtTime(u.last_activity_at) : '—'}
                </td>
                {/* 操作列：sticky 固定右缘，横向滚动时按钮始终可见 */}
                <td className="sticky-col px-4 py-2.5">
                  <div className="flex items-center gap-1">
                    <button
                      className="btn-icon"
                      title={u.role === 'admin' ? t('adminUsers.demote') : t('adminUsers.promote')}
                      disabled={busyId === u.id}
                      onClick={() => patchUser(u, { role: u.role === 'admin' ? 'user' : 'admin' },
                        u.role === 'admin' ? t('adminUsers.demotedMsg', { email: u.email }) : t('adminUsers.promotedMsg', { email: u.email }))}
                    >
                      {u.role === 'admin' ? <ArrowDownCircle className="h-3.5 w-3.5" /> : <ArrowUpCircle className="h-3.5 w-3.5" />}
                    </button>
                    {u.status === 'active' ? (
                      <button className="btn-icon-danger" title={t('common.disable')} disabled={busyId === u.id}
                        onClick={() => patchUser(u, { status: 'disabled' }, t('adminUsers.disabledMsg', { email: u.email }))}>
                        <Ban className="h-3.5 w-3.5" />
                      </button>
                    ) : (
                      <button className="btn-icon" title={t('common.enable')} disabled={busyId === u.id}
                        onClick={() => patchUser(u, { status: 'active' }, t('adminUsers.enabledMsg', { email: u.email }))}>
                        <CircleCheck className="h-3.5 w-3.5" />
                      </button>
                    )}
                    <button className="btn-icon" title={t('adminUsers.resetPassword')} disabled={busyId === u.id}
                      onClick={() => { setResetTarget(u); setResetPass('') }}>
                      <KeyRound className="h-3.5 w-3.5" />
                    </button>
                  </div>
                </td>
              </tr>
            ))}
            {rows.length === 0 && !loading && (
              <tr><td colSpan={10} className="px-4 py-10 text-center text-muted">{t('adminUsers.noMatch')}</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="mt-3 flex items-center justify-between text-sm text-muted">
        <span>{t('adminUsers.totalUsers', { total })}</span>
        <div className="flex items-center gap-2">
          <button className="btn-ghost" disabled={page <= 1}
            onClick={() => { const p = page - 1; setPage(p); load(p) }}>{t('common.prev')}</button>
          <span>{page} / {pages}</span>
          <button className="btn-ghost" disabled={page >= pages}
            onClick={() => { const p = page + 1; setPage(p); load(p) }}>{t('common.next')}</button>
        </div>
      </div>
      {loading && <div className="mt-2 text-center text-xs text-muted opacity-70">{t('common.loading')}</div>}

      {resetTarget && (
        <Modal onClose={() => setResetTarget(null)} title={t('adminUsers.resetPasswordTitle', { email: resetTarget.email })}>
          {resetPass ? (
            <div className="space-y-3">
              <div className="text-sm text-muted">{t('adminUsers.newPasswordTip')}</div>
              <div className="mono-well flex items-center justify-between rounded-lg px-3 py-2 font-mono text-sm">
                <span>{resetPass}</span>
                <button className="btn-ghost"
                  onClick={async () => { await navigator.clipboard.writeText(resetPass); setCopied(true); setTimeout(() => setCopied(false), 1500) }}>
                  {copied ? '✅ ' + t('common.copied') : t('common.copy')}
                </button>
              </div>
              <button className="btn-primary w-full" onClick={() => setResetTarget(null)}>{t('adminUsers.done')}</button>
            </div>
          ) : (
            <div className="space-y-3">
              <div className="text-sm text-muted">{t('adminUsers.resetConfirmTip')}</div>
              <div className="flex gap-2">
                <button className="btn-danger flex-1" disabled={busyId === resetTarget.id} onClick={() => doReset(resetTarget)}>{t('adminUsers.confirmReset')}</button>
                <button className="btn-ghost flex-1" onClick={() => setResetTarget(null)}>{t('common.cancel')}</button>
              </div>
            </div>
          )}
        </Modal>
      )}
    </div>
  )
}
