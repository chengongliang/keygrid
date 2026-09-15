import { useEffect, useState, type ReactNode } from 'react'
import { HelpCircle } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { UsageHourRow } from '@/lib/api'

// ---- 用量分析共享组件（手写 SVG / CSS，不引第三方库）----
// 配色约定：输入=violet、输出=blue，与其余页面蓝紫审美一致。

export const COLOR_IN = '#8b5cf6'
export const COLOR_OUT = '#3b82f6'
export const PALETTE = ['#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6', '#14b8a6', '#ec4899', '#64748b']

export function fmtCompact(n: number): string {
  if (n >= 1e9) return trim1(n / 1e9) + 'B'
  if (n >= 1e6) return trim1(n / 1e6) + 'M'
  if (n >= 1e3) return trim1(n / 1e3) + 'K'
  return String(Math.round(n))
}
function trim1(x: number): string {
  return (Math.round(x * 10) / 10).toString().replace(/\.0$/, '')
}
export function fmtInt(n: number): string {
  return Math.round(n).toLocaleString('en-US')
}

// 环比：上一期无数据（prev=0）时返回 null（不渲染 badge）
export function pctDelta(cur: number, prev: number): number | null {
  if (prev <= 0) return null
  return ((cur - prev) / prev) * 100
}

// ---- 日期工具（与后端 Asia/Shanghai 日粒度对齐）----
const SH_MS = 8 * 3600e3

// UTC ISO → 上海日历日 'YYYY-MM-DD'
export function toSHDay(iso: string): string {
  return new Date(Date.parse(iso) + SH_MS).toISOString().slice(0, 10)
}
// 上海日历日（今天）
export function shTodayStr(): string {
  return toSHDay(new Date().toISOString())
}
// 上海日历日 ±n 天
export function addDayStr(day: string, n: number): string {
  const t = new Date(day + 'T00:00:00Z')
  t.setUTCDate(t.getUTCDate() + n)
  return t.toISOString().slice(0, 10)
}
// 上海日历日 00:00 → UTC ISO
export function shDayStartISO(day: string): string {
  return new Date(day + 'T00:00:00+08:00').toISOString()
}
// UTC ISO ±n 天
export function addDaysISO(iso: string, n: number): string {
  return new Date(Date.parse(iso) + n * 86400e3).toISOString()
}
// 'YYYY-MM-DD' → 'M/D'
export function dayLabel(iso: string): string {
  const [, m, d] = iso.split('-')
  return `${Number(m)}/${Number(d)}`
}

export interface DateRange { from: string; to: string } // UTC ISO，[from, to)
export interface RangeSel extends DateRange { key: string }

// 快捷档 → 区间（上海自然日；24H 为滚动 24 小时）
export function presetRange(key: string): DateRange {
  const today = shTodayStr()
  const dayStart = (offset: number) => shDayStartISO(addDayStr(today, offset))
  switch (key) {
    case 'today':
      return { from: dayStart(0), to: addDaysISO(dayStart(0), 1) }
    case '24h':
      return { from: addDaysISO(new Date().toISOString(), -1), to: new Date().toISOString() }
    case '7d':
      return { from: dayStart(-6), to: addDaysISO(dayStart(-6), 7) }
    case '30d':
      return { from: dayStart(-29), to: addDaysISO(dayStart(-29), 30) }
    case '90d':
      return { from: dayStart(-89), to: addDaysISO(dayStart(-89), 90) }
    default:
      return { from: dayStart(-6), to: addDaysISO(dayStart(-6), 7) }
  }
}

// 等长前移一个周期（环比用）
export function prevRange(r: DateRange): DateRange {
  const len = Date.parse(r.to) - Date.parse(r.from)
  return { from: new Date(Date.parse(r.from) - len).toISOString(), to: r.from }
}

// 区间展示标签（to 为开区间，展示取 to-1ms 所在日）；跨年时补年份
function fmtDayCN(iso: string): string {
  const [y, m, d] = toSHDay(iso).split('-')
  return `${Number(m)}/${Number(d)}` + (y !== String(new Date().getUTCFullYear()) ? `/${y.slice(2)}` : '')
}
export function rangeLabel(r: DateRange): string {
  return `${fmtDayCN(r.from)} ~ ${fmtDayCN(new Date(Date.parse(r.to) - 1).toISOString())}`
}

