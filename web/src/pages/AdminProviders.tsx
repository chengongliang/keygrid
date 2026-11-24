import { Fragment, useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { RefreshCw, RotateCcw } from 'lucide-react'
import { fmtCompact, Pagination } from '@/components/usage-charts'
import { ProviderErrorsPanel } from '@/components/provider-errors'
import { api, type ProviderHealthRow } from '@/lib/api'

// 熔断状态 → badge 样式/文案（key 用于 t()）
const BREAKER_STYLE: Record<string, { cls: string; key: string }> = {
  closed: { cls: 'badge-green', key: 'breakerClosed' },
  'half-open': { cls: 'badge-yellow', key: 'breakerHalfOpen' },
  open: { cls: 'badge-red', key: 'breakerOpen' },
}

// 30s 轮询：熔断是秒级状态，页面停留时能直接看到自动恢复/半开试探
const POLL_MS = 30_000

export default function AdminProviders() {
  const { t } = useTranslation()
  const [rows, setRows] = useState<ProviderHealthRow[]>([])
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')
  const [onlyAbnormal, setOnlyAbnormal] = useState(false)
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(20)
  const [busyId, setBusyId] = useState(0)
  // 展开的行 = 查看该渠道的最近失败详情（同一时刻只展开一个，避免表格跳动）
  const [expanded, setExpanded] = useState(0)

  const load = useCallback(() => {
    api.get<ProviderHealthRow[]>('/api/admin/providers')
      .then((d) => setRows(d ?? []))
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    load()
    const id = setInterval(load, POLL_MS)
    return () => clearInterval(id)
  }, [load])

  const resetBreaker = async (row: ProviderHealthRow) => {
    setBusyId(row.id)
    setErr('')
    try {
      await api.post(`/api/admin/providers/${row.id}/breaker/reset`)
      setMsg(t('adminProviders.resetDone', { name: row.name }))
      setTimeout(() => setMsg(''), 3000)
      load()
    } catch (e: any) {
      setErr(e.message ?? t('adminProviders.resetFailed'))
    } finally {
      setBusyId(0)
    }
  }

  // 仅看异常 = 熔断中/半开，或近 24h 有失败（与「24h 失败」列同一口径，含 4xx/5xx）
  const query = search.trim().toLowerCase()
  const visible = rows.filter((r) => {
    const abnormal = (r.breaker_check && r.breaker_state && r.breaker_state !== 'closed') || r.fail_24h > 0
    const matches = !query || String(r.id).includes(query)
      || r.name.toLowerCase().includes(query) || r.user_email.toLowerCase().includes(query)
    return (!onlyAbnormal || abnormal) && matches
  })
  const currentPage = Math.min(page, Math.max(0, Math.ceil(visible.length / pageSize) - 1))
  const pagedRows = visible.slice(currentPage * pageSize, (currentPage + 1) * pageSize)
  useEffect(() => { setPage(currentPage) }, [currentPage])

  return (
    <div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-lg font-semibold">{t('adminProviders.title')}</h2>
        <div className="flex items-center gap-3">
          <label
            className="flex cursor-pointer items-center gap-2 text-sm text-muted"
            title={t('adminProviders.onlyAbnormalHint')}
          >
            <input type="checkbox" checked={onlyAbnormal} onChange={(e) => { setOnlyAbnormal(e.target.checked); setPage(0); setExpanded(0) }} />
            {t('adminProviders.onlyAbnormal')}
          </label>
          <button className="btn-ghost flex items-center gap-1.5" onClick={load} disabled={loading}>
            <RefreshCw className="h-3.5 w-3.5" />
            {t('common.refresh')}
          </button>
        </div>
      </div>
      <p className="mb-4 text-xs text-muted">{t('adminProviders.desc')}</p>
      <input
        type="search"
        className="input-sm mb-3 w-full sm:max-w-sm"
        aria-label={t('adminProviders.searchPlaceholder')}
        placeholder={t('adminProviders.searchPlaceholder')}
        value={search}
        onChange={(e) => { setSearch(e.target.value); setPage(0); setExpanded(0) }}
      />

      {err && <div className="mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
      {msg && <div className="mb-3 w-fit rounded-lg px-3 py-2 text-sm badge-green">{msg}</div>}

      <div className="card overflow-x-auto p-0">
        <table className="w-full min-w-[860px] text-sm">
          <thead>
            <tr className="thead-row whitespace-nowrap text-left text-xs">
              <th className="px-4 py-3">ID</th>
              <th className="px-4 py-3">{t('adminProviders.colUser')}</th>
              <th className="px-4 py-3">{t('adminProviders.colChannel')}</th>
              <th className="px-4 py-3">{t('adminProviders.colCred')}</th>
              <th className="px-4 py-3">{t('adminProviders.colBreaker')}</th>
              <th className="px-4 py-3">{t('adminProviders.colReq24h')}</th>
              <th className="px-4 py-3">{t('adminProviders.colFail24h')}</th>
              <th className="px-4 py-3">{t('adminProviders.colSuccessRate')}</th>
              <th className="px-4 py-3">{t('adminProviders.colLatency')}</th>
              <th className="sticky-col px-4 py-3" style={{ boxShadow: 'none' }}>{t('common.actions')}</th>
            </tr>
          </thead>
          <tbody>
            {pagedRows.map((r) => {
              const bs = BREAKER_STYLE[r.breaker_state]
              const abnormal = r.breaker_check && !!bs && r.breaker_state !== 'closed'
              return (
                <Fragment key={r.id}>
                <tr
                  className="tbody-row cursor-pointer"
                  title={t('adminProviders.rowHint')}
                  onClick={() => setExpanded(expanded === r.id ? 0 : r.id)}
                >
                  <td className="px-4 py-2.5 text-muted">{r.id}</td>
                  <td className="max-w-[200px] truncate px-4 py-2.5">{r.user_email}</td>
                  <td className="px-4 py-2.5">
                    <div className="flex items-center gap-2">
                      <span className="max-w-[180px] truncate">{r.name}</span>
                      {!r.enabled && <span className="badge-zinc">{t('common.disabled')}</span>}
                    </div>
                    <div className="text-xs text-muted">{r.kind} · {r.protocol}</div>
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5">
                    <span className={r.cred_status === 'active' ? 'badge-green' : 'badge-zinc'}>
                      {r.cred_status || '—'}
                    </span>
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5">
                    {!r.breaker_check ? (
                      <span className="badge-zinc" title={t('adminProviders.breakerOffTip')}>
                        {t('adminProviders.breakerOff')}
                      </span>
                    ) : bs ? (
                      <span className={bs.cls}>{t(`adminProviders.${bs.key}`)}</span>
                    ) : (
                      '—'
                    )}
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums" title={r.req_24h.toLocaleString()}>
                    {fmtCompact(r.req_24h)}
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 tabular-nums" title={r.fail_24h.toLocaleString()}>
                    <span className={r.fail_24h > 0 ? 'text-red-600 dark:text-red-400' : 'text-muted'}>
                      {fmtCompact(r.fail_24h)}
                    </span>
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">
                    {r.req_24h > 0 ? `${r.success_rate.toFixed(1)}%` : '—'}
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">
                    {r.req_24h > 0 ? `${Math.round(r.avg_latency_ms)} ms` : '—'}
                  </td>
                  {/* 操作列：sticky 固定右缘，横向滚动时按钮始终可见。
                      重置熔断常显，未开启检测 / 未熔断时禁用（tooltip 由外层 span 承载，
                      disabled 按钮自身不触发 hover），只有熔断中才可点。 */}
                  <td className="sticky-col px-4 py-2.5" style={{ boxShadow: 'none' }} onClick={(e) => e.stopPropagation()}>
                    <span
                      className="inline-flex"
                      title={
                        !r.breaker_check
                          ? t('adminProviders.resetBreakerOff')
                          : abnormal
                            ? t('adminProviders.resetBreaker')
                            : t('adminProviders.resetBreakerIdle')
                      }
                    >
                      <button
                        className="btn-icon"
                        aria-label={t('adminProviders.resetBreaker')}
                        disabled={busyId === r.id || !abnormal}
                        onClick={() => resetBreaker(r)}
                      >
                        <RotateCcw className="h-3.5 w-3.5" />
                      </button>
                    </span>
                  </td>
                </tr>
                {expanded === r.id && (
                  <tr>
                    <td colSpan={10} className="p-0" style={{ background: 'color-mix(in oklab, var(--line) 18%, transparent)' }}>
                      <ProviderErrorsPanel providerId={r.id} admin />
                    </td>
                  </tr>
                )}
                </Fragment>
              )
            })}
            {visible.length === 0 && !loading && (
              <tr><td colSpan={10} className="px-4 py-10 text-center text-muted">{t('adminProviders.empty')}</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <Pagination className="mt-3"
        total={visible.length} page={currentPage} pageSize={pageSize}
        onPage={(p) => { setPage(p); setExpanded(0) }}
        onPageSize={(n) => { setPageSize(n); setPage(0); setExpanded(0) }}
      />
      {loading && <div className="mt-2 text-xs text-muted opacity-70">{t('common.loading')}</div>}
    </div>
  )
}
