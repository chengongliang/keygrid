import { useCallback, useEffect, useState } from 'react'
import { Bot, RefreshCw, ShieldAlert, Sparkles } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import {
  getWorkbuddyOverview,
  refreshWorkbuddyCredits,
  runWorkbuddyJob,
  updateWorkbuddySettings,
  type WorkbuddyChannel,
  type WorkbuddyJobKey,
  type WorkbuddyOverview,
  type WorkbuddyRun,
  type WorkbuddySettings,
} from '@/lib/api'

// 7 个内置任务的展示顺序（与后端 job_key 一致）
const JOB_KEYS: WorkbuddyJobKey[] = ['checkin', 'credits', 'daily_chat', 'growth', 'night', 'cat', 'daily_chat_web']

// 任务名 i18n key（模块级常量存 key，渲染处 t()）
const JOB_LABELS: Record<WorkbuddyJobKey, string> = {
  checkin: 'workbuddy.jobCheckin',
  credits: 'workbuddy.jobCredits',
  daily_chat: 'workbuddy.jobDailyChat',
  growth: 'workbuddy.jobGrowth',
  night: 'workbuddy.jobNight',
  cat: 'workbuddy.jobCat',
  daily_chat_web: 'workbuddy.jobDailyChatWeb',
}

// 高风险任务：开关默认灰置（弱化配色）且开启必须二次确认
const HIGH_RISK_JOBS = new Set<WorkbuddyJobKey>(['growth', 'night', 'cat', 'daily_chat_web'])

// 设置表单草稿：仅这几个字段走「显式保存」；开关类改动即时 PATCH
type SettingsDraft = Pick<
  WorkbuddySettings,
  'reserve_credits' | 'daily_token_limit' | 'product' | 'scrub_mode' | 'fingerprint'
>

// 数值守卫：接口字段可能缺省，统一按 0 处理，避免渲染 NaN / undefined
const safeNum = (n?: number): number => (typeof n === 'number' && Number.isFinite(n) ? n : 0)

const fmtNum = (n?: number): string => safeNum(n).toLocaleString(undefined, { maximumFractionDigits: 2 })