// ---- 悬停详情（纯 CSS tooltip）----
export function Tip({ label, children, side = 'top', className }: {
  label?: ReactNode
  children: ReactNode
  side?: 'top' | 'bottom'
  className?: string
}) {
  if (label == null) return <span className={className}>{children}</span>
  return (
    <span className={'group/tip relative inline-flex ' + (className ?? '')}>
      {children}
      <span className={`pointer-events-none absolute left-1/2 z-30 hidden w-max max-w-64 -translate-x-1/2 rounded-lg px-2.5 py-1.5 text-[11px] leading-relaxed group-hover/tip:block ${
        side === 'top' ? 'bottom-full mb-1.5' : 'top-full mt-1.5'}`}
        style={{
          background: 'var(--panel-solid)',
          border: '1px solid var(--line)',
          color: 'var(--text)',
          boxShadow: '0 6px 20px rgba(0,0,0,0.14)',
          whiteSpace: 'pre-line',
        }}>
        {label}
      </span>
    </span>
  )
}

// ---- 分段切换（胶囊组）----
export function Segmented({ options, value, onChange }: {
  options: { key: string; label: string }[]
  value: string
  onChange: (k: string) => void
}) {
  return (
    <div className="inline-flex items-center gap-0.5 rounded-lg p-0.5" style={{ border: '1px solid var(--line)' }}>
      {options.map((o) => (
        <button key={o.key} onClick={() => onChange(o.key)}
          className={'rounded-md px-2.5 py-1 text-xs font-medium transition ' + (value === o.key ? 'tab-active' : 'text-muted hover:opacity-80')}>
          {o.label}
        </button>
      ))}
    </div>
  )
}

// ---- 日期筛选栏：今天 / 24H / 7D / 30D / 90D / 自定义 ----
// 整体一条胶囊：快捷档 + 分隔线 + 无边框日期输入 + 应用按钮，高度统一 h-6
// 非组件常量存 i18n key，渲染处统一 t()
const RANGE_PRESETS = [
  { key: 'today', label: 'usage.range.today' },
  { key: '24h', label: 'usage.range.24h' },
  { key: '7d', label: 'usage.range.7d' },
  { key: '30d', label: 'usage.range.30d' },
  { key: '90d', label: 'usage.range.90d' },
  { key: 'custom', label: 'usage.range.custom' },
]

const dateInputCls = 'h-6 w-30 shrink-0 rounded-md bg-transparent px-1.5 text-xs focus:outline-none focus:ring-2 focus:ring-blue-600/30'

export function RangeBar({ value, onChange }: {
  value: RangeSel
  onChange: (v: RangeSel) => void
}) {
  const { t } = useTranslation()
  const [selKey, setSelKey] = useState(value.key)
  const [fromStr, setFromStr] = useState(() => addDayStr(shTodayStr(), -6))
  const [toStr, setToStr] = useState(() => shTodayStr())
  useEffect(() => setSelKey(value.key), [value.key])

  const pick = (k: string) => {
    setSelKey(k)
    if (k !== 'custom') onChange({ key: k, ...presetRange(k) })
  }
  const valid = fromStr !== '' && toStr !== '' && fromStr <= toStr
  const apply = () => {
    if (!valid) return
    onChange({ key: 'custom', from: shDayStartISO(fromStr), to: addDaysISO(shDayStartISO(toStr), 1) })
  }

  return (
    <div className="inline-flex max-w-full flex-wrap items-center gap-0.5 rounded-lg p-0.5"
      style={{ border: '1px solid var(--line)' }}>
      {RANGE_PRESETS.map((o) => (
        <button key={o.key} onClick={() => pick(o.key)}
          className={'shrink-0 rounded-md px-2.5 py-1 text-xs font-medium transition ' +
            (selKey === o.key ? 'tab-active' : 'text-muted hover:opacity-80')}>
          {t(o.label)}
        </button>
      ))}
      {selKey === 'custom' && (
        <span className="flex shrink-0 items-center gap-1">
          <span className="mx-0.5 h-4 w-px" style={{ background: 'var(--line)' }} />
          <input type="date" value={fromStr} max={toStr || undefined}
            onChange={(e) => setFromStr(e.target.value)} className={dateInputCls} />
          <span className="text-xs text-muted">–</span>
          <input type="date" value={toStr} min={fromStr || undefined}
            onChange={(e) => setToStr(e.target.value)} className={dateInputCls} />
          <button onClick={apply} disabled={!valid}
            className="ml-0.5 inline-flex h-6 shrink-0 items-center rounded-md bg-blue-600 px-2.5 text-xs font-medium text-white transition hover:bg-blue-500 disabled:pointer-events-none disabled:opacity-50">
            {t('common.apply')}
          </button>
        </span>
      )}
    </div>
  )
}

