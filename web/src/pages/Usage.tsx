import { useEffect, useMemo, useState } from 'react'
import { Activity, ArrowDownToLine, ArrowUpFromLine, ChevronDown, DollarSign, Gauge, KeyRound, Layers, RefreshCw, Server, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import ProviderLogo from '@/components/ProviderLogo'
import { api, type ApiKey, type UsageAggRow, type UsageByKeyRow, type UsageByModelRow, type UsageByProviderRow, type UsageCostSummary, type UsageHourRow } from '@/lib/api'
import {
  COLOR_IN, COLOR_OUT, PALETTE,
  Donut, HourHeatmap, KpiCard, LegendDot, LegendList, Pagination, RangeBar, Segmented, StackedBarChart,
  dayLabel, fmtCompact, fmtInt, pctDelta, prevRange, presetRange, rangeLabel,
  type RangeSel,
} from '@/components/usage-charts'

// 组装查询串：from/to + 可选 key_id/provider_id/model（0 / '' 表示该维度不筛选）。
// by-key / by-provider 聚合接口通过 skip 对应维度避免"筛选后只剩一行"。
const qs = (r: { from: string; to: string }, f?: { keyId?: number; providerId?: number; model?: string }) => {
  const p = new URLSearchParams({ from: r.from, to: r.to })
  if (f?.keyId) p.set('key_id', String(f.keyId))
  if (f?.providerId) p.set('provider_id', String(f.providerId))
  if (f?.model) p.set('model', f.model)
  return p.toString()
}

export default function Usage() {
  const { t } = useTranslation()
  // 指标切换选项（「请求数」需翻译，Token 为专有名词不译）
  const metricOpts = [
    { key: 'tokens', label: 'Token' },
    { key: 'requests', label: t('usage.requests') },
  ]
  const [rows, setRows] = useState<UsageAggRow[]>([])
  const [prevRows, setPrevRows] = useState<UsageAggRow[]>([])
  const [hourRows, setHourRows] = useState<UsageHourRow[]>([])
  const [byKeyRows, setByKeyRows] = useState<UsageByKeyRow[]>([])
  const [byModelRows, setByModelRows] = useState<UsageByModelRow[]>([])
  const [byProviderRows, setByProviderRows] = useState<UsageByProviderRow[]>([])
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [summary, setSummary] = useState<UsageCostSummary | null>(null)
  const [range, setRange] = useState<RangeSel>(() => ({ key: '7d', ...presetRange('7d') }))
  // 筛选维度：0 / '' = 全部
  const [keyId, setKeyId] = useState(0)
  const [providerId, setProviderId] = useState(0)
  const [model, setModel] = useState('')
  const [trendMetric, setTrendMetric] = useState('tokens')
  const [donutMetric, setDonutMetric] = useState('tokens')
  const [heatMetric, setHeatMetric] = useState<'tokens' | 'requests'>('tokens')
  // 渠道用量表默认折叠（表格较高，展开后才会占据中间空间；点头部展开/收起）
  const [providerOpen, setProviderOpen] = useState(false)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  // 手动刷新：递增 tick 重拉全部数据（含费用汇总与 Key 下拉）
  const [refreshTick, setRefreshTick] = useState(0)
  // 最近一次数据成功加载时间（刷新反馈：按钮旁展示「更新于 HH:MM:SS」）
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null)
  // 明细表分页（数据已全量在前端，纯前端切片）；区间/筛选变化时回到第一页
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(20)
  useEffect(() => { setPage(0) }, [rows])
  const pagedRows = rows.slice(page * pageSize, page * pageSize + pageSize)
  // 渠道用量表分页（档位含小容量：默认折叠场景下 5/10 更紧凑）
  const [providerPage, setProviderPage] = useState(0)
  const [providerPageSize, setProviderPageSize] = useState(10)
  useEffect(() => { setProviderPage(0) }, [byProviderRows])
  const providerPagedRows = byProviderRows.slice(providerPage * providerPageSize, (providerPage + 1) * providerPageSize)
  // Key 用量表与渠道表同款：默认折叠 + 前端切片分页
  const [keyOpen, setKeyOpen] = useState(false)
  const [keyPage, setKeyPage] = useState(0)
  const [keyPageSize, setKeyPageSize] = useState(10)
  useEffect(() => { setKeyPage(0) }, [byKeyRows])
  const keyPagedRows = byKeyRows.slice(keyPage * keyPageSize, (keyPage + 1) * keyPageSize)

  // Key 下拉选项数据（含禁用/过期的 key —— 历史用量仍然要能看）
  useEffect(() => {
    api.get<ApiKey[]>('/api/keys').then((d) => setKeys(d ?? [])).catch(() => {})
  }, [refreshTick])

  // 费用汇总（累计总费用 / 近 30 天）：只展示不拦截 —— 额度硬限只在 key 级
  useEffect(() => {
    api.get<UsageCostSummary>('/api/usage/summary').then((s) => setSummary(s)).catch(() => {})
  }, [refreshTick])

  const filtered = keyId > 0 || providerId > 0 || model !== ''

  useEffect(() => {
    setLoading(true)
    setErr('')
    // 当前区间 + 等长前移的上一周期（环比）+ 分时活跃 + 按 Key/按模型/按渠道汇总，全部带上筛选条件
    const f = { keyId, providerId, model }
    Promise.all([
      api.get<UsageAggRow[]>(`/api/usage?${qs(range, f)}`),
      api.get<UsageAggRow[]>(`/api/usage?${qs(prevRange(range), f)}`),
      api.get<UsageHourRow[]>(`/api/usage/hourly?${qs(range, f)}`),
      // by-key：不按 key 切（否则只剩一行）→ 某模型在各 Key 上的用量分布
      api.get<UsageByKeyRow[]>(`/api/usage/by-key?${qs(range, { providerId, model })}`),
      // by-model：不按模型切 → 某 Key 的模型分布；模型下拉选项也取自这里
      api.get<UsageByModelRow[]>(`/api/usage/by-model?${qs(range, { keyId })}`),
      // by-provider：不按渠道切（否则只剩一行）→ 某 Key/模型在渠道间的分布
      api.get<UsageByProviderRow[]>(`/api/usage/by-provider?${qs(range, { keyId, model })}`),
    ])
      .then(([cur, prev, hr, bk, bm, bp]) => {
        setRows(cur ?? [])
        setPrevRows(prev ?? [])
        setHourRows(hr ?? [])
        setByKeyRows(bk ?? [])
        setByModelRows(bm ?? [])
        setByProviderRows(bp ?? [])
        setLastUpdated(new Date())
      })
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }, [range, keyId, providerId, model, refreshTick])

  // 点击维度行筛选后滚回页面顶部：表格在页面底部，筛选效果发生在上方，滚过去让变化立刻可见
  const scrollToCharts = () => {
    document.getElementById('usage-top')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

  // Key 下拉选项：现有 Key + 区间内出现过但已删除的 Key（by-key 兜底）
  const keyOptions = useMemo(() => {
    const opts = keys.map((k) => ({ id: k.id, label: k.name ? `${k.name} · ${k.prefix}…` : `${k.prefix}…` }))
    const seen = new Set(keys.map((k) => k.id))
    for (const r of byKeyRows) {
      if (!seen.has(r.api_key_id)) opts.push({ id: r.api_key_id, label: t('usage.deletedKeyId', { id: r.api_key_id }) })
    }
    return opts.sort((a, b) => a.id - b.id)
  }, [keys, byKeyRows, t])

  // 明细表 Key 列的 id → 展示信息映射（已删除的 Key 标记出来）
  const keyInfoMap = useMemo(() => {
    const m = new Map<number, { name: string; prefix: string; deleted: boolean }>()
    for (const k of keys) m.set(k.id, { name: k.name || '', prefix: k.prefix, deleted: false })
    for (const r of byKeyRows) {
      if (!m.has(r.api_key_id)) m.set(r.api_key_id, { name: '', prefix: r.key_prefix || '', deleted: true })
    }
    return m
  }, [keys, byKeyRows])

  const keyDisplayName = (id: number): string => {
    const info = keyInfoMap.get(id)
    if (!info) return t('usage.keyId', { id })
    return info.name || (info.prefix ? `${info.prefix}…` : t('usage.keyId', { id }))
  }

  // 渠道名展示：已删除渠道回退「已删除渠道 #id」
  const providerDisplayName = (r: UsageByProviderRow): string =>
    r.name || t('usage.deletedProviderId', { id: r.provider_id })

  // ---- 汇总（当前 / 上一周期）----
  const cur = useMemo(() => sumUp(rows), [rows])
  const prev = useMemo(() => sumUp(prevRows), [prevRows])
  const delta = (k: keyof ReturnType<typeof sumUp>) => pctDelta(cur[k], prev[k])
  const prevLabel = rangeLabel(prevRange(range))

  // ---- 每日趋势（输入/输出堆叠）----
  const trend = useMemo(() => {
    const m = new Map<string, { prompt: number; completion: number; requests: number }>()
    for (const r of rows) {
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
            { key: 'in', label: t('usage.input'), value: v.prompt, color: COLOR_IN },
            { key: 'out', label: t('usage.output'), value: v.completion, color: COLOR_OUT },
          ]
        : [{ key: 'req', label: t('usage.requests'), value: v.requests, color: COLOR_OUT }],
    }))
  }, [rows, trendMetric, t])

  // ---- 模型分布（Top7 + 其他）----
  const donutData = useMemo(() => {
    const m = new Map<string, { tokens: number; requests: number }>()
    for (const r of rows) {
      const v = m.get(r.model) ?? { tokens: 0, requests: 0 }
      v.tokens += r.prompt_tokens + r.completion_tokens
      v.requests += r.requests
      m.set(r.model, v)
    }
    const all = [...m.entries()].sort((a, b) => b[1].tokens - a[1].tokens)
    const pick = (key: 'tokens' | 'requests') => (e: [string, { tokens: number; requests: number }]) => e[1][key]
    const top = all.slice(0, 7)
    const rest = all.slice(7).reduce((s, e) => s + pick(donutMetric as 'tokens' | 'requests')(e), 0)
    const data = top.map(([label, v], i) => ({ label, value: v[donutMetric as 'tokens' | 'requests'], color: PALETTE[i % PALETTE.length] }))
    if (rest > 0) data.push({ label: t('usage.other'), value: rest, color: '#a1a1aa' })
    return data
  }, [rows, donutMetric, t])

  // ---- Key 用量表：总 Tokens 占比 ----
  const keyTotalTokens = useMemo(() => byKeyRows.reduce((s, r) => s + r.prompt_tokens + r.completion_tokens, 0), [byKeyRows])

  // ---- 渠道用量表：总 Tokens 占比 ----
  const providerTotalTokens = useMemo(() => byProviderRows.reduce((s, r) => s + r.prompt_tokens + r.completion_tokens, 0), [byProviderRows])

  const total = cur.prompt + cur.completion
  const filterDesc = [
    keyId > 0 ? t('usage.filterKey', { name: keyDisplayName(keyId) }) : '',
    providerId > 0 ? t('usage.filterProvider', { name: byProviderRows.find((r) => r.provider_id === providerId)?.name || t('usage.providerIdKey', { id: providerId }) }) : '',
    model ? t('usage.filterModel', { model }) : '',
  ].filter(Boolean).join(' · ')

  return (
    <div id="usage-top">
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-3">
          <h2 className="text-lg font-semibold">{t('usage.title')}</h2>
          {summary && (
            <span className="badge badge-green" title={t('usage.totalCostTitle')}>
              <DollarSign className="h-3 w-3" /> {t('usage.totalCost', { amount: fmtMoney(summary.total_cost) })}
            </span>
          )}
        </div>
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

      {/* 筛选栏：按 Key / 按模型 */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <span className="flex items-center gap-1.5 text-xs text-muted"><KeyRound className="h-3.5 w-3.5" /> Key</span>
        <select value={keyId} onChange={(e) => setKeyId(Number(e.target.value))} className="input-sm max-w-52">
          <option value={0}>{t('usage.allKeys')}</option>
          {keyOptions.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
        </select>
        <span className="ml-2 text-xs text-muted">{t('common.model')}</span>
        <select value={model} onChange={(e) => setModel(e.target.value)} className="input-sm max-w-52">
          <option value="">{t('usage.allModels')}</option>
          {byModelRows.map((m) => <option key={m.model} value={m.model}>{m.model}</option>)}
        </select>
        {filtered && (
          <button className="btn-ghost h-6" onClick={() => { setKeyId(0); setProviderId(0); setModel('') }}>
            <X size={12} /> {t('usage.clearFilters')}
          </button>
        )}
      </div>

      {err && <div className="err-well mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">{err}</div>}

      {rows.length === 0 && !loading && !err ? (
        <div className="card py-16 text-center text-sm text-muted">
          {filtered
            ? t('usage.noDataFiltered', { filter: filterDesc ? `（${filterDesc}）` : '' })
            : t('usage.noDataHint')}
        </div>
      ) : (
        /* key=refreshTick：手动刷新时重挂载触发淡入动画，让“刷新生效”可见 */
        <div key={refreshTick} className="data-refresh">
          {/* KPI 卡片阵列 */}
          <div className="mb-4 grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-6">
            <KpiCard icon={<Layers className="h-3.5 w-3.5" />} label={t('usage.totalTokens')} value={fmtCompact(total)} valueTitle={fmtInt(total)} delta={delta('tokens')} prevLabel={prevLabel}
              hint={`${t('usage.hint.totalTokens')}\n${t('usage.deltaHint')}`} />
            <KpiCard icon={<ArrowDownToLine className="h-3.5 w-3.5" />} label={t('usage.inputTokens')} value={fmtCompact(cur.prompt)} valueTitle={fmtInt(cur.prompt)} sub={t('usage.pctOf', { pct: `${total > 0 ? ((cur.prompt / total) * 100).toFixed(1) : '0.0'}` })} delta={delta('prompt')} prevLabel={prevLabel}
              hint={`${t('usage.hint.inputTokens')}\n${t('usage.deltaHint')}`} />
            <KpiCard icon={<ArrowUpFromLine className="h-3.5 w-3.5" />} label={t('usage.outputTokens')} value={fmtCompact(cur.completion)} valueTitle={fmtInt(cur.completion)} sub={t('usage.pctOf', { pct: `${total > 0 ? ((cur.completion / total) * 100).toFixed(1) : '0.0'}` })} delta={delta('completion')} prevLabel={prevLabel}
              hint={`${t('usage.hint.outputTokens')}\n${t('usage.deltaHint')}`} />
            <KpiCard icon={<Activity className="h-3.5 w-3.5" />} label={t('usage.requests')} value={fmtInt(cur.requests)} delta={delta('requests')} prevLabel={prevLabel}
              hint={`${t('usage.hint.requests')}\n${t('usage.deltaHint')}`} />
            <KpiCard icon={<Gauge className="h-3.5 w-3.5" />} label={t('usage.avgLatency')} value={`${Math.round(cur.avgLatency)}ms`} delta={delta('avgLatency')} prevLabel={prevLabel}
              hint={`${t('usage.hint.avgLatency')}\n${t('usage.deltaHint')}`} />
            <KpiCard icon={<DollarSign className="h-3.5 w-3.5" />} label={t('usage.cost')} value={fmtMoney(cur.cost)} delta={delta('cost')} prevLabel={prevLabel}
              hint={`${t('usage.hint.cost')}\n${t('usage.deltaHint')}`} />
          </div>

          <div className="grid grid-cols-1 gap-4 lg:grid-cols-5">
            {/* 每日趋势 */}
            <div className="card lg:col-span-3">
              <div className="mb-3 flex items-center justify-between gap-2">
                <h3 className="flex items-center gap-3 text-sm font-medium">
                  {t('usage.dailyTrend')}
                  <span className="flex items-center gap-3">
                    {trendMetric === 'tokens'
                      ? (<>
                          <LegendDot color={COLOR_IN} label={t('usage.input')} hint={t('usage.hint.inputLegend')} />
                          <LegendDot color={COLOR_OUT} label={t('usage.output')} hint={t('usage.hint.outputLegend')} />
                        </>)
                      : <LegendDot color={COLOR_OUT} label={t('usage.requests')} hint={t('usage.hint.reqLegend')} />}
                  </span>
                </h3>
                <Segmented options={metricOpts} value={trendMetric} onChange={setTrendMetric} />
              </div>
              {trend.length === 0 ? <Empty /> : <StackedBarChart data={trend} />}
            </div>

            {/* 模型分布甜甜圈 */}
            <div className="card lg:col-span-2">
              <div className="mb-3 flex items-center justify-between gap-2">
                <h3 className="text-sm font-medium">{t('usage.modelDist')}</h3>
                <Segmented options={metricOpts} value={donutMetric} onChange={setDonutMetric} />
              </div>
              {donutData.length === 0 ? <Empty /> : (
                <div className="flex items-center gap-4">
                  <Donut data={donutData}
                    centerValue={donutMetric === 'tokens' ? fmtCompact(total) : fmtInt(cur.requests)}
                    centerLabel={donutMetric === 'tokens' ? 'Tokens' : t('usage.request')} />
                  <LegendList data={donutData} />
                </div>
              )}
            </div>
          </div>

          {/* 分时活跃热力图 */}
          <div className="card mt-4">
            <div className="mb-3 flex items-center justify-between gap-2">
              <h3 className="text-sm font-medium">{t('usage.hourly')}</h3>
              <Segmented options={metricOpts} value={heatMetric} onChange={(k) => setHeatMetric(k as 'tokens' | 'requests')} />
            </div>
            <HourHeatmap rows={hourRows} metric={heatMetric} />
          </div>

          {/* 渠道用量（默认折叠）：每个自助配置的渠道在当前区间内的用量汇总，
              展开后点击行可按该渠道筛选上方全部图表；仅元数据聚合，与路由同口径隔离。
              分页条在 overflow 容器外，避免随表格横向滚动 */}
          <div className="mt-4">
            <div className="card overflow-x-auto">
              <div
                className="flex cursor-pointer select-none items-center justify-between gap-2 px-4 py-3.5"
                onClick={() => setProviderOpen((v) => !v)}
                title={providerOpen ? t('usage.providerCollapse') : t('usage.providerExpand')}
              >
                <h3 className="flex items-center gap-2 text-sm font-medium">
                  <Server className="h-3.5 w-3.5 text-muted" /> {t('usage.providerUsage')}
                  <ChevronDown className={`h-4 w-4 text-muted transition-transform ${providerOpen ? 'rotate-180' : ''}`} />
                </h3>
                <span className="text-xs text-muted">{t('usage.providerUsageHint')}</span>
              </div>
              {providerOpen && (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="thead-row text-left text-xs">
                      <th className="px-4 py-3">{t('common.provider')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.requests')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.inputTokens')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.outputTokens')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.totalTokens')}</th>
                      <th className="px-4 py-3">{t('usage.tokenShare')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.cost')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.avgLatency')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.lastUsed')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {providerPagedRows.map((r) => {
                      const tk = r.prompt_tokens + r.completion_tokens
                      const pct = providerTotalTokens > 0 ? (tk / providerTotalTokens) * 100 : 0
                      const active = providerId === r.provider_id
                      const kindLabel = r.kind === 'oauth'
                        ? (r.oauth_provider || 'oauth')
                        : r.protocol
                      return (
                        <tr key={r.provider_id} className="tbody-row cursor-pointer"
                          style={active ? { background: 'color-mix(in oklab, #7c3aed 10%, transparent)' } : undefined}
                          title={t('usage.rowFilterTipProvider')}
                          onClick={() => { if (!active) { setProviderId(r.provider_id); scrollToCharts() } else setProviderId(0) }}>
                          <td className="px-4 py-2.5">
                            <div className="flex items-center gap-2">
                              <ProviderLogo platform={r.kind === 'oauth' ? (r.oauth_provider || r.protocol) : r.protocol}
                                name={providerDisplayName(r)} size={24} />
                              <span className="max-w-40 truncate" title={r.name || ''}>
                                {r.name || <span className="text-muted">{t('usage.deletedProvider')}</span>}
                              </span>
                              <span className="badge badge-zinc shrink-0 text-[10px]">{kindLabel}</span>
                              {active && <span className="badge-zinc badge shrink-0">{t('usage.filtering')}</span>}
                            </div>
                          </td>
                          <td className="px-4 py-2.5 text-right tabular-nums">{fmtInt(r.requests)}</td>
                          <td className="px-4 py-2.5 text-right tabular-nums text-muted" title={fmtInt(r.prompt_tokens)}>{fmtCompact(r.prompt_tokens)}</td>
                          <td className="px-4 py-2.5 text-right tabular-nums text-muted" title={fmtInt(r.completion_tokens)}>{fmtCompact(r.completion_tokens)}</td>
                          <td className="px-4 py-2.5 text-right font-medium tabular-nums" title={fmtInt(tk)}>{fmtCompact(tk)}</td>
                          <td className="px-4 py-2.5">
                            <div className="flex items-center gap-2">
                              <div className="track h-1.5 w-24 shrink-0 overflow-hidden rounded-full">
                                <div className="h-full rounded-full" style={{ width: `${pct}%`, background: '#8b5cf6' }} />
                              </div>
                              <span className="text-xs text-muted tabular-nums">{pct.toFixed(1)}%</span>
                            </div>
                          </td>
                          <td className="px-4 py-2.5 text-right tabular-nums font-medium">{fmtMoney(r.cost)}</td>
                          <td className="px-4 py-2.5 text-right tabular-nums text-muted">{Math.round(r.avg_latency_ms)}ms</td>
                          <td className="whitespace-nowrap px-4 py-2.5 text-right text-xs text-muted">
                            {r.last_used_at ? new Date(r.last_used_at).toLocaleString() : '—'}
                          </td>
                        </tr>
                      )
                    })}
                    {byProviderRows.length === 0 && (
                      <tr><td colSpan={9} className="px-4 py-10 text-center text-muted">{t('usage.noDataInRange')}</td></tr>
                    )}
                  </tbody>
                </table>
              )}
            </div>
            {/* 分页条在 overflow 容器外，避免随表格横向滚动；展开且有数据时才显示 */}
            {providerOpen && byProviderRows.length > 0 && (
              <Pagination className="mt-2"
                total={byProviderRows.length} page={providerPage} pageSize={providerPageSize}
                pageSizes={[5, 10, 20, 50]}
                onPage={setProviderPage}
                onPageSize={(n) => { setProviderPageSize(n); setProviderPage(0) }} />
            )}
          </div>

          {/* Key 用量表（默认折叠，与渠道用量表同款交互）：每个平台签发的 Key 在当前区间内的用量汇总 */}
          <div className="mt-4">
            <div className="card overflow-x-auto">
              <div
                className="flex cursor-pointer select-none items-center justify-between gap-2 px-4 py-3.5"
                onClick={() => setKeyOpen((v) => !v)}
                title={keyOpen ? t('usage.keyCollapse') : t('usage.keyExpand')}
              >
                <h3 className="flex items-center gap-2 text-sm font-medium">
                  <KeyRound className="h-3.5 w-3.5 text-muted" /> {t('usage.keyUsage')}
                  <ChevronDown className={`h-4 w-4 text-muted transition-transform ${keyOpen ? 'rotate-180' : ''}`} />
                </h3>
                <span className="text-xs text-muted">{t('usage.keyUsageHint')}</span>
              </div>
              {keyOpen && (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="thead-row text-left text-xs">
                      <th className="px-4 py-3">Key</th>
                      <th className="px-4 py-3 text-right">{t('usage.requests')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.inputTokens')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.outputTokens')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.totalTokens')}</th>
                      <th className="px-4 py-3">{t('usage.tokenShare')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.cost')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.avgLatency')}</th>
                      <th className="px-4 py-3 text-right">{t('usage.lastUsed')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {keyPagedRows.map((r) => {
                      const tk = r.prompt_tokens + r.completion_tokens
                      const pct = keyTotalTokens > 0 ? (tk / keyTotalTokens) * 100 : 0
                      const info = keyInfoMap.get(r.api_key_id)
                      const active = keyId === r.api_key_id
                      return (
                        <tr key={r.api_key_id} className="tbody-row cursor-pointer"
                          style={active ? { background: 'color-mix(in oklab, #7c3aed 10%, transparent)' } : undefined}
                          title={t('usage.rowFilterTip')}
                          onClick={() => { if (!active) { setKeyId(r.api_key_id); scrollToCharts() } else setKeyId(0) }}>
                          <td className="px-4 py-2.5">
                            <div className="flex items-center gap-2">
                              <span className="max-w-40 truncate" title={info?.name || ''}>
                                {r.key_name || <span className="text-muted">{t('usage.deletedKey')}</span>}
                              </span>
                              {r.key_prefix && <span className="font-mono text-xs text-muted">{r.key_prefix}…</span>}
                              {active && <span className="badge-zinc badge shrink-0">{t('usage.filtering')}</span>}
                            </div>
                          </td>
                          <td className="px-4 py-2.5 text-right tabular-nums">{fmtInt(r.requests)}</td>
                          <td className="px-4 py-2.5 text-right tabular-nums text-muted" title={fmtInt(r.prompt_tokens)}>{fmtCompact(r.prompt_tokens)}</td>
                          <td className="px-4 py-2.5 text-right tabular-nums text-muted" title={fmtInt(r.completion_tokens)}>{fmtCompact(r.completion_tokens)}</td>
                          <td className="px-4 py-2.5 text-right font-medium tabular-nums" title={fmtInt(tk)}>{fmtCompact(tk)}</td>
                          <td className="px-4 py-2.5">
                            <div className="flex items-center gap-2">
                              <div className="track h-1.5 w-24 shrink-0 overflow-hidden rounded-full">
                                <div className="h-full rounded-full" style={{ width: `${pct}%`, background: '#3b82f6' }} />
                              </div>
                              <span className="text-xs text-muted tabular-nums">{pct.toFixed(1)}%</span>
                            </div>
                          </td>
                          <td className="px-4 py-2.5 text-right tabular-nums font-medium">{fmtMoney(r.cost)}</td>
                          <td className="px-4 py-2.5 text-right tabular-nums text-muted">{Math.round(r.avg_latency_ms)}ms</td>
                          <td className="whitespace-nowrap px-4 py-2.5 text-right text-xs text-muted">
                            {r.last_used_at ? new Date(r.last_used_at).toLocaleString() : '—'}
                          </td>
                        </tr>
                      )
                    })}
                    {byKeyRows.length === 0 && (
                      <tr><td colSpan={9} className="px-4 py-10 text-center text-muted">{t('usage.noDataInRange')}</td></tr>
                    )}
                  </tbody>
                </table>
              )}
            </div>
            {/* 分页条在 overflow 容器外，避免随表格横向滚动；展开且有数据时才显示 */}
            {keyOpen && byKeyRows.length > 0 && (
              <Pagination className="mt-2"
                total={byKeyRows.length} page={keyPage} pageSize={keyPageSize}
                pageSizes={[5, 10, 20, 50]}
                onPage={setKeyPage}
                onPageSize={(n) => { setKeyPageSize(n); setKeyPage(0) }} />
            )}
          </div>

          {/* 明细表 */}
          <div className="card mt-4 overflow-x-auto p-0">
            <table className="w-full text-sm">
              <thead>
                <tr className="thead-row text-left text-xs">
                  <th className="px-4 py-3">{t('usage.date')}</th><th className="px-4 py-3">Key</th><th className="px-4 py-3">{t('common.model')}</th>
                  <th className="px-4 py-3 text-right">{t('usage.requests')}</th>
                  <th className="px-4 py-3 text-right">{t('usage.inputTokens')}</th>
                  <th className="px-4 py-3 text-right">{t('usage.outputTokens')}</th>
                  <th className="px-4 py-3 text-right">{t('usage.cost')}</th>
                  <th className="px-4 py-3 text-right">{t('usage.avgLatency')}</th>
                </tr>
              </thead>
              <tbody>
                {pagedRows.map((r, i) => {
                  const info = keyInfoMap.get(r.api_key_id)
                  return (
                    <tr key={i} className="tbody-row">
                      <td className="whitespace-nowrap px-4 py-2.5 text-xs text-muted" title={r.day}>{dayLabel(r.day)}</td>
                      <td className="px-4 py-2.5">
                        <span className="max-w-40 cursor-default truncate text-xs" title={keyDisplayName(r.api_key_id)}>
                          {keyDisplayName(r.api_key_id)}
                        </span>
                        {info?.deleted && <span className="ml-1.5 text-[10px] text-muted">{t('usage.deleted')}</span>}
                      </td>
                      <td className="px-4 py-2.5"><span className="badge badge-zinc max-w-52 cursor-default truncate" title={r.model}>{r.model}</span></td>
                      <td className="px-4 py-2.5 text-right tabular-nums">{r.requests}</td>
                      <td className="px-4 py-2.5 text-right tabular-nums text-muted">{fmtInt(r.prompt_tokens)}</td>
                      <td className="px-4 py-2.5 text-right tabular-nums text-muted">{fmtInt(r.completion_tokens)}</td>
                      <td className="px-4 py-2.5 text-right tabular-nums font-medium">{fmtMoney(r.cost)}</td>
                      <td className="px-4 py-2.5 text-right tabular-nums text-muted">{Math.round(r.avg_latency_ms)}ms</td>
                    </tr>
                  )
                })}
                {rows.length === 0 && (
                  <tr><td colSpan={8} className="px-4 py-10 text-center text-muted">{t('usage.noDataHint')}</td></tr>
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
      )}
      {loading && <div className="mt-2 text-center text-xs text-muted opacity-70">{t('common.loading')}</div>}
    </div>
  )
}

function Empty() {
  const { t } = useTranslation()
  return <div className="py-10 text-center text-sm text-muted opacity-70">{t('common.noData')}</div>
}

function sumUp(rows: UsageAggRow[]) {
  let requests = 0, prompt = 0, completion = 0, cost = 0, latencySum = 0
  for (const r of rows) {
    requests += r.requests
    prompt += r.prompt_tokens
    completion += r.completion_tokens
    cost += r.cost ?? 0
    latencySum += r.avg_latency_ms * r.requests // avg×count = 组内总延迟，加权平均精确
  }
  return {
    requests, prompt, completion, cost,
    tokens: prompt + completion,
    avgLatency: requests > 0 ? latencySum / requests : 0,
  }
}

// fmtMoney 费用展示：金额量级小（单请求 $0.00x 级），保留 4 位并去尾零；≥$100 保留 2 位
function fmtMoney(n: number) {
  const v = Math.abs(n) >= 100 ? n.toFixed(2) : n.toFixed(4)
  return '$' + v.replace(/0+$/, '').replace(/\.$/, '')
}
