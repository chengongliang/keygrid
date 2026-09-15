import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, ArrowDownToLine, ArrowUpFromLine, AlertTriangle, DollarSign, Layers, RefreshCw, Users2 } from 'lucide-react'
import { api, type PlatformUsageAggRow, type TopUserRow, type UsageHourRow } from '@/lib/api'
import {
  COLOR_IN, COLOR_OUT, PALETTE,
  Donut, HourHeatmap, KpiCard, LegendDot, LegendList, Pagination, RangeBar, Segmented, StackedBarChart,
  dayLabel, fmtCompact, fmtInt, pctDelta, prevRange, presetRange, rangeLabel,
  type RangeSel,
} from '@/components/usage-charts'

// label 存语义 key，渲染处 t()（模块级常量无法调用 hook）
const DIMS = [
  { key: 'day', label: 'adminUsage.byDay' },
  { key: 'user', label: 'adminUsage.byUser' },
  { key: 'model', label: 'adminUsage.byModel' },
]

const METRIC_OPTS = [
  { key: 'tokens', label: 'adminUsage.metricTokens' },
  { key: 'requests', label: 'adminUsage.requests' },
]

const qs = (r: { from: string; to: string }) =>
  `from=${encodeURIComponent(r.from)}&to=${encodeURIComponent(r.to)}`