// ---- KPI 卡片（图标/标签悬停显示指标口径；环比徽标悬停显示对比周期）----
export function KpiCard({ icon, label, value, sub, delta, hint, prevLabel, valueTitle }: {
  icon: ReactNode
  label: string
  value: string
  sub?: string
  delta?: number | null
  hint?: ReactNode
  prevLabel?: string
  // 紧凑数值（如 22.7M）的悬浮详数（如 22,688,074）
  valueTitle?: string
}) {
  const { t } = useTranslation()
  const labelEl = (
    <span className="flex min-w-0 items-center gap-1.5 text-xs text-muted">
      {icon}
      <span className="truncate">{label}</span>
      {hint != null && <HelpCircle className="h-3 w-3 shrink-0 opacity-45" />}
    </span>
  )
  return (
    <div className="card flex flex-col gap-1 p-3.5">
      <div className="flex items-center justify-between gap-1">
        <Tip label={hint} side="bottom" className="min-w-0">{labelEl}</Tip>
        {delta != null && (
          <Tip label={t('usage.deltaTip', { period: prevLabel ? `（${prevLabel}）` : '' })} side="bottom">
            <DeltaBadge delta={delta} />
          </Tip>
        )}
      </div>
      <div className="font-mono text-xl font-semibold leading-tight" style={{ fontVariantNumeric: 'tabular-nums' }} title={valueTitle}>{value}</div>
      {sub && <div className="text-[11px] text-muted">{sub}</div>}
    </div>
  )
}

export function DeltaBadge({ delta }: { delta: number }) {
  const up = delta >= 0
  // 用量涨跌中性语义：涨=amber、跌=emerald（与参考图一致，跌显示绿色）
  return (
    <span className={'inline-flex shrink-0 cursor-default items-center gap-0.5 rounded-full px-1.5 py-0.5 text-[11px] font-medium ' +
      (up ? 'text-amber-600 dark:text-amber-400' : 'text-emerald-600 dark:text-emerald-400')}
      style={{ background: up ? 'color-mix(in oklab, #d97706 12%, transparent)' : 'color-mix(in oklab, #059669 12%, transparent)' }}>
      {up ? '▲' : '▼'}{Math.abs(delta).toFixed(1)}%
    </span>
  )
}

// ---- 图例圆点（可带悬停说明）----
export function LegendDot({ color, label, hint }: { color: string; label: string; hint?: ReactNode }) {
  const dot = (
    <span className="inline-flex cursor-default items-center gap-1.5 text-xs text-muted">
      <span className="size-2 rounded-full" style={{ background: color }} />
      {label}
    </span>
  )
  return hint != null ? <Tip label={hint}>{dot}</Tip> : dot
}

// ---- 图表悬浮提示（fixed 定位，跟随鼠标进入点）----
// SVG 内无法嵌 HTML Tip，统一用 fixed tooltip：即时显示、样式与 Tip 一致、不受卡片 overflow 裁剪
export interface ChartHover {
  x: number // 视口坐标（clientX/Y）
  y: number
  title: string
  rows: { color: string; label: string; value: string }[]
}

export function ChartTip({ hover }: { hover: ChartHover }) {
  return (
    <div className="pointer-events-none fixed z-50 w-max max-w-64 -translate-x-1/2 -translate-y-full rounded-lg px-2.5 py-1.5 text-[11px] leading-relaxed"
      style={{
        left: hover.x,
        top: hover.y - 10, // 鼠标上方留 10px 间距
        background: 'var(--panel-solid)',
        border: '1px solid var(--line)',
        color: 'var(--text)',
        boxShadow: '0 6px 20px rgba(0,0,0,0.14)',
      }}>
      <div className="mb-0.5 font-medium">{hover.title}</div>
      {hover.rows.map((r, i) => (
        <div key={i} className="flex items-center gap-1.5 whitespace-nowrap">
          <span className="size-2 shrink-0 rounded-full" style={{ background: r.color }} />
          <span>{r.label}</span>
          <span className="ml-2 tabular-nums text-muted">{r.value}</span>
        </div>
      ))}
    </div>
  )
}

// 悬浮锚点：记录鼠标进入时的视口坐标（enter 时定位一次，移动中不跟随，避免抖动）
const hoverAt = (e: { clientX: number; clientY: number }) => ({ x: e.clientX, y: e.clientY })

