import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { RefreshCw } from 'lucide-react'
import { api, type RequestErrorRow } from '@/lib/api'

// provider-errors.tsx 失败请求详情面板（用户端渠道健康 / admin 渠道健康共用）。
//
// 只展示诊断元数据与上游错误摘要（后端已截断且不落 prompt/响应正文）；
// admin 视图走 admin 接口（不带 user_id 过滤），用户视图只取自己的渠道。

// 失败分类 → i18n key 与 badge 样式
const KIND_META: Record<string, { key: string; cls: string }> = {
  no_channel: { key: 'kindNoChannel', cls: 'badge-zinc' },
  circuit_open: { key: 'kindCircuitOpen', cls: 'badge-yellow' },
  credential: { key: 'kindCredential', cls: 'badge-yellow' },
  transform: { key: 'kindTransform', cls: 'badge-yellow' },
  proxy: { key: 'kindProxy', cls: 'badge-yellow' },
  upstream_4xx: { key: 'kindUpstream4xx', cls: 'badge-red' },
  upstream_429: { key: 'kindUpstream429', cls: 'badge-yellow' },
  upstream_5xx: { key: 'kindUpstream5xx', cls: 'badge-red' },
  transport: { key: 'kindTransport', cls: 'badge-red' },
  stream_aborted: { key: 'kindStreamAborted', cls: 'badge-red' },
}

const fmtTime = (iso: string) => {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function ProviderErrorsPanel({ providerId, admin = false }: { providerId: number; admin?: boolean }) {
  const { t } = useTranslation()
  const [rows, setRows] = useState<RequestErrorRow[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  const path = admin ? `/api/admin/providers/${providerId}/errors` : `/api/providers/${providerId}/errors`

  const load = useCallback(() => {
    setLoading(true)
    api.get<RequestErrorRow[]>(`${path}?limit=50`)
      .then((d) => setRows(d ?? []))
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }, [path])

  useEffect(() => { load() }, [load])

  return (
    <div className="px-4 py-3">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs font-medium">{t('requestErrors.title')}</span>
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">{t('requestErrors.hint')}</span>
          <button className="btn-icon" title={t('common.refresh')} onClick={load} disabled={loading}>
            <RefreshCw className="h-3.5 w-3.5" />
          </button>
        </div>
      </div>
      {err && <div className="mb-2 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}
      {loading && rows.length === 0 ? (
        <div className="py-3 text-xs text-muted">{t('common.loading')}</div>
      ) : rows.length === 0 ? (
        <div className="py-3 text-xs text-muted">{t('requestErrors.empty')}</div>
      ) : (
        <div className="overflow-x-auto rounded-lg border" style={{ borderColor: 'var(--line)' }}>
          <table className="w-full min-w-[760px] text-xs">
            <thead>
              <tr className="thead-row whitespace-nowrap text-left">
                <th className="px-3 py-2">{t('requestErrors.colTime')}</th>
                <th className="px-3 py-2">{t('requestErrors.colKind')}</th>
                <th className="px-3 py-2">{t('requestErrors.colStatus')}</th>
                <th className="px-3 py-2">{t('requestErrors.colModel')}</th>
                <th className="px-3 py-2">{t('requestErrors.colKey')}</th>
                <th className="px-3 py-2">{t('requestErrors.colLatency')}</th>
                <th className="px-3 py-2">{t('requestErrors.colMessage')}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((e) => {
                const meta = KIND_META[e.kind]
                return (
                  <tr key={e.id} className="tbody-row align-top">
                    <td className="whitespace-nowrap px-3 py-2 text-muted tabular-nums">{fmtTime(e.created_at)}</td>
                    <td className="whitespace-nowrap px-3 py-2">
                      <span className={meta?.cls ?? 'badge-zinc'}>
                        {meta ? t(`requestErrors.${meta.key}`) : e.kind || '—'}
                      </span>
                      {/* 路由层失败（provider_id=0）不属于任何渠道，标注以免困惑 */}
                      {e.provider_id === 0 && (
                        <span className="ml-1 text-muted">{t('requestErrors.noProvider')}</span>
                      )}
                    </td>
                    <td className="whitespace-nowrap px-3 py-2 text-muted tabular-nums">{e.status_code || '—'}</td>
                    <td className="max-w-[180px] truncate px-3 py-2" title={e.model}>{e.model || '—'}</td>
                    <td className="max-w-[120px] truncate px-3 py-2 text-muted" title={e.api_key_name}>{e.api_key_name || '—'}</td>
                    <td className="whitespace-nowrap px-3 py-2 text-muted tabular-nums">{e.latency_ms} ms</td>
                    <td className="px-3 py-2 break-all">{e.message || '—'}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