export default function AdminUsage() {
  const { t } = useTranslation()
  const [rows, setRows] = useState<PlatformUsageAggRow[]>([])
  const [dayRows, setDayRows] = useState<PlatformUsageAggRow[]>([])
  const [prevRows, setPrevRows] = useState<PlatformUsageAggRow[]>([])
  const [modelRows, setModelRows] = useState<PlatformUsageAggRow[]>([])
  const [top, setTop] = useState<TopUserRow[]>([])
  const [hourRows, setHourRows] = useState<UsageHourRow[]>([])
  const [range, setRange] = useState<RangeSel>(() => ({ key: '7d', ...presetRange('7d') }))
  const [by, setBy] = useState('day')
  const [trendMetric, setTrendMetric] = useState('tokens')
  const [heatMetric, setHeatMetric] = useState<'tokens' | 'requests'>('tokens')
  const [donutMetric, setDonutMetric] = useState('tokens')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  // 手动刷新：递增 tick 重拉全部数据
  const [refreshTick, setRefreshTick] = useState(0)
  // 最近一次数据成功加载时间（刷新反馈：按钮旁展示「更新于 HH:MM:SS」）
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null)
  // 明细表分页（数据已全量在前端，纯前端切片）；维度/区间变化时回到第一页
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(20)
  useEffect(() => { setPage(0) }, [rows, by])
  const pagedRows = rows.slice(page * pageSize, page * pageSize + pageSize)
  // 常量选项的 label 翻译成当前语言（Segmented 只接受 string label）
  const dimOpts = DIMS.map((d) => ({ key: d.key, label: t(d.label) }))
  const metricOpts = METRIC_OPTS.map((m) => ({ key: m.key, label: t(m.label) }))

  useEffect(() => {
    setLoading(true)
    setErr('')
    // 明细随 by 切换；趋势/KPI 固定走 by=day；环比 = 等长前移周期；
    // 模型分布固定 by=model，任何维度下都可见。
    const p = qs(range), pp = qs(prevRange(range))
    Promise.all([
      api.get<PlatformUsageAggRow[]>(`/api/admin/usage?by=${by}&${p}`),
      api.get<PlatformUsageAggRow[]>(`/api/admin/usage?by=day&${p}`),
      api.get<PlatformUsageAggRow[]>(`/api/admin/usage?by=day&${pp}`),
      api.get<PlatformUsageAggRow[]>(`/api/admin/usage?by=model&${p}`),
      api.get<TopUserRow[]>(`/api/admin/usage/top?limit=8&${p}`),
      api.get<UsageHourRow[]>(`/api/admin/usage/hourly?${p}`),
    ])
      .then(([a, day, prevDay, models, t, hr]) => {
        setRows(a ?? [])
        setDayRows(day ?? [])
        setPrevRows(prevDay ?? [])
        setModelRows(models ?? [])
        setTop(t ?? [])
        setHourRows(hr ?? [])
        setLastUpdated(new Date())
      })
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }, [by, range, refreshTick])

  const cur = useMemo(() => sumUp(dayRows), [dayRows])
  const prev = useMemo(() => sumUp(prevRows), [prevRows])
  const delta = (k: keyof ReturnType<typeof sumUp>) => pctDelta(cur[k], prev[k])
  const prevLabel = rangeLabel(prevRange(range))

  const failRate = cur.requests > 0 ? (cur.failed / cur.requests) * 100 : 0

  const trend = useMemo(() => {
    const m = new Map<string, { prompt: number; completion: number; requests: number }>()
    for (const r of dayRows) {
      const v = m.get(r.day) ?? { prompt: 0, completion: 0, requests: 0 }
      v.prompt += r.prompt_tokens
      v.completion += r.completion_tokens
      v.requests += r.requests
      m.set(r.day, v)
    }
    return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([d, v]) => ({
      label: dayLabel(d),
      segs: trendMetric === 'tokens'
        ? [
            { key: 'in', label: t('adminUsage.labelIn'), value: v.prompt, color: COLOR_IN },
            { key: 'out', label: t('adminUsage.labelOut'), value: v.completion, color: COLOR_OUT },
          ]
        : [{ key: 'req', label: t('adminUsage.requests'), value: v.requests, color: COLOR_OUT }],
    }))
  }, [dayRows, trendMetric, t])

  // 模型分布（Top7 + 其他）
  const modelDonut = useMemo(() => {
    const all = modelRows
      .map((r) => ({ label: r.model, tokens: r.prompt_tokens + r.completion_tokens, requests: r.requests }))
      .sort((a, b) => b.tokens - a.tokens)
    const pick = (x: { tokens: number; requests: number }) => (donutMetric === 'tokens' ? x.tokens : x.requests)
    const top = all.slice(0, 7)
    const rest = all.slice(7).reduce((s, x) => s + pick(x), 0)
    const data = top.map((x, i) => ({ label: x.label, value: pick(x), color: PALETTE[i % PALETTE.length] }))
    if (rest > 0) data.push({ label: t('adminUsage.other'), value: rest, color: '#a1a1aa' })
    return data
  }, [modelRows, donutMetric, t])

  // TOP 用户（Top6 + 其他）
  const topDonut = useMemo(() => {
    const pick = (t: TopUserRow) => (donutMetric === 'tokens' ? t.prompt_tokens + t.completion_tokens : t.requests)
    const data = top.slice(0, 6).map((t, i) => ({
      label: t.email || `user#${t.user_id}`,
      value: pick(t),
      color: PALETTE[i % PALETTE.length],
    }))
    const rest = top.slice(6).reduce((s, t) => s + pick(t), 0)
    if (rest > 0) data.push({ label: t('adminUsage.other'), value: rest, color: '#a1a1aa' })
    return data
  }, [top, donutMetric, t])

  const exportCsv = () => {
    const token = localStorage.getItem('token') ?? ''
    // fetch 带 token 下载（不能直接 <a href>，admin API 需鉴权）
    fetch(`/api/admin/usage/export?by=${by}&${qs(range)}`, { headers: { Authorization: `Bearer ${token}` } })
      .then((res) => { if (!res.ok) throw new Error(t('adminUsage.exportFailed', { code: res.status })); return res.blob() })
      .then((blob) => {
        const url = URL.createObjectURL(blob)
        const a = document.createElement('a')
        a.href = url
        a.download = `usage_${by}.csv`
        a.click()
        URL.revokeObjectURL(url)
      })
      .catch((e) => setErr(e.message))
  }

  const rowLabel = (r: PlatformUsageAggRow) => {
    if (by === 'user') return r.email || `user#${r.user_id}`
    if (by === 'model') return r.model
    return dayLabel(r.day)
  }

  const donutCenterValue = donutMetric === 'tokens' ? fmtCompact(cur.tokens) : fmtInt(cur.requests)

  return (
    <div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-lg font-semibold">{t('adminUsage.title')}</h2>
        <div className="flex items-center gap-2">
          <RangeBar value={range} onChange={setRange} />
          {lastUpdated && (
            <span className="hidden whitespace-nowrap text-xs text-muted sm:inline">
              {t('usage.updatedAt', { time: lastUpdated.toLocaleTimeString() })}
            </span>
          )}
          <button
            className="btn-ghost shrink-0"
            disabled={loading}
            onClick={() => setRefreshTick((n) => n + 1)}
            title={t('common.refresh')}
          >
            <RefreshCw className={`h-3.5 w-3.5 ${loading ? 'animate-spin' : ''}`} />
            {t('common.refresh')}
          </button>
        </div>
      </div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <Segmented options={dimOpts} value={by} onChange={setBy} />
        <button className="btn-ghost" onClick={exportCsv}>{t('adminUsage.exportCsv')}</button>
      </div>
      {err && <div className="err-well mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">{err}</div>}

      {/* 数据区：手动刷新时重挂载触发淡入动画，让“刷新生效”可见 */}
      <div key={refreshTick} className="data-refresh">
      {/* KPI 卡片阵列 */}
      <div className="mb-4 grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-6">
        <KpiCard icon={<Activity className="h-3.5 w-3.5" />} label={t('adminUsage.requests')} value={fmtInt(cur.requests)} delta={delta('requests')} prevLabel={prevLabel}
          hint={t('adminUsage.kpi.requestsHint')} />
        <KpiCard icon={<AlertTriangle className="h-3.5 w-3.5" />} label={t('adminUsage.kpi.failRate')}
          value={cur.requests > 0 ? failRate.toFixed(1) + '%' : '—'}
          sub={cur.failed > 0 ? t('adminUsage.kpi.failedCount', { n: fmtInt(cur.failed) }) : t('adminUsage.kpi.allSuccess')}
          hint={t('adminUsage.kpi.failRateHint')} />
        <KpiCard icon={<Layers className="h-3.5 w-3.5" />} label={t('adminUsage.kpi.totalTokens')} value={fmtCompact(cur.tokens)} valueTitle={fmtInt(cur.tokens)} delta={delta('tokens')} prevLabel={prevLabel}
          hint={t('adminUsage.kpi.totalTokensHint')} />
        <KpiCard icon={<ArrowDownToLine className="h-3.5 w-3.5" />} label={t('adminUsage.inTokens')} value={fmtCompact(cur.prompt)} valueTitle={fmtInt(cur.prompt)}
          sub={t('adminUsage.kpi.share', { p: cur.tokens > 0 ? ((cur.prompt / cur.tokens) * 100).toFixed(1) : '0.0' })} delta={delta('prompt')} prevLabel={prevLabel}
          hint={t('adminUsage.kpi.inTokensHint')} />
        <KpiCard icon={<ArrowUpFromLine className="h-3.5 w-3.5" />} label={t('adminUsage.outTokens')} value={fmtCompact(cur.completion)} valueTitle={fmtInt(cur.completion)}
          sub={t('adminUsage.kpi.share', { p: cur.tokens > 0 ? ((cur.completion / cur.tokens) * 100).toFixed(1) : '0.0' })} delta={delta('completion')} prevLabel={prevLabel}
          hint={t('adminUsage.kpi.outTokensHint')} />
        <KpiCard icon={<DollarSign className="h-3.5 w-3.5" />} label={t('adminUsage.kpi.cost')} value={fmtMoney(cur.cost)} delta={delta('cost')} prevLabel={prevLabel}
          hint={t('adminUsage.kpi.costHint')} />
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-5">
        {/* 每日趋势 */}
        <div className="card lg:col-span-3">
          <div className="mb-3 flex items-center justify-between gap-2">
            <h3 className="flex items-center gap-3 text-sm font-medium">
              {t('adminUsage.trend.title')}
              <span className="flex items-center gap-3">
                {trendMetric === 'tokens'
                  ? (<>
                      <LegendDot color={COLOR_IN} label={t('adminUsage.labelIn')} hint={t('adminUsage.trend.inHint')} />
                      <LegendDot color={COLOR_OUT} label={t('adminUsage.labelOut')} hint={t('adminUsage.trend.outHint')} />
                    </>)
                  : <LegendDot color={COLOR_OUT} label={t('adminUsage.requests')} hint={t('adminUsage.trend.reqHint')} />}
              </span>
            </h3>
            <Segmented options={metricOpts} value={trendMetric} onChange={setTrendMetric} />
          </div>
          {trend.length === 0 ? <Empty /> : <StackedBarChart data={trend} />}
        </div>

        {/* 模型分布 */}
        <div className="card lg:col-span-2">
          <div className="mb-3 flex items-center justify-between gap-2">
            <h3 className="text-sm font-medium">{t('adminUsage.donut.title')}</h3>
            <Segmented options={metricOpts} value={donutMetric} onChange={setDonutMetric} />
          </div>
          {modelDonut.length === 0 ? <Empty /> : (
            <div className="flex items-center gap-4">
              <Donut data={modelDonut} centerValue={donutCenterValue}
                centerLabel={donutMetric === 'tokens' ? 'Tokens' : t('adminUsage.centerRequests')} />
              <LegendList data={modelDonut} />
            </div>
          )}
        </div>
      </div>

      <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-5">
        {/* TOP 用户 */}
        <div className="card lg:col-span-2">
          <div className="mb-3 flex items-center gap-2">
            <Users2 className="h-3.5 w-3.5 text-muted" />
            <h3 className="text-sm font-medium">{t('adminUsage.top.title')}</h3>
          </div>
          {topDonut.length === 0 ? <Empty /> : (
            <div className="flex items-center gap-4">
              <Donut data={topDonut} centerValue={donutCenterValue}
                centerLabel={donutMetric === 'tokens' ? 'Tokens' : t('adminUsage.centerRequests')} />
              <LegendList data={topDonut} />
            </div>
          )}
        </div>

        {/* 分时活跃热力图 */}
        <div className="card lg:col-span-3">
          <div className="mb-3 flex items-center justify-between gap-2">
            <h3 className="text-sm font-medium">{t('adminUsage.heat.title')}</h3>
            <Segmented options={metricOpts} value={heatMetric} onChange={(k) => setHeatMetric(k as 'tokens' | 'requests')} />
          </div>
          <HourHeatmap rows={hourRows} metric={heatMetric} />
        </div>
      </div>

      {/* 明细表 */}
      <div className="card mt-4 overflow-x-auto p-0">
        <table className="w-full text-sm">
          <thead>
            <tr className="thead-row text-left text-xs">
              <th className="px-4 py-3">{by === 'user' ? t('adminUsage.colUser') : by === 'model' ? t('adminUsage.colModel') : t('adminUsage.colDate')}</th>
              <th className="px-4 py-3 text-right">{t('adminUsage.requests')}</th>
              <th className="px-4 py-3 text-right">{t('adminUsage.colFailed')}</th>
              <th className="px-4 py-3 text-right">{t('adminUsage.inTokens')}</th>
              <th className="px-4 py-3 text-right">{t('adminUsage.outTokens')}</th>
              <th className="px-4 py-3 text-right">{t('adminUsage.colCost')}</th>
              <th className="px-4 py-3 text-right">{t('adminUsage.colAvgLatency')}</th>
            </tr>
          </thead>
          <tbody>
            {pagedRows.map((r, i) => (
              <tr key={i} className="tbody-row">
                <td className="max-w-64 cursor-default truncate px-4 py-2.5" title={rowLabel(r)}>{rowLabel(r)}</td>
                <td className="px-4 py-2.5 text-right tabular-nums">{r.requests}</td>
                <td className={'px-4 py-2.5 text-right tabular-nums ' + (r.failed_requests > 0 ? 'font-medium text-red-500' : 'text-muted')}>{r.failed_requests}</td>
                <td className="px-4 py-2.5 text-right tabular-nums text-muted" title={fmtInt(r.prompt_tokens)}>{fmtCompact(r.prompt_tokens)}</td>
                <td className="px-4 py-2.5 text-right tabular-nums text-muted" title={fmtInt(r.completion_tokens)}>{fmtCompact(r.completion_tokens)}</td>
                <td className="px-4 py-2.5 text-right tabular-nums font-medium">{fmtMoney(r.cost)}</td>
                <td className="px-4 py-2.5 text-right tabular-nums text-muted">{Math.round(r.avg_latency_ms)}ms</td>
              </tr>
            ))}
            {rows.length === 0 && !loading && (
              <tr><td colSpan={7} className="px-4 py-10 text-center text-muted">{t('adminUsage.noDataInRange')}</td></tr>
            )}
          </tbody>
        </table>
      </div>
      {/* 分页条放在 overflow 容器外，避免随表格横向滚动 */}
      {rows.length > 0 && (
        <Pagination className="mt-2"
          total={rows.length} page={page} pageSize={pageSize}
          onPage={setPage}
          onPageSize={(n) => { setPageSize(n); setPage(0) }} />
      )}
      </div>
      {loading && <div className="mt-2 text-center text-xs text-muted opacity-70">{t('common.loading')}</div>}
    </div>
  )
}

function Empty() {
  const { t } = useTranslation()
  return <div className="py-10 text-center text-sm text-muted opacity-70">{t('common.noData')}</div>
}

function sumUp(rows: PlatformUsageAggRow[]) {
  let requests = 0, failed = 0, prompt = 0, completion = 0, cost = 0, latencySum = 0
  for (const r of rows) {
    requests += r.requests
    failed += r.failed_requests
    prompt += r.prompt_tokens
    completion += r.completion_tokens
    cost += r.cost ?? 0
    latencySum += r.avg_latency_ms * r.requests
  }
  return {
    requests, failed, prompt, completion, cost,
    tokens: prompt + completion,
    avgLatency: requests > 0 ? latencySum / requests : 0,
  }
}

// fmtMoney 费用展示：金额量级小（单请求 $0.00x 级），保留 4 位并去尾零；≥$100 保留 2 位
// 非有限值（后端旧版缺 cost 字段 / 接口失败）兜底为 0，禁止白屏
function fmtMoney(n: number) {
  const x = Number.isFinite(n) ? n : 0
  const v = Math.abs(x) >= 100 ? x.toFixed(2) : x.toFixed(4)
  return '$' + v.replace(/0+$/, '').replace(/\.$/, '')
}