// ---- 堆叠柱状图 ----
export interface BarSeg { key: string; value: number; color: string; label: string }

export function StackedBarChart({ data, height = 190, tipUnit }: {
  data: { label: string; segs: BarSeg[] }[]
  height?: number
  tipUnit?: string
}) {
  const [hover, setHover] = useState<{ i: number; x: number; y: number } | null>(null)
  const W = 640, P = { t: 18, r: 12, b: 22, l: 44 }
  const H = height
  const iw = W - P.l - P.r, ih = H - P.t - P.b
  const totals = data.map((d) => d.segs.reduce((s, x) => s + x.value, 0))
  const max = Math.max(...totals, 1)
  const n = data.length
  const slot = n > 0 ? iw / n : 0
  const bw = Math.min(slot * 0.62, 46)
  const u = tipUnit ?? ''

  const yTicks = [1, 0.5, 0].map((f) => ({ f, y: P.t + ih * (1 - f) }))
  // 柱子多时隔几个标一个日期
  const labelStep = Math.max(1, Math.ceil(n / 12))

  return (
    <div className="relative">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ fontVariantNumeric: 'tabular-nums' }}>
      {yTicks.map(({ f, y }) => (
        <g key={f}>
          <line x1={P.l} y1={y} x2={W - P.r} y2={y} stroke="var(--line)" strokeDasharray={f === 0 ? undefined : '3 3'} opacity={f === 0 ? 1 : 0.7} />
          <text x={P.l - 6} y={y + 3} textAnchor="end" fontSize={10} fill="var(--muted)">{fmtCompact(max * f)}</text>
        </g>
      ))}
      {data.map((d, i) => {
        const x = P.l + slot * i + (slot - bw) / 2
        let acc = 0
        return (
          <g key={d.label + i}
            onMouseEnter={(e) => setHover({ i, ...hoverAt(e) })}
            onMouseLeave={() => setHover((h) => (h?.i === i ? null : h))}>
            {d.segs.map((s) => {
              const h = (s.value / max) * ih
              const y = P.t + ih - acc - h
              acc += h
              return (
                <rect key={s.key} x={x} y={y} width={bw} height={Math.max(h, s.value > 0 ? 2 : 0)} rx={2.5}
                  fill={s.color} opacity={hover?.i === i ? 1 : 0.92} />
              )
            })}
            {(n <= 12 || i % labelStep === 0) && (
              <text x={x + bw / 2} y={H - 8} textAnchor="middle" fontSize={9.5} fill="var(--muted)">{d.label}</text>
            )}
          </g>
        )
      })}
      </svg>
      {hover != null && data[hover.i] && (
        <ChartTip hover={{
          x: hover.x,
          y: hover.y,
          title: data[hover.i].label,
          rows: data[hover.i].segs.map((s) => ({ color: s.color, label: s.label, value: `${fmtInt(s.value)}${u}` })),
        }} />
      )}
    </div>
  )
}

// ---- 甜甜圈图（分段 stroke + 段间隙）----
export function Donut({ data, size = 148, thickness = 17, centerValue, centerLabel }: {
  data: { label: string; value: number; color: string }[]
  size?: number
  thickness?: number
  centerValue?: string
  centerLabel?: string
}) {
  const { t } = useTranslation()
  const [hover, setHover] = useState<{ label: string; frac: number; x: number; y: number } | null>(null)
  const total = data.reduce((s, d) => s + d.value, 0)
  const r = (size - thickness) / 2
  const C = 2 * Math.PI * r
  const gap = data.length > 1 ? Math.min(3, C / 60) : 0
  let offset = 0

  return (
    <div className="relative shrink-0">
      <svg viewBox={`0 0 ${size} ${size}`} width={size} height={size} className="shrink-0"
        onMouseLeave={() => setHover(null)}>
        <g transform={`rotate(-90 ${size / 2} ${size / 2})`}>
          <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="var(--line)" strokeWidth={thickness} opacity={0.4} />
          {total > 0 && data.map((d) => {
            const frac = d.value / total
            const arc = Math.max(frac * C - gap, 0.5)
            const hovered = hover?.label === d.label
            const el = (
              <circle key={d.label} cx={size / 2} cy={size / 2} r={r} fill="none"
                stroke={d.color} strokeWidth={hovered ? thickness + 3 : thickness}
                strokeDasharray={`${arc} ${C - arc}`} strokeDashoffset={-offset} strokeLinecap="butt"
                opacity={hover && !hovered ? 0.45 : 1}
                onMouseEnter={(e) => setHover({ label: d.label, frac, ...hoverAt(e) })} />
            )
            offset += frac * C
            return el
          })}
        </g>
        {centerValue && (
          <text x={size / 2} y={size / 2 - 2} textAnchor="middle" fontSize={17} fontWeight={600} fill="var(--text)"
            style={{ fontVariantNumeric: 'tabular-nums' }}>{centerValue}</text>
        )}
        {centerLabel && (
          <text x={size / 2} y={size / 2 + 15} textAnchor="middle" fontSize={10} fill="var(--muted)">{centerLabel}</text>
        )}
      </svg>
      {hover != null && (() => {
        const d = data.find((x) => x.label === hover.label)
        if (!d) return null
        return (
          <ChartTip hover={{
            x: hover.x,
            y: hover.y,
            title: d.label,
            rows: [{ color: d.color, label: t('usage.share'), value: `${fmtInt(d.value)}（${(hover.frac * 100).toFixed(1)}%）` }],
          }} />
        )
      })()}
    </div>
  )
}

