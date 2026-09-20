import { Fragment, useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { RefreshCw, RotateCcw } from 'lucide-react'
import { api, type ProviderHealthRow } from '@/lib/api'
import { ProviderErrorsPanel } from '@/components/provider-errors'

// providers-health-view.tsx 用户端「健康」视图：以表格一览自己渠道的可用性，
// 快速启停 / 重置熔断 / 展开最近失败详情。
//
// 与 admin 渠道健康总览同一口径（后端复用同一查询），但只含自己的渠道。

// 熔断状态 → badge 样式/i18n key
const BREAKER_STYLE: Record<string, { cls: string; key: string }> = {
  closed: { cls: 'badge-green', key: 'breakerClosed' },
  'half-open': { cls: 'badge-yellow', key: 'breakerHalfOpen' },
  open: { cls: 'badge-red', key: 'breakerOpen' },
}

export function ProvidersHealthView({ onChanged }: { onChanged?: () => void }) {
  const { t } = useTranslation()
  const [rows, setRows] = useState<ProviderHealthRow[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [busyId, setBusyId] = useState(0)
  const [expanded, setExpanded] = useState(0)

  const load = useCallback(() => {
    setLoading(true)
    api.get<ProviderHealthRow[]>('/api/providers/health')
      .then((d) => setRows(d ?? []))
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => { load() }, [load])

  // 快速启停：未授权的 oauth 渠道后端会拒绝（无凭据无法转发），前端先禁用开关
  const toggle = async (row: ProviderHealthRow) => {
    setErr('')
    setBusyId(row.id)
    try {
      await api.patch(`/api/providers/${row.id}`, { enabled: !row.enabled })
      load()
      onChanged?.()
    } catch (e: any) {
      setErr(e.message ?? t('providers.healthToggleFailed'))
    } finally {
      setBusyId(0)
    }
  }

  const resetBreaker = async (row: ProviderHealthRow) => {
    setErr('')
    setBusyId(row.id)
    try {
      await api.post(`/api/providers/${row.id}/breaker/reset`)
      setMsg(t('providers.healthResetDone', { name: row.name }))
      setTimeout(() => setMsg(''), 3000)
      load()
    } catch (e: any) {
      setErr(e.message ?? t('providers.healthResetFailed'))
    } finally {
      setBusyId(0)
    }
  }

  const unauthorized = (r: ProviderHealthRow) => r.kind === 'oauth' && r.cred_status !== 'active'
  const abnormal = (r: ProviderHealthRow) => r.breaker_check && !!r.breaker_state && r.breaker_state !== 'closed'

  return (
    <div>
      <p className="mb-3 text-xs text-muted">{t('providers.healthDesc')}</p>
      {err && <div className="mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
      {msg && <div className="mb-3 w-fit rounded-lg px-3 py-2 text-sm badge-green">{msg}</div>}

      <div className="card overflow-x-auto p-0">
        <table className="w-full min-w-[820px] text-sm">
          <thead>
            <tr className="thead-row whitespace-nowrap text-left text-xs">
              <th className="px-4 py-3">ID</th>
              <th className="px-4 py-3">{t('providers.healthColChannel')}</th>
              <th className="px-4 py-3">{t('providers.healthColCred')}</th>
              <th className="px-4 py-3">{t('providers.healthColBreaker')}</th>
              <th className="px-4 py-3">{t('providers.healthColReq24h')}</th>
              <th className="px-4 py-3">{t('providers.healthColFail24h')}</th>
              <th className="px-4 py-3">{t('providers.healthColSuccess')}</th>
              <th className="px-4 py-3">{t('providers.healthColLatency')}</th>
              <th className="px-4 py-3">{t('common.actions')}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => {
              const bs = BREAKER_STYLE[r.breaker_state]
              return (
                <Fragment key={r.id}>
                  <tr
                    className="tbody-row cursor-pointer"
                    onClick={() => setExpanded(expanded === r.id ? 0 : r.id)}
                    title={t('providers.healthRowHint')}
                  >
                    <td className="px-4 py-2.5 text-muted">{r.id}</td>
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
                        <span className="badge-zinc" title={t('providers.breakerCheckHint')}>{t('providers.breakerOff')}</span>
                      ) : bs ? (
                        <span className={bs.cls}>{t(`providers.${bs.key}`)}</span>
                      ) : '—'}
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">{r.req_24h.toLocaleString()}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 tabular-nums">
                      <span className={r.fail_24h > 0 ? 'text-red-600 dark:text-red-400' : 'text-muted'}>
                        {r.fail_24h.toLocaleString()}
                      </span>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">
                      {r.req_24h > 0 ? `${r.success_rate.toFixed(1)}%` : '—'}
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted tabular-nums">
                      {r.req_24h > 0 ? `${Math.round(r.avg_latency_ms)} ms` : '—'}
                    </td>
                    {/* 操作列：启停 + （熔断中时）重置；阻止冒泡避免误触发展开行 */}
                    <td className="whitespace-nowrap px-4 py-2.5" onClick={(e) => e.stopPropagation()}>
                      <div className="flex items-center gap-1.5">
                        <button
                          className="btn-ghost"
                          disabled={busyId === r.id || unauthorized(r)}
                          title={unauthorized(r) ? t('providers.healthUnauthorized') : undefined}
                          onClick={() => toggle(r)}
                        >
                          {r.enabled ? t('common.disable') : t('common.enable')}
                        </button>
                        {/* 重置熔断：常显，未开启检测 / 未熔断时禁用（tooltip 说明原因） */}
                        <span
                          className="inline-flex"
                          title={
                            !r.breaker_check
                              ? t('providers.healthResetOff')
                              : abnormal(r)
                                ? t('providers.healthReset')
                                : t('providers.healthResetIdle')
                          }
                        >
                          <button
                            className="btn-ghost"
                            disabled={busyId === r.id || !abnormal(r)}
                            onClick={() => resetBreaker(r)}
                          >
                            <RotateCcw className="h-3.5 w-3.5" />
                            {t('providers.healthReset')}
                          </button>
                        </span>
                      </div>
                    </td>
                  </tr>
                  {expanded === r.id && (
                    <tr>
                      <td colSpan={9} className="p-0" style={{ background: 'color-mix(in oklab, var(--line) 18%, transparent)' }}>
                        <ProviderErrorsPanel providerId={r.id} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
            {rows.length === 0 && !loading && (
              <tr><td colSpan={9} className="px-4 py-10 text-center text-muted">{t('providers.healthEmpty')}</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="mt-3 flex items-center justify-between text-sm text-muted">
        <span>{t('providers.healthTotal', { total: rows.length })}</span>
        <button className="btn-ghost flex items-center gap-1.5" onClick={load} disabled={loading}>
          <RefreshCw className="h-3.5 w-3.5" />
          {t('common.refresh')}
        </button>
      </div>
    </div>
  )
}