// 日期格式化：无效/缺失一律显示占位（与 Usage.tsx 的 new Date(...).toLocaleString() 口径一致）
const fmtTime = (iso?: string): string => {
  if (!iso) return '—'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

// 耗时：finished - started；任一缺失或非法时占位
const fmtDuration = (started?: string, finished?: string): string => {
  if (!started || !finished) return '—'
  const s = new Date(started).getTime()
  const f = new Date(finished).getTime()
  if (Number.isNaN(s) || Number.isNaN(f) || f < s) return '—'
  const ms = f - s
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)}s` : `${ms}ms`
}

// 错误信息提取（不引入 any）
const errMessage = (e: unknown): string => (e instanceof Error ? e.message : String(e))

// 取最早恢复的冷却时刻（状态徽章悬停展示恢复时间用）；无有效时间返回 undefined
const earliestCooldown = (cooldowns?: Record<string, string>): string | undefined => {
  let best = Number.POSITIVE_INFINITY
  let bestIso: string | undefined
  for (const iso of Object.values(cooldowns ?? {})) {
    const ts = new Date(iso).getTime()
    if (Number.isNaN(ts)) continue
    if (ts < best) {
      best = ts
      bestIso = iso
    }
  }
  return bestIso
}

const toDraft = (s: WorkbuddySettings): SettingsDraft => ({
  reserve_credits: safeNum(s.reserve_credits),
  daily_token_limit: safeNum(s.daily_token_limit),
  product: s.product ?? 'workbuddy',
  // 空值 = 未表态：后端默认 on（对齐参考实现的无条件脱敏，否则 Claude Code 会被判 11128）
  scrub_mode: s.scrub_mode || 'on',
  fingerprint: !!s.fingerprint,
})

export default function WorkBuddy() {
  const { t } = useTranslation()
  const [data, setData] = useState<WorkbuddyOverview | null>(null)
  const [draft, setDraft] = useState<SettingsDraft | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [notice, setNotice] = useState('')
  const [saving, setSaving] = useState(false)
  // 积分刷新中的渠道集合
  const [refreshing, setRefreshing] = useState<Record<number, boolean>>({})
  // 立即执行中的任务
  const [runningJob, setRunningJob] = useState<WorkbuddyJobKey | null>(null)

  const load = useCallback(() => {
    setLoading(true)
    setErr('')
    getWorkbuddyOverview()
      .then((d) => {
        // 契约保证返回 channels/runs 数组，但仍做一次兜底，避免后端缺字段时整页崩
        setData({
          ...d,
          channels: d.channels ?? [],
          runs: d.runs ?? [],
          platform: d.platform ?? { jobs_allowed: true },
        })
        setDraft(toDraft(d.settings))
      })
      .catch((e: unknown) => setErr(t('workbuddy.loadFailed', { error: errMessage(e) })))
      .finally(() => setLoading(false))
  }, [t])

  useEffect(load, [load])

  // 设置类即时 PATCH：成功后把补丁合并回本地 state。
  // jobs_enabled 在入参里是部分位（Partial），需与现有位合并后再写回。
  const applyPatch = async (patch: Parameters<typeof updateWorkbuddySettings>[0]) => {
    setErr('')
    setNotice('')
    try {
      await updateWorkbuddySettings(patch)
      setData((d) => {
        if (!d) return d
        const { jobs_enabled, ...rest } = patch
        const settings: WorkbuddySettings = { ...d.settings, ...rest }
        if (jobs_enabled) settings.jobs_enabled = { ...d.settings.jobs_enabled, ...jobs_enabled }
        return { ...d, settings }
      })
    } catch (e: unknown) {
      setErr(t('workbuddy.saveFailed', { error: errMessage(e) }))
    }
  }

  // 总开关：false → true 必须先确认风险文案
  const toggleEnabled = async (next: boolean) => {
    if (next && !confirm(t('workbuddy.confirmEnable'))) return
    await applyPatch({ enabled: next })
  }

  // 任务开关：高风险项开启前二次确认
  const toggleJob = async (key: WorkbuddyJobKey, next: boolean) => {
    if (!data) return
    if (next && HIGH_RISK_JOBS.has(key) && !confirm(t('workbuddy.confirmHighRisk', { job: t(JOB_LABELS[key]) }))) return
    await applyPatch({ jobs_enabled: { ...data.settings.jobs_enabled, [key]: next } })
  }

  // 表单显式保存（数值 / 下拉）
  const saveSettings = async () => {
    if (!draft) return
    setSaving(true)
    setErr('')
    setNotice('')
    try {
      await updateWorkbuddySettings(draft)
      setData((d) => (d ? { ...d, settings: { ...d.settings, ...draft } } : d))
      setNotice(t('workbuddy.saved'))
    } catch (e: unknown) {
      setErr(t('workbuddy.saveFailed', { error: errMessage(e) }))
    } finally {
      setSaving(false)
    }
  }

  // 手动刷新单个渠道积分
  const refreshCredits = async (ch: WorkbuddyChannel) => {
    setRefreshing((m) => ({ ...m, [ch.provider_id]: true }))
    setErr('')
    setNotice('')
    try {
      const r = await refreshWorkbuddyCredits(ch.provider_id)
      setData((d) =>
        d
          ? {
              ...d,
              channels: d.channels.map((c) =>
                c.provider_id === ch.provider_id ? { ...c, credits: r.credits, credits_at: r.credits_at } : c,
              ),
            }
          : d,
      )
      setNotice(t('workbuddy.creditsRefreshed', { name: ch.name }))
    } catch (e: unknown) {
      setErr(errMessage(e))
    } finally {
      setRefreshing((m) => {
        const n = { ...m }
        delete n[ch.provider_id]
        return n
      })
    }
  }

  // 立即执行任务（二次确认后 POST；完成再重载日志与运行态）
  const runNow = async (key: WorkbuddyJobKey) => {
    if (!confirm(t('workbuddy.confirmRun', { job: t(JOB_LABELS[key]) }))) return
    setRunningJob(key)
    setErr('')
    setNotice('')
    try {
      const r = await runWorkbuddyJob(key)
      const results = r.results ?? []
      setNotice(
        results.length
          ? results
              .map((x) => t('workbuddy.runResult', { id: x.provider_id, status: x.status, message: x.message }))
              .join('；')
          : t('workbuddy.runDone', { job: t(JOB_LABELS[key]) }),
      )
      load()
    } catch (e: unknown) {
      setErr(errMessage(e))
    } finally {
      setRunningJob(null)
    }
  }

  // 渠道状态徽章：parked_reason → 文案 / 配色 / 悬停说明
  const parkBadge = (reason?: string): { cls: string; key: string } => {
    switch (reason) {
      case 'disabled':
        return { cls: 'badge-zinc', key: 'workbuddy.parkDisabled' }
      case 'daily_limit':
        return { cls: 'badge-yellow', key: 'workbuddy.parkDailyLimit' }
      case 'reserve_credits':
        return { cls: 'badge-yellow', key: 'workbuddy.parkReserve' }
      case 'cooldown':
        return { cls: 'badge-blue', key: 'workbuddy.parkCooldown' }
      default:
        return { cls: 'badge-green', key: 'workbuddy.parkNone' }
    }
  }

  // 渠道 id → 名称（日志表回显；渠道可能已删除）
  const channelName = (id: number): string =>
    data?.channels.find((c) => c.provider_id === id)?.name ?? `#${id}`

  const runStatus = (status: WorkbuddyRun['status']): { cls: string; key: string } => {
    switch (status) {
      case 'ok':
        return { cls: 'badge-green', key: 'workbuddy.statusOk' }
      case 'skipped':
        return { cls: 'badge-zinc', key: 'workbuddy.statusSkipped' }
      case 'failed':
        return { cls: 'badge-red', key: 'workbuddy.statusFailed' }
      default:
        return { cls: 'badge-yellow', key: 'workbuddy.statusParked' }
    }
  }

  if (loading && !data) {
    return <div className="text-muted">{t('common.loading')}</div>
  }

  if (!data) {
    return (
      <div>
        {err && <div className="err-well rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">{err}</div>}
        <button className="btn-ghost mt-3" onClick={load}>
          <RefreshCw className="h-3.5 w-3.5" />
          {t('common.retry')}
        </button>
      </div>
    )
  }

  const settings = data.settings
  const jobsAllowed = data.platform?.jobs_allowed !== false
  const enabled = !!settings.enabled

  return (
    <div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="text-lg font-semibold">{t('workbuddy.title')}</h2>
          <p className="mt-0.5 text-xs text-muted">{t('workbuddy.subtitle')}</p>
        </div>
        <button className="btn-ghost" disabled={loading} onClick={load}>
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? 'animate-spin' : ''}`} />
          {t('common.refresh')}
        </button>
      </div>

      {err && <div className="err-well mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">{err}</div>}
      {notice && (
        <div
          className="mb-3 rounded-lg px-3 py-2 text-sm text-emerald-600 dark:text-emerald-400"
          style={{
            border: '1px solid color-mix(in oklab, #059669 40%, transparent)',
            background: 'color-mix(in oklab, #059669 8%, transparent)',
          }}
        >
          {notice}
        </div>
      )}

      {/* 顶部开关区 */}
      <div className="card mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-start gap-2.5">
          <Bot className="mt-0.5 h-5 w-5 shrink-0 text-violet-600 dark:text-violet-400" />
          <div className="min-w-0">
            <div className="font-medium">{t('workbuddy.enable')}</div>
            <p className="mt-0.5 text-xs text-muted">{enabled ? t('workbuddy.enableHint') : t('workbuddy.enableRisk')}</p>
          </div>
        </div>
        <label className="flex shrink-0 items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-violet-500"
            checked={enabled}
            onChange={(e) => toggleEnabled(e.target.checked)}
          />
          <span>{enabled ? t('common.enabled') : t('common.disabled')}</span>
        </label>
      </div>

      {/* 设置表单 */}
      {draft && (
        <div className="card mb-4">
          <h3 className="mb-3 flex items-center gap-2 text-sm font-medium">
            <Sparkles className="h-3.5 w-3.5 text-muted" />
            {t('workbuddy.settingsTitle')}
          </h3>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div>
              <label className="label">{t('workbuddy.reserveCredits')}</label>
              <input
                className="input"
                type="number"
                min={0}
                value={draft.reserve_credits}
                onChange={(e) => setDraft({ ...draft, reserve_credits: safeNum(Number(e.target.value)) })}
              />
              <p className="mt-1 text-xs text-muted">{t('workbuddy.reserveCreditsHint')}</p>
            </div>
            <div>
              <label className="label">{t('workbuddy.dailyTokenLimit')}</label>
              <input
                className="input"
                type="number"
                min={0}
                value={draft.daily_token_limit}
                onChange={(e) => setDraft({ ...draft, daily_token_limit: safeNum(Number(e.target.value)) })}
              />
              <p className="mt-1 text-xs text-muted">{t('workbuddy.dailyTokenLimitHint')}</p>
            </div>
            <div>
              <label className="label">{t('workbuddy.product')}</label>
              <select
                className="input"
                value={draft.product}
                onChange={(e) => setDraft({ ...draft, product: e.target.value as WorkbuddySettings['product'] })}
              >
                <option value="workbuddy">{t('workbuddy.productWorkbuddy')}</option>
                <option value="vscode">{t('workbuddy.productVscode')}</option>
                <option value="cli">{t('workbuddy.productCli')}</option>
              </select>
            </div>
            <div>
              <label className="label">{t('workbuddy.scrubMode')}</label>
              <select
                className="input"
                value={draft.scrub_mode}
                onChange={(e) => setDraft({ ...draft, scrub_mode: e.target.value as WorkbuddySettings['scrub_mode'] })}
              >
                <option value="off">{t('workbuddy.scrubOff')}</option>
                <option value="shadow">{t('workbuddy.scrubShadow')}</option>
                <option value="on">{t('workbuddy.scrubOn')}</option>
              </select>
            </div>
          </div>
          <p className="mt-1 text-xs text-muted">{t('workbuddy.scrubDefaultHint')}</p>
          {draft.scrub_mode === 'off' && (
            <p className="mt-1 text-xs text-amber-600 dark:text-amber-400">{t('workbuddy.scrubOffWarn')}</p>
          )}
          {draft.scrub_mode === 'on' && (
            <div className="warn-well mt-3 flex items-start gap-2 rounded-lg px-3 py-2 text-xs" style={{ color: 'var(--text)' }}>
              <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0 text-amber-600 dark:text-amber-400" />
              <span>{t('workbuddy.scrubOnWarn')}</span>
            </div>
          )}
          <label className="mt-3 flex items-start gap-2 text-sm">
            <input
              type="checkbox"
              className="mt-1 accent-violet-500"
              checked={draft.fingerprint}
              onChange={(e) => setDraft({ ...draft, fingerprint: e.target.checked })}
            />
            <span>
              {t('workbuddy.fingerprint')}
              <span className="block text-xs text-muted">{t('workbuddy.fingerprintHint')}</span>
            </span>
          </label>
          <div className="mt-3 flex justify-end">
            <button className="btn-primary" disabled={saving} onClick={saveSettings}>
              {saving ? t('common.loading') : t('workbuddy.save')}
            </button>
          </div>
        </div>
      )}

      {data.channels.length === 0 ? (
        /* 空态：没有任何 WorkBuddy/CodeBuddy 渠道，引导去渠道页 */
        <div className="card py-16 text-center">
          <Bot className="mx-auto mb-3 h-8 w-8 text-muted" />
          <p className="font-medium">{t('workbuddy.emptyTitle')}</p>
          <p className="mx-auto mt-1 max-w-md text-sm text-muted">{t('workbuddy.emptyHint')}</p>
          <button className="btn-primary mt-4" onClick={() => { location.hash = 'providers' }}>
            {t('workbuddy.goProviders')}
          </button>
        </div>
      ) : (
        <>
          {/* 渠道运行态表 */}
          <div className="card mb-4 overflow-x-auto p-0">
            <div className="flex items-center justify-between gap-2 px-4 py-3.5">
              <h3 className="text-sm font-medium">{t('workbuddy.channelsTitle')}</h3>
              <span className="text-xs text-muted">{t('workbuddy.channelsHint')}</span>
            </div>
            <table className="w-full text-sm">
              <thead>
                <tr className="thead-row text-left text-xs">
                  <th className="px-4 py-3">{t('workbuddy.colName')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colRealm')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colAccount')}</th>
                  <th className="px-4 py-3 text-right">{t('workbuddy.colCredits')}</th>
                  <th className="px-4 py-3 text-right">{t('workbuddy.colDailyTokens')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colStatus')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colCooldowns')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colLastOk')}</th>
                </tr>
              </thead>
              <tbody>
                {data.channels.map((ch) => {
                  const park = parkBadge(ch.parked_reason)
                  const cooldowns = Object.entries(ch.cooldowns ?? {})
                  return (
                    <tr key={ch.provider_id} className="tbody-row">
                      <td className="px-4 py-2.5">
                        <div className="flex items-center gap-2">
                          <span className="max-w-48 truncate font-medium" title={ch.name}>{ch.name}</span>
                          {ch.nickname && <span className="truncate text-xs text-muted" title={ch.nickname}>{ch.nickname}</span>}
                        </div>
                      </td>
                      <td className="px-4 py-2.5">
                        {ch.realm === 'intl' ? (
                          <span className="badge-blue">{t('workbuddy.realmIntl')}</span>
                        ) : ch.realm === 'cn' ? (
                          <span className="badge-zinc">{t('workbuddy.realmCn')}</span>
                        ) : (
                          <span className="text-muted">—</span>
                        )}
                      </td>
                      <td className="px-4 py-2.5"><span className="font-mono text-xs">{ch.account || '—'}</span></td>
                      <td className="px-4 py-2.5 text-right">
                        <div className="flex items-center justify-end gap-2">
                          {/* 从未观测到积分（credits_at 为空）时显示「—」而不是 0：
                              0 会被误读成「余额真的为 0」，而实际可能只是没有积分包
                              （实测国际版账号 Accounts=null）或还没刷新过。 */}
                          <span className="tabular-nums" title={ch.credits_at ? fmtTime(ch.credits_at) : undefined}>
                            {ch.credits_at ? fmtNum(ch.credits) : '—'}
                          </span>
                          <button
                            className="btn-ghost"
                            disabled={!!refreshing[ch.provider_id]}
                            onClick={() => refreshCredits(ch)}
                          >
                            {refreshing[ch.provider_id] ? t('common.loading') : t('workbuddy.refreshCredits')}
                          </button>
                        </div>
                      </td>
                      <td className="px-4 py-2.5 text-right">
                        <span className="tabular-nums">{fmtNum(ch.daily_tokens)}</span>
                        {ch.tokens_date && <span className="ml-1.5 text-xs text-muted">{ch.tokens_date}</span>}
                      </td>
                      <td className="px-4 py-2.5">
                        <span
                          className={park.cls}
                          title={
                            ch.parked_reason === 'cooldown'
                              ? t('workbuddy.parkCooldownUntil', { time: fmtTime(earliestCooldown(ch.cooldowns)) })
                              : t(park.key)
                          }
                        >
                          {t(park.key)}
                        </span>
                      </td>
                      <td className="px-4 py-2.5">
                        {cooldowns.length === 0 ? (
                          <span className="text-xs text-muted">{t('workbuddy.none')}</span>
                        ) : (
                          <div className="flex flex-wrap gap-1">
                            {cooldowns.map(([model, until]) => {
                              // 通配键 "*" = 上游风控（403/11140）给的整条渠道冷却，
                              // 不是某个模型 —— 直接展示 "*" 会让人看不懂。
                              const label = model === '*' ? t('workbuddy.cooldownWholeChannel') : model
                              return (
                                <span key={model} className="badge-zinc font-mono" title={t('workbuddy.cooldownUntil', { model: label, time: fmtTime(until) })}>
                                  {label} · {fmtTime(until)}
                                </span>
                              )
                            })}
                          </div>
                        )}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-xs text-muted">{fmtTime(ch.last_ok_at)}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>

          {/* 任务区 */}
          <div className="card mb-4">
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
              <h3 className="text-sm font-medium">{t('workbuddy.jobsTitle')}</h3>
              {!jobsAllowed && (
                <span className="flex items-center gap-1 text-xs text-amber-600 dark:text-amber-400">
                  <ShieldAlert className="h-3.5 w-3.5" />
                  {t('workbuddy.jobsPlatformOff')}
                </span>
              )}
            </div>
            <div className={`space-y-2 ${enabled ? '' : 'opacity-50'}`}>
              {JOB_KEYS.map((key) => {
                const high = HIGH_RISK_JOBS.has(key)
                const on = !!settings.jobs_enabled?.[key]
                const disabled = !enabled || !jobsAllowed
                return (
                  <div
                    key={key}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border px-3 py-2"
                    style={{ borderColor: 'var(--line)' }}
                  >
                    <label className={`flex items-center gap-2 text-sm ${high ? 'text-muted' : ''}`}>
                      <input
                        type="checkbox"
                        className="h-4 w-4 accent-violet-500"
                        checked={on}
                        disabled={disabled}
                        onChange={(e) => toggleJob(key, e.target.checked)}
                      />
                      <span>{t(JOB_LABELS[key])}</span>
                      {high && <span className="badge-yellow">{t('workbuddy.highRisk')}</span>}
                    </label>
                    <button
                      className="btn-ghost"
                      disabled={disabled || runningJob === key}
                      onClick={() => runNow(key)}
                    >
                      {runningJob === key ? t('common.loading') : t('workbuddy.runNow')}
                    </button>
                  </div>
                )
              })}
            </div>
            {!enabled && <p className="mt-2 text-xs text-muted">{t('workbuddy.jobsDisabledHint')}</p>}
          </div>

          {/* 执行日志表 */}
          <div className="card overflow-x-auto p-0">
            <div className="flex items-center justify-between gap-2 px-4 py-3.5">
              <h3 className="text-sm font-medium">{t('workbuddy.runsTitle')}</h3>
              <span className="text-xs text-muted">{t('workbuddy.runsHint')}</span>
            </div>
            <table className="w-full text-sm">
              <thead>
                <tr className="thead-row text-left text-xs">
                  <th className="px-4 py-3">{t('workbuddy.colJob')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colProvider')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colTrigger')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colStatus')}</th>
                  <th className="px-4 py-3">{t('workbuddy.colMessage')}</th>
                  <th className="px-4 py-3 text-right">{t('workbuddy.colDuration')}</th>
                </tr>
              </thead>
              <tbody>
                {data.runs.map((r, i) => {
                  const st = runStatus(r.status)
                  const jobKey = r.job_key as WorkbuddyJobKey
                  const jobLabel = JOB_LABELS[jobKey] ? t(JOB_LABELS[jobKey]) : r.job_key
                  return (
                    <tr key={`${r.job_key}-${r.provider_id}-${r.started_at}-${i}`} className="tbody-row">
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <span className="badge-zinc">{jobLabel}</span>
                      </td>
                      <td className="px-4 py-2.5"><span className="max-w-48 truncate" title={channelName(r.provider_id)}>{channelName(r.provider_id)}</span></td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-xs text-muted">
                        {r.trigger === 'schedule' ? t('workbuddy.triggerSchedule') : r.trigger === 'manual' ? t('workbuddy.triggerManual') : r.trigger}
                      </td>
                      <td className="px-4 py-2.5"><span className={st.cls}>{t(st.key)}</span></td>
                      <td className="max-w-md px-4 py-2.5">
                        <span className="block truncate text-xs text-muted" title={r.message}>{r.message || '—'}</span>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-right tabular-nums text-muted">
                        {fmtDuration(r.started_at, r.finished_at)}
                      </td>
                    </tr>
                  )
                })}
                {data.runs.length === 0 && (
                  <tr>
                    <td colSpan={6} className="px-4 py-10 text-center text-muted">{t('common.noData')}</td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  )
}