// ---- 图例列表（名称 + 数值 + 百分比，右对齐等宽）----
export function LegendList({ data, unit = '' }: {
  data: { label: string; value: number; color: string }[]
  unit?: string
}) {
  const { t } = useTranslation()
  const total = data.reduce((s, d) => s + d.value, 0) || 1
  return (
    <div className="min-w-0 flex-1 space-y-1.5">
      {data.map((d) => (
        // Tip 根元素必须可收缩（w-full + min-w-0），否则 inline-flex 由内容撑宽，
        // 长数值会把整行撑出卡片（TOP 用户/模型分布溢出问题）
        <Tip key={d.label} label={t('usage.legendTip', {
          label: d.label,
          value: `${fmtInt(d.value)}${unit}`,
          pct: `${((d.value / total) * 100).toFixed(1)}%`,
        })}
          className="w-full min-w-0">
          <span className="flex w-full cursor-default items-center gap-2 text-xs">
            <span className="size-2 shrink-0 rounded-full" style={{ background: d.color }} />
            <span className="min-w-0 flex-1 truncate">{d.label}</span>
            <span className="shrink-0 text-muted tabular-nums">{fmtInt(d.value)}{unit}</span>
            <span className="w-11 shrink-0 text-right text-muted tabular-nums">{((d.value / total) * 100).toFixed(1)}%</span>
          </span>
        </Tip>
      ))}
    </div>
  )
}

// ---- 分页条（明细表用；数据已全量在前端，纯前端切片）----
export function Pagination({ total, page, pageSize, onPage, onPageSize, pageSizes = [20, 50, 100], className = '' }: {
  total: number
  page: number // 0-based
  pageSize: number
  onPage: (p: number) => void
  onPageSize?: (n: number) => void
  pageSizes?: number[]
  className?: string
}) {
  const { t } = useTranslation()
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const cur = Math.min(page, pages - 1)
  const from = total === 0 ? 0 : cur * pageSize + 1
  const to = Math.min(total, (cur + 1) * pageSize)

  // 页码序列：1 … c-1 c c+1 … N；总页数少则全量展开
  const nums: (number | '…')[] = []
  if (pages <= 7) {
    for (let i = 1; i <= pages; i++) nums.push(i)
  } else {
    let s = Math.max(2, cur), e = Math.min(pages - 1, cur + 2)
    if (cur <= 3) { s = 2; e = 4 }
    if (cur >= pages - 4) { s = pages - 3; e = pages - 1 }
    nums.push(1)
    if (s > 2) nums.push('…')
    for (let i = s; i <= e; i++) nums.push(i)
    if (e < pages - 1) nums.push('…')
    nums.push(pages)
  }

  const navBtn = 'inline-flex h-6 items-center rounded-md px-2 text-xs transition text-muted hover:opacity-80 disabled:pointer-events-none disabled:opacity-40'
  const pageBtn = (active: boolean) =>
    'inline-flex h-6 min-w-6 items-center justify-center rounded-md px-1.5 text-xs tabular-nums transition ' +
    (active ? 'tab-active font-medium' : 'text-muted hover:opacity-80')

  return (
    <div className={'flex flex-wrap items-center justify-between gap-2 ' + className}>
      <div className="flex items-center gap-2 text-xs text-muted">
        <span>{t('usage.pageInfo', { total: fmtInt(total), from, to })}</span>
        {onPageSize && (
          <select
            value={pageSize}
            onChange={(e) => onPageSize(Number(e.target.value))}
            className="h-6 rounded-md px-1 text-xs focus:outline-none focus:ring-2 focus:ring-blue-600/30"
            style={{ border: '1px solid var(--line)', background: 'var(--input-bg)', color: 'var(--text)' }}
          >
            {pageSizes.map((n) => <option key={n} value={n}>{t('usage.perPage', { n })}</option>)}
          </select>
        )}
      </div>
      <div className="flex items-center gap-0.5">
        <button className={navBtn} disabled={cur <= 0} onClick={() => onPage(cur - 1)}>{t('common.prev')}</button>
        {nums.map((n, i) => n === '…'
          ? <span key={`e${i}`} className="px-1 text-xs text-muted">…</span>
          : <button key={n} className={pageBtn(n - 1 === cur)} disabled={n - 1 === cur} onClick={() => onPage(n - 1)}>{n}</button>
        )}
        <button className={navBtn} disabled={cur >= pages - 1} onClick={() => onPage(cur + 1)}>{t('common.next')}</button>
      </div>
    </div>
  )
}

// ---- 分时活跃热力图（7 行 × 24 列）----
// 星期标签存 i18n key，渲染处统一 t()
const DOW_LABELS = ['usage.dow.sun', 'usage.dow.mon', 'usage.dow.tue', 'usage.dow.wed', 'usage.dow.thu', 'usage.dow.fri', 'usage.dow.sat']

export function HourHeatmap({ rows, metric }: {
  rows: UsageHourRow[]
  metric: 'tokens' | 'requests'
}) {
  const { t } = useTranslation()
  const cell = new Map<string, number>()
  for (const r of rows) {
    const v = metric === 'tokens' ? r.prompt_tokens + r.completion_tokens : r.requests
    if (v > 0) cell.set(`${r.dow}-${r.hour}`, v)
  }
  const max = Math.max(...cell.values(), 0)
  // 5 档强度
  const level = (v: number): number => {
    if (v <= 0) return 0
    const f = v / max
    if (f > 0.75) return 4
    if (f > 0.5) return 3
    if (f > 0.25) return 2
    return 1
  }
  const cellBg = (v: number): string => {
    if (max === 0 || v <= 0) return 'color-mix(in oklab, var(--muted) 10%, transparent)'
    const f = 22 + level(v) * 19 // 41% ~ 98%
    return `color-mix(in oklab, #3b82f6 ${f}%, transparent)`
  }

  return (
    <div>
      <div className="grid gap-[3px]" style={{ gridTemplateColumns: '34px repeat(24, 1fr)' }}>
        {/* 列头：每 3 小时一个刻度 */}
        <div />
        {Array.from({ length: 24 }, (_, h) => (
          <div key={h} className="text-center text-[9px] leading-4 text-muted">{h % 3 === 0 ? h : ''}</div>
        ))}
        {/* 7 行：周日 → 周六 */}
        {DOW_LABELS.map((dl, dow) => {
          const dowLabel = t(dl)
          return (
            <div key={dl} className="contents">
              <div className="pr-1 text-right text-[10px] leading-none text-muted" style={{ alignSelf: 'center' }}>{dowLabel}</div>
              {Array.from({ length: 24 }, (_, h) => {
                const v = cell.get(`${dow}-${h}`) ?? 0
                const hh = String(h).padStart(2, '0')
                const label = v > 0
                  ? `${dowLabel} ${hh}:00 · ${metric === 'tokens' ? fmtInt(v) + ' tokens' : t('usage.requestsCount', { n: fmtInt(v) })}`
                  : `${dowLabel} ${hh}:00 · ${t('usage.noRequests')}`
                return (
                  <Tip key={h} label={label} className="aspect-square">
                    <span className="block h-full w-full rounded-[3px]" style={{ background: cellBg(v) }} />
                  </Tip>
                )
              })}
            </div>
          )
        })}
      </div>
      <div className="mt-2 flex items-center justify-end gap-1.5 text-[10px] text-muted">
        {t('usage.less')}
        {[0, 1, 2, 3, 4].map((l) => (
          <span key={l} className="size-2.5 rounded-[3px]" style={{ background: l === 0 ? 'color-mix(in oklab, var(--muted) 10%, transparent)' : `color-mix(in oklab, #3b82f6 ${22 + l * 19}%, transparent)` }} />
        ))}
        {t('usage.more')}
      </div>
    </div>
  )
}
