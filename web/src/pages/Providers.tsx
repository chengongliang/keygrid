import { Fragment, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import i18n from '@/i18n'
import { api, ApiError, type BeginResult, type FetchModelsResult, type ModelPrice, type Provider, type ProvidersMeta, type ProviderPreset, type QuotaData, type QuotaSnapshot, type QuotaWindow, type TestModelResult } from '@/lib/api'
import ProviderLogo from '@/components/ProviderLogo'
import { Markdown } from '@/components/Markdown'
import { resolveProviderPlatform } from '@/lib/providerIcons'

// 兜底：meta 接口不可用时向导仍可用（纯手工填写）
const FALLBACK_OAUTH = ['kimi', 'openai', 'anthropic']
const FALLBACK_PROTOCOLS = ['openai', 'anthropic', 'gemini']

type WizardState = {
  step: 1 | 2 | 3
  kind: 'api_key' | 'oauth'
  presetKey: string // 选中的预设 key（'' = 自定义）
  name: string
  nameTouched: boolean // 展示名是否被手动编辑过：未编辑时切换渠道自动跟随预设 key
  protocol: string
  base_url: string
  oauth_provider: string
  api_key: string
  model_map_text: string
  priority: number
  use_proxy: boolean // 上游请求是否走平台代理（OpenAI 等预设默认勾选）
}

const emptyWizard: WizardState = {
  step: 1, kind: 'api_key', presetKey: '', name: '', nameTouched: false, protocol: 'openai', base_url: '',
  oauth_provider: 'kimi', api_key: '', model_map_text: '', priority: 0, use_proxy: false,
}

// 编辑表单：字段与新增向导保持一致（渠道类型 / OAuth provider 定型后不可改，仅展示）。
// api_key 留空 = 不轮换凭据。
type EditForm = {
  id: number
  kind: 'api_key' | 'oauth'
  oauth_provider: string
  name: string
  protocol: string
  base_url: string
  priority: number
  enabled: boolean
  authorized: boolean
  model_map_text: string
  billing_map: Record<string, string>
  api_key: string
  use_proxy: boolean
}

const toEditForm = (p: Provider): EditForm => ({
  id: p.id,
  kind: p.kind,
  oauth_provider: p.oauth_provider ?? '',
  name: p.name,
  protocol: p.protocol,
  base_url: p.base_url,
  priority: p.priority,
  enabled: p.enabled,
  // 旧后端不回 authorized 时按已授权处理（保持向后兼容，行为同修复前）
  authorized: p.authorized ?? p.kind !== 'oauth',
  model_map_text: p.model_map ? JSON.stringify(p.model_map, null, 0) : '',
  billing_map: p.billing_map ?? {},
  api_key: '',
  use_proxy: p.use_proxy ?? false,
})

const presetLabel = (p: ProviderPreset) =>
  p.kind === 'oauth' ? `OAuth · ${p.key}` : p.description || p.base_url

// ---- 模型勾选器 helpers ----
// model_map 文本是唯一事实来源：勾选器（复选框/手动添加/全选清空）都是对它的便捷编辑，
// 高级 JSON 文本可重命名（平台名→上游名），双向共用一份状态。

// 解析 model_map JSON 文本：空串 → {}；非法 JSON → null（由调用方提示修正）
const parseModelMap = (text: string): Record<string, string> | null => {
  const t = text.trim()
  if (!t) return {}
  try {
    const v = JSON.parse(t)
    if (v && typeof v === 'object' && !Array.isArray(v)) return v as Record<string, string>
    return null
  } catch { return null }
}

// 序列化 model_map：空对象 → ''（不提交空串 JSON）
const serializeMap = (m: Record<string, string>): string =>
  Object.keys(m).length ? JSON.stringify(m, null, 0) : ''

// 勾选/取消勾选一个模型名（保留已有映射值）
const toggleKey = (cur: Record<string, string>, k: string): Record<string, string> => {
  const n = { ...cur }
  if (n[k] !== undefined) delete n[k]
  else n[k] = k
  return n
}

// 耗时显示：≥1000ms 显示为秒（保留两位小数），否则毫秒
const fmtLatency = (ms: number): string => (ms >= 1000 ? `${(ms / 1000).toFixed(2)}s` : `${ms}ms`)

// finish_reason 中文映射（未识别的原样显示，胶囊 hover 保留英文原值）
// 模块级常量存 i18n key，渲染处统一 i18n.t()（非组件上下文）
const FINISH_LABELS: Record<string, string> = {
  stop: 'providers.finish.stop',
  length: 'providers.finish.length',
  tool_calls: 'providers.finish.toolCalls',
  function_call: 'providers.finish.functionCall',
  content_filter: 'providers.finish.contentFilter',
}
const finishLabel = (reason: string): string => {
  const k = FINISH_LABELS[reason]
  return k ? i18n.t(k) : reason
}

// 线协议友好名映射（卡片副标题；未识别的协议原样显示）
const PROTOCOL_LABELS: Record<string, string> = {
  openai: 'providers.protocol.openai',
  anthropic: 'providers.protocol.anthropic',
  gemini: 'providers.protocol.gemini',
}
const protocolLabel = (proto: string): string => {
  const k = PROTOCOL_LABELS[proto]
  return k ? i18n.t(k) : proto
}

// ---- 额度展示（openai codex /wham/usage 同步快照）----

// 窗口标签映射（weekly=周限额、5h=5小时窗口）；模块级常量存 i18n key
const QUOTA_WINDOW_LABELS: Record<string, string> = {
  '5h': 'providers.quota.win5h',
  daily: 'providers.quota.winDaily',
  weekly: 'providers.quota.winWeekly',
  monthly: 'providers.quota.winMonthly',
  yearly: 'providers.quota.winYearly',
}
const quotaWindowLabel = (w: QuotaWindow): string => {
  const k = w.window_label ? QUOTA_WINDOW_LABELS[w.window_label] : undefined
  return k ? i18n.t(k) : (w.window_label || i18n.t('providers.quota.winDefault'))
}

// 重置倒计时（reset_at unix 秒 → 相对时间）
const fmtResetIn = (resetAt?: number): string => {
  if (!resetAt) return ''
  const diff = resetAt * 1000 - Date.now()
  if (diff <= 0) return i18n.t('providers.quota.resetNow')
  const mins = Math.max(1, Math.round(diff / 60000))
  if (mins < 60) return i18n.t('providers.quota.resetInMinutes', { mins })
  const hours = mins / 60
  if (hours < 48) return i18n.t('providers.quota.resetInHours', { hours: Math.round(hours) })
  return i18n.t('providers.quota.resetInDays', { days: (hours / 24).toFixed(1) })
}

// 快照更新相对时间
const fmtAgo = (iso?: string): string => {
  if (!iso) return ''
  const diff = Date.now() - new Date(iso).getTime()
  if (diff < 0) return i18n.t('providers.quota.justNow')
  const mins = Math.floor(diff / 60000)
  if (mins < 1) return i18n.t('providers.quota.justNow')
  if (mins < 60) return i18n.t('providers.quota.minutesAgo', { mins })
  const hours = Math.floor(mins / 60)
  if (hours < 24) return i18n.t('providers.quota.hoursAgo', { hours })
  return i18n.t('providers.quota.daysAgo', { days: Math.floor(hours / 24) })
}

// ---- 预设测活类型（点选自动填充提示词与输出长度，免手输）----
// label/prompt 均存 i18n key，使用处 t() 解析（prompt 是发给模型的测试提示词，随界面语言切换）
const TEST_PRESETS: { key: string; label: string; prompt: string; maxTokens: number }[] = [
  { key: 'chat', label: 'providers.testPreset.chat', prompt: 'providers.testPrompt.chat', maxTokens: 128 },
  { key: 'knowledge', label: 'providers.testPreset.knowledge', prompt: 'providers.testPrompt.knowledge', maxTokens: 1024 },
  { key: 'math', label: 'providers.testPreset.math', prompt: 'providers.testPrompt.math', maxTokens: 1024 },
  { key: 'reason', label: 'providers.testPreset.reason', prompt: 'providers.testPrompt.reason', maxTokens: 2048 },
  { key: 'long', label: 'providers.testPreset.long', prompt: 'providers.testPrompt.long', maxTokens: 4096 },
  { key: 'code', label: 'providers.testPreset.code', prompt: 'providers.testPrompt.code', maxTokens: 4096 },
]


export default function Providers() {
  const { t } = useTranslation()
  const [list, setList] = useState<Provider[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [wiz, setWiz] = useState<WizardState | null>(null)
  const [editForm, setEditForm] = useState<EditForm | null>(null)
  // 额度快照（openai codex 渠道；GET /api/quota 读快照，手动刷新走 refresh 接口）
  const [quotaMap, setQuotaMap] = useState<Record<number, QuotaSnapshot>>({})
  const [quotaModal, setQuotaModal] = useState<Provider | null>(null)
  const [quotaRefreshing, setQuotaRefreshing] = useState(false)

  // 模型探测弹窗（拉取上游模型列表 + 单模型可用性测试）
  const [modelsModal, setModelsModal] = useState<Provider | null>(null)
  const [modelsRes, setModelsRes] = useState<FetchModelsResult | null>(null)
  const [fetchingModels, setFetchingModels] = useState(false)
  const [testTarget, setTestTarget] = useState('')
  const [testingModel, setTestingModel] = useState(false)
  const [testRes, setTestRes] = useState<TestModelResult | null>(null)
  // 在途请求代数：弹窗切换/重新拉取后，迟到的旧响应直接丢弃（上游探测最长挂 30s）
  const fetchGenRef = useRef(0)
  const testGenRef = useRef(0)
  // 模拟真实请求测试（可配提示词/max_tokens，流式/非流式）
  const [realPrompt, setRealPrompt] = useState(() => t(TEST_PRESETS[0].prompt))
  const [realMaxTokens, setRealMaxTokens] = useState(128)
  const [realTesting, setRealTesting] = useState<'stream' | 'nonstream' | null>(null)
  const [realRes, setRealRes] = useState<TestModelResult | null>(null)
  const realGenRef = useRef(0)
  // 流式测试的在途请求：停止按钮/关闭弹窗时 abort
  const streamAbortRef = useRef<AbortController | null>(null)
  const [realPreset, setRealPreset] = useState(TEST_PRESETS[0].key)
  const [realHistory, setRealHistory] = useState<{ typeLabel: string; res: TestModelResult }[]>([])
  // 配置时探测（probe_models）+ 手动添加模型输入
  const [probeRes, setProbeRes] = useState<FetchModelsResult | null>(null)
  const [probing, setProbing] = useState(false)
  const probeGenRef = useRef(0)
  const [manualModel, setManualModel] = useState('')

  // 预设模板（后端单一事实来源，失败时向导退化为手工填写）
  const [meta, setMeta] = useState<ProvidersMeta | null>(null)
  useEffect(() => {
    api.get<ProvidersMeta>('/api/providers/meta').then(setMeta).catch(() => {})
  }, [])

  // 计费：价格表只读（编辑弹窗「计费名映射」下拉用；空 = 未定价模型免费，无需映射）
  const [prices, setPrices] = useState<ModelPrice[]>([])
  useEffect(() => {
    api.get<ModelPrice[]>('/api/prices').then((d) => setPrices(d ?? [])).catch(() => {})
  }, [])

  const presets: ProviderPreset[] = meta?.presets ?? []
  const oauthKeys = meta?.oauth_keys ?? FALLBACK_OAUTH
  const protocols = meta?.protocols ?? FALLBACK_PROTOCOLS
  const oauthFlows = meta?.oauth_flows ?? {}
  // 平台已配置出口代理时才展示代理勾选框（展示脱敏后的具体地址）
  const proxyURL = meta?.proxy_url ?? ''
  const proxyConfigured = proxyURL !== ''
  const findPreset = (key: string) => presets.find((p) => p.key === key)

  // 选预设 → 预填表单（base_url/协议/优先级/模型映射/代理开关）
  const applyPreset = (key: string) => {
    const p = findPreset(key)
    if (!p) { setWiz({ ...wiz!, presetKey: '' }); return }
    resetProbe()
    setWiz({
      ...wiz!,
      presetKey: key,
      kind: p.kind,
      protocol: p.protocol,
      base_url: p.base_url,
      priority: p.default_priority || 0,
      // 展示名未手动编辑过则跟随预设（手动改过则尊重用户输入）
      name: wiz!.nameTouched ? wiz!.name : p.key,
      nameTouched: wiz!.nameTouched,
      oauth_provider: p.kind === 'oauth' ? p.key : wiz!.oauth_provider,
      // 需代理站点（如 OpenAI）默认勾选走平台代理；其他预设恢复未勾选
      use_proxy: p.needs_proxy ?? false,
      // 切换预设即切换平台：旧平台的模型映射对新平台无意义，重置为该预设的内置模型
      model_map_text: presetModelMap(p),
    })
  }

  // 预设内置模型 → 同名映射 JSON（空列表 → ''，不落 '{}' 垃圾）
  const presetModelMap = (p: ProviderPreset): string =>
    serializeMap(Object.fromEntries(p.models.map((m) => [m, m])))

  // OAuth Provider 下拉切换：与点预设卡片等效，同步重置该平台的内置模型映射
  const applyOauthProvider = (key: string) => {
    const p = findPreset(key)
    resetProbe()
    setWiz({
      ...wiz!,
      oauth_provider: key,
      presetKey: p?.key ?? '',
      protocol: p?.protocol ?? wiz!.protocol,
      // 与预设卡片一致：OAuth 渠道同样要落到完整上游端点（历史后端默认值曾缺
      // /codex/responses 尾段，见 relay.NormalizeCodexBaseURL）
      base_url: p?.base_url ?? '',
      // 展示名未手动编辑过则跟随新平台；无预设平台清掉旧自动名（placeholder 兜底）
      name: wiz!.nameTouched ? wiz!.name : (p?.key ?? ''),
      use_proxy: p?.needs_proxy ?? false,
      model_map_text: p ? presetModelMap(p) : '',
    })
  }

  // OAuth 授权中状态
  const [oauthFlow, setOauthFlow] = useState<{ providerId: number; begin: BeginResult } | null>(null)
  const [pasteUrl, setPasteUrl] = useState('')
  const [completingOauth, setCompletingOauth] = useState(false)
  const pollTimer = useRef<ReturnType<typeof setInterval> | null>(null)

  const load = () => {
    setLoading(true)
    api.get<Provider[]>('/api/providers')
      .then((d) => setList(d ?? []))
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
    // 额度快照独立加载（失败静默：额度条不展示即可，不影响渠道列表）
    api.get<QuotaSnapshot[]>('/api/quota')
      .then((d) => {
        const m: Record<number, QuotaSnapshot> = {}
        for (const q of d ?? []) m[q.provider_id] = q
        setQuotaMap(m)
      })
      .catch(() => {})
  }
  useEffect(load, [])

  // 组件卸载时停掉轮询
  useEffect(() => () => { if (pollTimer.current) clearInterval(pollTimer.current) }, [])

  const startOauthFlow = async (p: Provider) => {
    setErr('')
    try {
      const begin = await api.post<BeginResult>(`/api/providers/${p.id}/oauth/start`)
      setOauthFlow({ providerId: p.id, begin })
      // device_code 流：自动开始轮询；pkce 流：用户手动跳转后回来点"我已授权"
      if (begin.user_code) {
        const interval = ((begin.interval ?? 2) as number) * 1000
        pollTimer.current = setInterval(() => pollOauth(p.id, false), interval)
      }
    } catch (e: any) { setErr(e.message) }
  }

  const pollOauth = async (id: number, stopOnError = true) => {
    try {
      const r = await api.get<{ status: string }>(`/api/providers/${id}/oauth/poll`)
      if (r.status === 'ok') {
        stopPoll()
        setOauthFlow(null)
        load()
      }
    } catch (e: any) {
      if (stopOnError || e.status !== 502) { stopPoll(); setOauthFlow(null); if (stopOnError) setErr(e.message) }
    }
  }

  const stopPoll = () => { if (pollTimer.current) { clearInterval(pollTimer.current); pollTimer.current = null } }

  // 粘贴回调 URL 完成授权（Codex：回调落在用户本机 localhost，平台收不到）
  const completeOauth = async (id: number) => {
    if (!pasteUrl.trim()) return
    setCompletingOauth(true)
    setErr('')
    try {
      await api.post(`/api/providers/${id}/oauth/complete`, { redirect_url: pasteUrl.trim() })
      setOauthFlow(null)
      setPasteUrl('')
      load()
    } catch (e: any) { setErr(e.message) }
    finally { setCompletingOauth(false) }
  }

  // 手动刷新额度（POST /api/providers/{id}/quota/refresh）；refreshSliently=true 时不弹全局错误
  const refreshQuota = async (p: Provider, silently = false) => {
    setQuotaRefreshing(true)
    try {
      const snap = await api.post<QuotaSnapshot | null>(`/api/providers/${p.id}/quota/refresh`)
      if (snap) {
        setQuotaMap((m) => ({ ...m, [snap.provider_id]: snap }))
      } else if (!silently) {
        setErr(t('providers.errRateLimited'))
      }
    } catch (e: any) {
      if (!silently) setErr(e.message)
    } finally {
      setQuotaRefreshing(false)
    }
  }

  const toggle = async (p: Provider) => {
    await api.patch(`/api/providers/${p.id}`, { enabled: !p.enabled })
    load()
  }

  const remove = async (p: Provider) => {
    if (!confirm(t('providers.deleteConfirm', { name: p.name }))) return
    await api.del(`/api/providers/${p.id}`)
    load()
  }

  const submitWizard = async () => {
    if (!wiz) return
    setErr('')
    // 与勾选器同一套校验：必须是 JSON 对象（[]/"x" 等一律拒绝）
    const model_map = parseModelMap(wiz.model_map_text)
    if (model_map === null) {
      setErr(t('providers.errModelMapJson'))
      return
    }
    const body: Record<string, unknown> = {
      name: wiz.name, kind: wiz.kind, protocol: wiz.protocol,
      model_map, priority: wiz.priority,
      use_proxy: wiz.use_proxy,
    }
    if (wiz.kind === 'api_key') {
      body.base_url = wiz.base_url
      body.api_key = wiz.api_key
    } else {
      body.oauth_provider = wiz.oauth_provider
      if (wiz.base_url) body.base_url = wiz.base_url
    }
    try {
      const p = await api.post<Provider>('/api/providers', body)
      setWiz(null)
      load()
      if (p.kind === 'oauth') startOauthFlow(p)
    } catch (e: any) { setErr(e.message) }
  }

  const saveEdit = async () => {
    if (!editForm) return
    setErr('')
    // 与勾选器同一套校验：必须是 JSON 对象（[]/"x" 等一律拒绝）
    const model_map = parseModelMap(editForm.model_map_text)
    if (model_map === null) {
      setErr(t('providers.errModelMapJson'))
      return
    }
    const body: Record<string, unknown> = {
      name: editForm.name, protocol: editForm.protocol,
      priority: editForm.priority, enabled: editForm.enabled,
      use_proxy: editForm.use_proxy,
      // 始终提交（空对象 = 清空全部模型映射），保证取消勾选能真正生效
      model_map,
      // 计费名映射（P1.5）：空对象 = 清空；后端校验映射目标必须在价格表内
      billing_map: editForm.billing_map,
    }
    // base_url 两种渠道都提交：OAuth 渠道（尤其 Codex）同样要求完整上游端点，
    // 放开编辑用于修正历史默认值造成的错误地址（后端会归一化，空值回退默认）
    body.base_url = editForm.base_url
    // api_key 仅在填写时轮换凭据（后端约定：空/缺省 = 保留原凭据）
    if (editForm.kind === 'api_key' && editForm.api_key) body.api_key = editForm.api_key
    try {
      await api.patch(`/api/providers/${editForm.id}`, body)
      setEditForm(null)
      load()
    } catch (e: any) { setErr(e.message) }
  }

  // 拉取上游模型列表（GET /v1/models）
  const fetchModels = async (p: Provider) => {
    const gen = ++fetchGenRef.current
    setErr(''); setModelsRes(null); setTestRes(null); setTestTarget('')
    setFetchingModels(true)
    try {
      const r = await api.post<FetchModelsResult>(`/api/providers/${p.id}/models`)
      if (gen === fetchGenRef.current) setModelsRes(r)
    } catch (e: any) {
      if (gen === fetchGenRef.current) setErr(e.message)
    } finally {
      if (gen === fetchGenRef.current) setFetchingModels(false)
    }
  }

  // 测试指定模型可用性（向上游发 max_tokens=1 的最小请求）
  const testModelAvail = async (p: Provider, model: string) => {
    if (!model.trim()) return
    const gen = ++testGenRef.current
    setErr(''); setTestRes(null)
    setTestingModel(true)
    try {
      const r = await api.post<TestModelResult>(`/api/providers/${p.id}/test_model`, { model })
      if (gen === testGenRef.current) setTestRes(r)
    } catch (e: any) {
      if (gen === testGenRef.current) setErr(e.message)
    } finally {
      if (gen === testGenRef.current) setTestingModel(false)
    }
  }

  // 模拟真实请求测试（预设类型免手输；流式/非流式）
  const applyTestPreset = (key: string) => {
    const tp = TEST_PRESETS.find((p) => p.key === key)
    if (!tp) return
    setRealPreset(key); setRealPrompt(t(tp.prompt)); setRealMaxTokens(tp.maxTokens)
  }

  // 按提示词反查类型标签（手动编辑过则显示「自定义」）；返回 i18n key，渲染处 t()
  const presetLabelOf = (prompt: string): string =>
    TEST_PRESETS.find((tp) => t(tp.prompt) === prompt)?.label ?? 'providers.testPreset.custom'

  // 停止进行中的流式测试（保留已接收内容）
  const stopStreamTest = () => {
    streamAbortRef.current?.abort()
    streamAbortRef.current = null
  }

  const testModelReal = async (p: Provider, stream: boolean) => {
    if (!testTarget.trim()) return
    const gen = ++realGenRef.current
    const typeLabel = presetLabelOf(realPrompt)
    setErr(''); setRealRes(null)
    setRealTesting(stream ? 'stream' : 'nonstream')
    try {
      if (stream) {
        await testModelRealStream(p, gen, typeLabel)
        return
      }
      const r = await api.post<TestModelResult>(`/api/providers/${p.id}/test_model`, {
        model: testTarget, stream, prompt: realPrompt, max_tokens: realMaxTokens,
      })
      if (gen === realGenRef.current) {
        setRealRes(r)
        setRealHistory((h) => [{ typeLabel, res: r }, ...h].slice(0, 8))
      }
    } catch (e: any) {
      if (gen === realGenRef.current) setErr(e.message)
    } finally {
      if (gen === realGenRef.current) setRealTesting(null)
    }
  }

  // 流式测试：请求 SSE 端点，逐帧解析 delta 实时累积内容（打字机），
  // done 帧落地最终统计；用户停止/弹窗关闭时 abort，保留已接收部分。
  const testModelRealStream = async (p: Provider, gen: number, typeLabel: string) => {
    const controller = new AbortController()
    streamAbortRef.current = controller
    let content = ''
    let reasoning = ''
    const model = testTarget
    let startedAt = 0 // 上游响应开始后置为 performance.now()（流式中总耗时实时跳动）
    let firstTokenMs = 0
    const applyDelta = () => {
      if (gen !== realGenRef.current) return
      setRealRes({
        ok: true, status: 200,
        latency_ms: startedAt ? Math.round(performance.now() - startedAt) : 0,
        first_token_ms: firstTokenMs || undefined,
        model, stream: true,
        prompt: realPrompt, content, reasoning_content: reasoning || undefined, channel_name: p.name,
      })
    }
    const settle = (firstTokenMs: number, latencyMs: number, stopped: boolean, stats?: Partial<TestModelResult>) => {
      if (gen !== realGenRef.current) return
      const r: TestModelResult = {
        ok: true, status: 200, latency_ms: latencyMs, first_token_ms: firstTokenMs || undefined,
        model, stream: true, prompt: realPrompt, content, reasoning_content: reasoning || undefined,
        channel_name: p.name, error: stopped ? i18n.t('providers.models.stopped') : undefined,
        ...stats,
      }
      setRealRes(r)
      setRealHistory((h) => [{ typeLabel, res: r }, ...h].slice(0, 8))
    }
    try {
      const token = localStorage.getItem('token')
      const res = await fetch(`/api/providers/${p.id}/test_model_stream`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
        body: JSON.stringify({ model, stream: true, prompt: realPrompt, max_tokens: realMaxTokens }),
        signal: controller.signal,
      })
      const ctype = res.headers.get('content-type') ?? ''
      if (!res.ok || !ctype.includes('text/event-stream')) {
        // 渠道不存在/参数/凭据错误仍走普通 JSON 响应
        const data = await res.json().catch(() => ({}))
        throw new ApiError(res.status, data.code ?? res.status, data.data?.error ?? data.message ?? res.statusText)
      }
      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      startedAt = performance.now()
      let buf = ''
      let settled = false
      outer: for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        for (;;) {
          const idx = buf.indexOf('\n\n')
          if (idx < 0) break
          const frame = buf.slice(0, idx)
          buf = buf.slice(idx + 2)
          const line = frame.trim()
          if (!line.startsWith('data:')) continue
          let ev: { type?: string; content_delta?: string; reasoning_delta?: string } & Partial<TestModelResult>
          try { ev = JSON.parse(line.slice(5).trim()) } catch { continue }
          if (ev.type === 'delta') {
            if (!firstTokenMs) firstTokenMs = Math.round(performance.now() - startedAt)
            content += ev.content_delta ?? ''
            reasoning += ev.reasoning_delta ?? ''
            applyDelta()
          } else if (ev.type === 'done') {
            settled = true
            settle(firstTokenMs, ev.latency_ms ?? Math.round(performance.now() - startedAt), false, {
              ok: ev.ok ?? true, status: ev.status, finish_reason: ev.finish_reason,
              prompt_tokens: ev.prompt_tokens, completion_tokens: ev.completion_tokens,
              error: ev.error || undefined,
            })
            break outer
          }
        }
      }
      // 流被意外截断且未收到 done：以已收内容收尾
      if (!settled) settle(firstTokenMs, Math.round(performance.now() - startedAt), true)
    } catch (e: any) {
      if (e?.name === 'AbortError') {
        // 用户主动停止：保留已接收内容（无内容时不落历史）
        if (content || reasoning) settle(0, 0, true)
        return
      }
      throw e
    } finally {
      if (streamAbortRef.current === controller) streamAbortRef.current = null
    }
  }

  // 关闭模型弹窗：递增全部在途代数 + 清空状态（流式在途请求一并中断）
  const closeModelsModal = () => {
    fetchGenRef.current++; testGenRef.current++; realGenRef.current++
    streamAbortRef.current?.abort()
    streamAbortRef.current = null
    setModelsModal(null); setModelsRes(null); setTestRes(null); setTestTarget('')
    setRealRes(null); setRealTesting(null); setRealHistory([])
  }

  // 打开模型弹窗时自动拉取一次（依赖 id：同一渠道不重复触发）
  useEffect(() => {
    if (modelsModal) fetchModels(modelsModal)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelsModal?.id])

  // 配置时探测上游模型列表（probe_models）。
  // 新增场景传 base_url+api_key（渠道未落库）；编辑场景传 provider_id（用存储凭据）。
  const probeModels = async (payload: { base_url?: string; api_key?: string; provider_id?: number; use_proxy?: boolean }) => {
    const gen = ++probeGenRef.current
    setErr(''); setProbeRes(null)
    setProbing(true)
    try {
      const r = await api.post<FetchModelsResult>('/api/providers/probe_models', payload)
      if (gen === probeGenRef.current) setProbeRes(r)
    } catch (e: any) {
      if (gen === probeGenRef.current) setErr(e.message)
    } finally {
      if (gen === probeGenRef.current) setProbing(false)
    }
  }

  // 重置探测区（打开/关闭弹窗、切换预设时调用）
  const resetProbe = () => {
    probeGenRef.current++
    setProbeRes(null); setProbing(false); setManualModel('')
  }

  // 对 model_map JSON 的一切结构化修改统一走 here：非法 JSON 时提示并保留用户输入
  const withMap = (text: string, op: (cur: Record<string, string>) => string): string => {
    const cur = parseModelMap(text)
    if (cur === null) { setErr(t('providers.errMapFormat')); return text }
    return op(cur)
  }

  // 手动添加模型（同名映射；已存在则忽略）
  const addManualKey = (text: string, name: string): string =>
    withMap(text, (cur) => {
      const k = name.trim()
      return !k || cur[k] !== undefined ? text : serializeMap({ ...cur, [k]: k })
    })

  // 禁用渠道沉底：启用的排前面（组内保持原有顺序），两组之间画一条分割线
  const sortedList = [...list].sort((a, b) => Number(b.enabled) - Number(a.enabled))

  return (
    <div>
      <div className="mb-4 flex items-center justify-between">
        <h2 className="text-lg font-semibold">{t('providers.title')}</h2>
        <button className="btn-primary" onClick={() => { resetProbe(); setWiz({ ...emptyWizard }) }}>{t('providers.addChannel')}</button>
      </div>
      {err && <div className="mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}

      {loading ? <div className="text-muted">{t('common.loading')}</div> : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 lg:grid-cols-3">
          {sortedList.map((p, i) => {
            const prev = i > 0 ? sortedList[i - 1] : undefined
            // 第一条禁用渠道前插入分割线（排序保证只出现一次，且仅当上方还有启用渠道时）
            const showDivider = !!prev?.enabled && !p.enabled
            return (
              <Fragment key={p.id}>
                {showDivider && (
                  <div className="col-span-full flex items-center gap-3 pt-2">
                    <span className="shrink-0 text-xs text-muted">{t('providers.disabledSection')}</span>
                    <div className="h-px flex-1" style={{ background: 'var(--line)' }} />
                  </div>
                )}
            <div className="card flex flex-col gap-3">
              <div className="flex items-start justify-between gap-2">
                <div className="flex min-w-0 items-center gap-2.5">
                  <ProviderLogo platform={resolveProviderPlatform(p)} name={p.name} size={36} />
                  <div className="min-w-0">
                    <div className="truncate font-medium" title={p.name}>{p.name}</div>
                    <div className="mt-0.5 text-xs text-muted">
                      {p.kind === 'oauth' ? `OAuth · ${p.oauth_provider}` : 'API Key'} · {protocolLabel(p.protocol)}
                    </div>
                  </div>
                </div>
                {p.kind === 'oauth' && p.authorized === false
                  ? <span className="badge-yellow">{t('providers.pendingAuth')}</span>
                  : p.enabled ? <span className="badge-green">active</span> : <span className="badge-zinc">disabled</span>}
              </div>
              <div className="flex flex-wrap items-center gap-1">
                {p.use_proxy && <span className="badge-blue" title={t('providers.proxyBadgeTitle')}>{t('providers.proxyBadge')}</span>}
                <span className="badge-zinc">{t('providers.priorityBadge', { n: p.priority })}</span>
              </div>
              {/* 额度快照（openai codex 渠道；同步失败展示错误行） */}
              {quotaMap[p.id] && (
                <QuotaCard snap={quotaMap[p.id]} onOpen={() => { resetProbe(); setQuotaModal(p) }} />
              )}
              <div className="truncate text-xs text-muted" title={p.base_url}>{p.base_url}</div>
              {p.model_map && Object.keys(p.model_map).length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {Object.keys(p.model_map).slice(0, 4).map((m) => (
                    <span key={m} className="badge-zinc">{m}</span>
                  ))}
                  {Object.keys(p.model_map).length > 4 && <span className="badge-zinc">+{Object.keys(p.model_map).length - 4}</span>}
                </div>
              )}
              <div className="mt-auto flex flex-wrap gap-1.5 text-xs">
                <button className="btn-ghost" onClick={() => setModelsModal(p)}>{t('common.model')}</button>
                {p.kind === 'oauth' && p.oauth_provider === 'openai' && (
                  <button className="btn-ghost" onClick={() => { resetProbe(); setQuotaModal(p) }}>{t('providers.quotaBtn')}</button>
                )}
                <button className="btn-ghost" onClick={() => { resetProbe(); setEditForm(toEditForm(p)) }}>{t('common.edit')}</button>
                {p.kind === 'oauth' && p.authorized === false ? (
                  // 未完成授权：启停无意义（无凭据不参与转发），主操作是继续授权
                  <button className="btn-ghost" onClick={() => startOauthFlow(p)}>{t('providers.continueAuth')}</button>
                ) : (
                  <>
                    <button className="btn-ghost" onClick={() => toggle(p)}>{p.enabled ? t('common.disable') : t('common.enable')}</button>
                    {p.kind === 'oauth' && (
                      <button className="btn-ghost" onClick={() => startOauthFlow(p)}>{t('providers.reauth')}</button>
                    )}
                  </>
                )}
                <button className="btn-danger" onClick={() => remove(p)}>{t('common.delete')}</button>
              </div>
            </div>
              </Fragment>
            )
          })}
          {!loading && list.length === 0 && (
            <div className="col-span-full rounded-xl border border-dashed p-10 text-center text-muted" style={{ borderColor: 'var(--line)' }}>
              {t('providers.empty')}
            </div>
          )}
        </div>
      )}

      {/* 添加向导弹窗 */}
      {wiz && (
        <Modal onClose={() => { resetProbe(); setWiz(null) }} wide title={t('providers.addChannel')}>
          {wiz.step === 1 && (
            <div className="grid grid-cols-2 gap-3">
              {/* 切换渠道类型：上个类型遗留的展示名/模型映射/探测结果一并清掉 */}
              <button className="card text-left hover:border-violet-700" onClick={() => { resetProbe(); setWiz({ ...wiz, kind: 'api_key', step: 2, ...(wiz.kind !== 'api_key' ? { name: '', nameTouched: false, model_map_text: '', base_url: '' } : {}) }) }}>
                <div className="font-medium">API Key</div>
                <div className="mt-1 text-xs text-muted">{t('providers.wizard.typeApiKeyDesc')}</div>
              </button>
              <button className="card text-left hover:border-violet-700" onClick={() => { resetProbe(); setWiz({ ...wiz, kind: 'oauth', step: 2, ...(wiz.kind !== 'oauth' ? { name: '', nameTouched: false, model_map_text: '', base_url: '' } : {}) }) }}>
                <div className="font-medium">{t('providers.wizard.typeOauth')}</div>
                <div className="mt-1 text-xs text-muted">{t('providers.wizard.typeOauthDesc')}</div>
              </button>
            </div>
          )}
          {wiz.step === 2 && (
            <div className="space-y-4">
              {/* 预设快选（来自 /api/providers/meta；接口不可用时隐藏） */}
              {presets.length > 0 && (
                <div>
                  <label className="label">{t('providers.wizard.quickPick')}{wiz.kind === 'oauth' ? t('providers.wizard.quickPickOauth') : t('providers.wizard.quickPickApi')}</label>
                  <div className="grid max-h-56 grid-cols-1 gap-1.5 overflow-y-auto sm:grid-cols-3">
                    {presets
                      .filter((p) => p.kind === wiz.kind)
                      .map((p) => (
                        <button
                          key={p.key}
                          className={`flex items-center gap-2 rounded-lg border px-2.5 py-1.5 text-left text-xs transition-colors ${wiz.presetKey === p.key ? 'border-violet-600 bg-violet-50 dark:bg-violet-950/40' : ''}`}
                          style={{ borderColor: wiz.presetKey === p.key ? undefined : 'var(--line)' }}
                          onClick={() => applyPreset(p.key)}
                          title={presetLabel(p)}
                        >
                          <ProviderLogo platform={p.key} name={p.key} size={22} />
                          <div className="min-w-0">
                            <div className="font-medium">{p.key}</div>
                            {p.description && <div className="truncate text-muted">{p.description}</div>}
                          </div>
                        </button>
                      ))}
                  </div>
                </div>
              )}
              <div>
                <label className="label">{t('providers.nameLabel')}</label>
                <input className="input" value={wiz.name} onChange={(e) => setWiz({ ...wiz, name: e.target.value, nameTouched: true })} placeholder={wiz.kind === 'oauth' ? t('providers.wizard.namePhOauth') : t('providers.wizard.namePhApi')} />
              </div>
              {wiz.kind === 'oauth' && (
                <div>
                  <label className="label">OAuth Provider *</label>
                  <select className="input" value={wiz.oauth_provider} onChange={(e) => applyOauthProvider(e.target.value)}>
                    {oauthKeys.map((k) => (
                      <option key={k} value={k}>{k}{oauthFlows[k] ? ` (${oauthFlows[k]})` : ''}</option>
                    ))}
                  </select>
                  {(() => { const p = findPreset(wiz.oauth_provider); return p?.auth_mode ? (
                    <p className="mt-1 text-xs text-muted">🔐 {p.auth_mode}</p>
                  ) : null })()}
                </div>
              )}
              <div>
                <label className="label">{wiz.kind === 'oauth' ? t('providers.wizard.baseUrlOauthLabel') : t('providers.wizard.baseUrlLabel')}</label>
                <input className="input" value={wiz.base_url} onChange={(e) => setWiz({ ...wiz, base_url: e.target.value })} placeholder={wiz.kind === 'oauth' ? 'https://chatgpt.com/backend-api/codex/responses' : 'https://api.deepseek.com'} />
                {wiz.kind === 'oauth' && <p className="mt-1 text-xs text-muted">{t('providers.wizard.baseUrlOauthHint')}</p>}
              </div>
              {(() => { const p = findPreset(wiz.presetKey) || findPreset(wiz.oauth_provider); return p ? (
                <p className="text-xs text-muted">
                  {p.api_key_url && <>🔑 <a className="underline" href={p.api_key_url} target="_blank" rel="noreferrer">{t('providers.wizard.getApiKey')}</a> · </>}
                  {p.models.length > 0 && <>📦 {t('providers.wizard.builtinModels', { models: p.models.slice(0, 5).join(' / ') })}{p.models.length > 5 ? t('providers.wizard.modelsMore', { count: p.models.length }) : ''}</>}
                </p>
              ) : null })()}
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="label">{t('providers.protocolLabel')}</label>
                  <select className="input" value={wiz.protocol} onChange={(e) => setWiz({ ...wiz, protocol: e.target.value })}>
                    {protocols.map((p) => <option key={p}>{p}</option>)}
                  </select>
                </div>
                <div>
                  <label className="label">{t('providers.priorityLabel')}</label>
                  <input className="input" type="number" value={wiz.priority} onChange={(e) => setWiz({ ...wiz, priority: Number(e.target.value) })} />
                </div>
              </div>
              {proxyConfigured && (
                <label className="flex items-start gap-2 text-sm">
                  <input id="wiz-proxy" type="checkbox" className="mt-1 accent-violet-500" checked={wiz.use_proxy}
                    onChange={(e) => setWiz({ ...wiz, use_proxy: e.target.checked })} />
                  <span>
                    {t('providers.proxyAccess')}
                    <span className="block text-xs text-muted">
                      <span className="font-mono">{proxyURL}</span> · {t('providers.proxyAdminNote')}
                    </span>
                  </span>
                </label>
              )}
              {wiz.kind === 'api_key' && (
                <div>
                  <label className="label">{t('providers.wizard.apiKeyLabel')}</label>
                  <input className="input" type="password" value={wiz.api_key} onChange={(e) => setWiz({ ...wiz, api_key: e.target.value })} placeholder="sk-…" />
                </div>
              )}
              <ModelPicker
                canFetch={wiz.kind === 'api_key' && !!wiz.base_url.trim() && !!wiz.api_key.trim()}
                fetchHint={t('providers.wizard.fetchHint')}
                fetching={probing}
                result={probeRes}
                onFetch={() => probeModels({ base_url: wiz.base_url, api_key: wiz.api_key, use_proxy: wiz.use_proxy })}
                selected={parseModelMap(wiz.model_map_text) ?? {}}
                onToggle={(m) => setWiz({ ...wiz, model_map_text: withMap(wiz.model_map_text, (cur) => serializeMap(toggleKey(cur, m))) })}
                onSelectAll={(names) => setWiz({ ...wiz, model_map_text: withMap(wiz.model_map_text, (cur) => serializeMap({ ...cur, ...Object.fromEntries(names.map((n) => [n, n])) })) })}
                onClear={() => setWiz({ ...wiz, model_map_text: withMap(wiz.model_map_text, () => '') })}
                jsonText={wiz.model_map_text}
                onJsonTextChange={(t) => setWiz({ ...wiz, model_map_text: t })}
                manual={manualModel}
                onManualChange={setManualModel}
                onManualAdd={() => { setWiz({ ...wiz, model_map_text: addManualKey(wiz.model_map_text, manualModel) }); setManualModel('') }}
              />
              <div className="flex justify-between">
                <button className="btn-ghost" onClick={() => setWiz({ ...wiz, step: 1 })}>{t('providers.wizard.prevStep')}</button>
                <button className="btn-primary" disabled={!wiz.name} onClick={submitWizard}>
                  {wiz.kind === 'oauth' ? t('providers.wizard.createAndAuth') : t('common.create')}
                </button>
              </div>
            </div>
          )}
        </Modal>
      )}

      {/* OAuth 授权弹窗 */}
      {oauthFlow && (
        <Modal onClose={() => { stopPoll(); setOauthFlow(null) }} title={t('providers.oauth.title')}>
          {oauthFlow.begin.user_code ? (
            <div className="space-y-4 text-center">
              <p className="text-sm text-muted">{t('providers.oauth.step1Device')}</p>
              <a className="btn-primary w-full" href={oauthFlow.begin.verification_uri_complete ?? oauthFlow.begin.verification_uri} target="_blank" rel="noreferrer">
                {t('providers.oauth.openHost', { host: new URL(oauthFlow.begin.verification_uri ?? 'https://example.com').host })}
              </a>
              <p className="text-sm text-muted">{t('providers.oauth.step2Device')}</p>
              <div className="mono-well rounded-lg py-3 font-mono text-2xl tracking-widest text-emerald-600 dark:text-emerald-400">
                {oauthFlow.begin.user_code}
              </div>
              <p className="flex items-center justify-center gap-2 text-sm text-muted">
                <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-t-violet-500" />
                {t('providers.oauth.waiting')}
              </p>
            </div>
          ) : (
            <div className="space-y-4 text-center">
              <p className="text-sm text-muted">{t('providers.oauth.step1Browser')}</p>
              <a className="btn-primary w-full" href={oauthFlow.begin.authorize_url} target="_blank" rel="noreferrer">
                {t('providers.oauth.openAuthPage')}
              </a>
              {oauthFlow.begin.redirect_uri ? (
                <>
                  <p className="text-sm text-muted">
                    {t('providers.oauth.step2PastePre')} <code>{oauthFlow.begin.redirect_uri}</code>
                    {t('providers.oauth.step2PastePost')}
                  </p>
                  <input
                    className="input font-mono text-xs"
                    value={pasteUrl}
                    onChange={(e) => setPasteUrl(e.target.value)}
                    placeholder={`${oauthFlow.begin.redirect_uri}?code=…&state=…`}
                  />
                  <button
                    className="btn-primary w-full"
                    disabled={!pasteUrl.trim() || completingOauth}
                    onClick={() => completeOauth(oauthFlow.providerId)}
                  >
                    {completingOauth ? t('providers.oauth.submitting') : t('providers.oauth.done')}
                  </button>
                </>
              ) : (
                <>
                  <p className="text-sm text-muted">{t('providers.oauth.step2Poll')}</p>
                  <button className="btn-ghost w-full" onClick={() => pollOauth(oauthFlow.providerId)}>{t('providers.oauth.iHaveAuthorized')}</button>
                </>
              )}
            </div>
          )}
        </Modal>
      )}

      {/* 模型探测弹窗：拉取上游模型列表 + 测试单模型可用性 */}
      {modelsModal && (
        <Modal onClose={closeModelsModal} wide title={t('providers.models.title', { name: modelsModal.name })}>
          <div className="space-y-4">
            <div className="flex items-center justify-between gap-2">
              <p className="text-xs text-muted">{t('providers.models.subtitle')}</p>
              <button className="btn-ghost whitespace-nowrap" disabled={fetchingModels} onClick={() => fetchModels(modelsModal)}>
                {fetchingModels ? t('providers.fetching') : t('providers.fetchBtn')}
              </button>
            </div>
            {fetchingModels && (
              <p className="flex items-center gap-2 text-sm text-muted">
                <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-t-violet-500" />
                {t('providers.requestingUpstream')}
              </p>
            )}
            {!fetchingModels && modelsRes && (
              modelsRes.ok ? (
                modelsRes.models && modelsRes.models.length > 0 ? (
                  <div>
                    <p className="mb-1.5 text-xs text-muted">{t('providers.models.listCount', { count: modelsRes.models.length })}</p>
                    <div className="flex max-h-56 flex-wrap gap-1.5 overflow-y-auto">
                      {modelsRes.models.map((m) => (
                        <button
                          key={m}
                          className={`badge-zinc cursor-pointer font-mono ${testTarget === m ? 'text-violet-600 ring-1 ring-violet-500 dark:text-violet-400' : ''}`}
                          onClick={() => setTestTarget(m)}
                          title={t('providers.models.clickToSelect')}
                        >
                          {m}
                        </button>
                      ))}
                    </div>
                  </div>
                ) : (
                  <p className="text-sm text-muted">{t('providers.models.noneReturned')}</p>
                )
              ) : (
                <p className="err-well rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">
                  ❌ {modelsRes.error ?? t('providers.fetchFailed')}{modelsRes.status ? ` (HTTP ${modelsRes.status})` : ''}
                </p>
              )
            )}
            <div>
              <label className="label">{t('providers.models.availLabel')}</label>
              <div className="flex gap-2">
                <input
                  className="input flex-1 font-mono text-xs"
                  value={testTarget}
                  onChange={(e) => setTestTarget(e.target.value)}
                  placeholder={t('providers.models.availPlaceholder')}
                />
                <button
                  className="btn-primary-lg whitespace-nowrap"
                  disabled={!testTarget.trim() || testingModel}
                  onClick={() => testModelAvail(modelsModal, testTarget)}
                >
                  {testingModel ? t('providers.testing') : t('providers.models.quickTest')}
                </button>
              </div>
              {testingModel && (
                <p className="mt-2 flex items-center gap-2 text-sm text-muted">
                  <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-t-violet-500" />
                  {t('providers.models.minRequest')}
                </p>
              )}
              {testRes && (
                <p className={`mt-2 text-sm ${testRes.ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400'}`}>
                  {testRes.ok
                    ? t('providers.models.availOk', { model: testRes.model, status: testRes.status, latency: fmtLatency(testRes.latency_ms) })
                    : t('providers.models.availFail', { model: testRes.model, error: testRes.error ?? t('providers.unknownError') })}
                </p>
              )}
            </div>
            <div className="border-t pt-3" style={{ borderColor: 'var(--line)' }}>
              <label className="label">{t('providers.models.realLabel')}</label>
              <div className="flex flex-wrap gap-1.5">
                {TEST_PRESETS.map((tp) => (
                  <button
                    key={tp.key}
                    className={`badge-zinc cursor-pointer text-xs ${realPreset === tp.key ? 'text-violet-600 ring-1 ring-violet-500 dark:text-violet-400' : ''}`}
                    onClick={() => applyTestPreset(tp.key)}
                    title={t('providers.models.presetTitle', { prompt: t(tp.prompt) })}
                  >
                    {t(tp.label)}
                  </button>
                ))}
              </div>
              <textarea
                className="input mt-2 font-mono text-xs"
                rows={2}
                value={realPrompt}
                onChange={(e) => { setRealPrompt(e.target.value); setRealPreset('') }}
                placeholder={t('providers.models.promptPlaceholder')}
              />
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <label className="whitespace-nowrap text-xs text-muted">max_tokens</label>
                <input
                  className="input w-24 px-2 py-1 text-xs"
                  type="number"
                  min={1}
                  max={10240}
                  value={realMaxTokens}
                  onChange={(e) => { setRealMaxTokens(Math.max(1, Math.min(10240, Number(e.target.value) || 128))); setRealPreset('') }}
                />
                <div className="ml-auto flex gap-2">
                  <button
                    className="btn-ghost whitespace-nowrap"
                    disabled={!testTarget.trim() || !!realTesting}
                    onClick={() => testModelReal(modelsModal, false)}
                  >
                    {realTesting === 'nonstream' ? t('providers.testing') : t('providers.models.nonstreamTest')}
                  </button>
                  <button
                    className="btn-primary whitespace-nowrap"
                    disabled={!testTarget.trim() || (realTesting !== null && realTesting !== 'stream')}
                    onClick={() => (realTesting === 'stream' ? stopStreamTest() : testModelReal(modelsModal, true))}
                  >
                    {realTesting === 'stream' ? t('providers.models.stopTest') : t('providers.models.streamTest')}
                  </button>
                </div>
              </div>
              {!!realTesting && (
                <p className="mt-2 flex items-center gap-2 text-sm text-muted">
                  <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-t-violet-500" />
                  {t(realTesting === 'stream' ? 'providers.models.sendingRealStream' : 'providers.models.sendingRealNonstream')}
                </p>
              )}
              {realRes && (
                <div className="mt-2 space-y-2 rounded-lg border p-2.5 text-xs" style={{ borderColor: 'var(--line)' }}>
                  <div className="flex flex-wrap items-center gap-1.5">
                    {realTesting === 'stream' ? (
                      <span className="badge-zinc gap-1">
                        <span className="inline-block h-2.5 w-2.5 animate-spin rounded-full border-2 border-t-violet-500" />
                        {t('providers.models.streaming')}
                      </span>
                    ) : (
                      <span className={realRes.ok ? 'font-medium text-emerald-600 dark:text-emerald-400' : 'font-medium text-red-600 dark:text-red-400'}>
                        {realRes.ok ? t('providers.models.ok') : t('providers.models.fail')}
                      </span>
                    )}
                    <span className="badge-zinc">
                      <span className="badge-val">{realRes.model}</span>
                    </span>
                    <span className="badge-zinc">{realRes.stream ? t('providers.models.stream') : t('providers.models.nonstream')}</span>
                    <span className="badge-zinc gap-1">
                      {t('providers.models.totalLatency')}
                      <span className="badge-val">{fmtLatency(realRes.latency_ms)}</span>
                    </span>
                    {realRes.stream && !!realRes.first_token_ms && (
                      <span className="badge-zinc gap-1">
                        {t('providers.models.firstToken')}
                        <span className="badge-val">{fmtLatency(realRes.first_token_ms)}</span>
                      </span>
                    )}
                    {realRes.finish_reason && (
                      <span className="badge-zinc gap-1" title={`finish_reason: ${realRes.finish_reason}`}>
                        {t('providers.models.finishReason')}
                        <span className="badge-val">{finishLabel(realRes.finish_reason)}</span>
                      </span>
                    )}
                    {!!realRes.prompt_tokens && (
                      <span className="badge-zinc gap-1">
                        {t('providers.models.promptTokens')}
                        <span className="badge-val">{realRes.prompt_tokens} tok</span>
                      </span>
                    )}
                    {!!realRes.completion_tokens && (
                      <span className="badge-zinc gap-1">
                        {t('providers.models.completionTokens')}
                        <span className="badge-val">{realRes.completion_tokens} tok</span>
                      </span>
                    )}
                  </div>
                  {!realRes.ok && realRes.error && (
                    <p className="err-well max-h-24 overflow-y-auto whitespace-pre-wrap rounded-lg px-2.5 py-2 text-red-600 dark:text-red-400">
                      {realRes.error}
                    </p>
                  )}
                  {realRes.prompt && (
                    <div>
                      <p className="mb-0.5 text-muted">{t('providers.models.promptLabel')}</p>
                      <div className="mono-well max-h-20 overflow-y-auto whitespace-pre-wrap rounded-lg px-2.5 py-2">{realRes.prompt}</div>
                    </div>
                  )}
                  {!!realRes.reasoning_content && (
                    <details>
                      <summary className="cursor-pointer select-none text-muted">{t('providers.models.thinking', { count: realRes.reasoning_content.length })}</summary>
                      <div className="mono-well mt-1 max-h-40 overflow-y-auto whitespace-pre-wrap rounded-lg px-2.5 py-2">{realRes.reasoning_content}</div>
                    </details>
                  )}
                  {realRes.content && (
                    <div>
                      <p className="mb-0.5 text-muted">{t('providers.models.contentLabel')}</p>
                      <div className="mono-well max-h-48 overflow-y-auto rounded-lg px-2.5 py-2">
                        <Markdown>{realRes.content}</Markdown>
                      </div>
                    </div>
                  )}
                </div>
              )}
              {realHistory.length > 0 && (
                <div className="mt-2">
                  <p className="mb-1 text-xs text-muted">{t('providers.models.historyLabel')}</p>
                  <div className="max-h-40 overflow-y-auto rounded-lg border" style={{ borderColor: 'var(--line)' }}>
                    {realHistory.map((h, i) => (
                      <button
                        key={i}
                        className={`flex w-full items-center gap-2 border-b px-2 py-1.5 text-left text-xs last:border-b-0 ${realRes === h.res ? 'bg-violet-50 dark:bg-violet-950/40' : 'hover:bg-black/5 dark:hover:bg-white/5'}`}
                        style={{ borderColor: 'var(--line)' }}
                        onClick={() => setRealRes(h.res)}
                      >
                        <span className={`badge-zinc whitespace-nowrap ${h.res.ok ? '' : 'text-red-600 dark:text-red-400'}`}>{t(h.typeLabel)}</span>
                        <span className="whitespace-nowrap">{fmtLatency(h.res.latency_ms)}</span>
                        <span className="whitespace-nowrap">{h.res.completion_tokens ? `${h.res.completion_tokens} tok` : '-'}</span>
                        <span className="truncate text-muted">{(h.res.prompt ?? '').slice(0, 40)}</span>
                      </button>
                    ))}
                  </div>
                </div>
              )}
            </div>
          </div>
        </Modal>
      )}

      {/* 额度详情弹窗（openai codex /wham/usage 快照：周限额/短窗口/重置时间/重置次数） */}
      {quotaModal && (
        <QuotaModal
          p={quotaModal}
          snap={quotaMap[quotaModal.id]}
          refreshing={quotaRefreshing}
          onClose={() => setQuotaModal(null)}
          onRefresh={() => refreshQuota(quotaModal)}
        />
      )}

      {/* 编辑弹窗（字段与新增向导保持一致；渠道类型/OAuth provider 定型后不可改） */}
      {editForm && (
        <Modal onClose={() => { resetProbe(); setEditForm(null) }} title={t('providers.edit.title', { name: editForm.name })}>
          <div className="space-y-4">
            <div>
              <label className="label">{t('providers.nameLabel')}</label>
              <input className="input" value={editForm.name} onChange={(e) => setEditForm({ ...editForm, name: e.target.value })} placeholder={t('providers.wizard.namePhApi')} />
            </div>
            {editForm.kind === 'oauth' && (
              <div>
                <label className="label">OAuth Provider</label>
                <input className="input" value={editForm.oauth_provider} disabled />
                <p className="mt-1 text-xs text-muted">{t('providers.edit.oauthLocked')}</p>
              </div>
            )}
            <div>
              <label className="label">{editForm.kind === 'oauth' ? t('providers.wizard.baseUrlOauthLabel') : t('providers.wizard.baseUrlLabel')}</label>
              <input className="input" value={editForm.base_url} onChange={(e) => setEditForm({ ...editForm, base_url: e.target.value })} placeholder={editForm.kind === 'oauth' ? 'https://chatgpt.com/backend-api/codex/responses' : 'https://api.deepseek.com'} />
              {editForm.kind === 'oauth' && <p className="mt-1 text-xs text-muted">{t('providers.wizard.baseUrlOauthHint')}</p>}
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="label">{t('providers.protocolLabel')}</label>
                <select className="input" value={editForm.protocol} onChange={(e) => setEditForm({ ...editForm, protocol: e.target.value })}>
                  {protocols.map((p) => <option key={p}>{p}</option>)}
                </select>
              </div>
              <div>
                <label className="label">{t('providers.priorityLabel')}</label>
                <input className="input" type="number" value={editForm.priority} onChange={(e) => setEditForm({ ...editForm, priority: Number(e.target.value) })} />
              </div>
            </div>
            {/* 渠道已勾代理但平台配置被清空时仍展示，让用户能取消勾选自救 */}
            {(proxyConfigured || editForm.use_proxy) && (
              <label className="flex items-start gap-2 text-sm">
                <input id="ed-proxy" type="checkbox" className="mt-1 accent-violet-500" checked={editForm.use_proxy}
                  onChange={(e) => setEditForm({ ...editForm, use_proxy: e.target.checked })} />
                <span>
                  {t('providers.proxyAccess')}
                  <span className="block text-xs text-muted">
                    <span className="font-mono">{proxyURL}</span> · {t('providers.proxyAdminNote')}
                  </span>
                </span>
              </label>
            )}
            {editForm.kind === 'api_key' && (
              <div>
                <label className="label">{t('providers.edit.apiKeyLabel')}</label>
                <input className="input" type="password" value={editForm.api_key} onChange={(e) => setEditForm({ ...editForm, api_key: e.target.value })} placeholder="sk-…" />
              </div>
            )}
            <ModelPicker
              canFetch
              fetchHint=""
              fetching={probing}
              result={probeRes}
              onFetch={() => probeModels(editForm.kind === 'api_key' ? { provider_id: editForm.id, base_url: editForm.base_url, use_proxy: editForm.use_proxy } : { provider_id: editForm.id, use_proxy: editForm.use_proxy })}
              selected={parseModelMap(editForm.model_map_text) ?? {}}
              onToggle={(m) => setEditForm({ ...editForm, model_map_text: withMap(editForm.model_map_text, (cur) => serializeMap(toggleKey(cur, m))) })}
              onSelectAll={(names) => setEditForm({ ...editForm, model_map_text: withMap(editForm.model_map_text, (cur) => serializeMap({ ...cur, ...Object.fromEntries(names.map((n) => [n, n])) })) })}
              onClear={() => setEditForm({ ...editForm, model_map_text: withMap(editForm.model_map_text, () => '') })}
              jsonText={editForm.model_map_text}
              onJsonTextChange={(t) => setEditForm({ ...editForm, model_map_text: t })}
              manual={manualModel}
              onManualChange={setManualModel}
              onManualAdd={() => { setEditForm({ ...editForm, model_map_text: addManualKey(editForm.model_map_text, manualModel) }); setManualModel('') }}
            />
            {/* 计费名映射（P1.5）：渠道私有别名 → 价格表标准名；映射只做归一化，改不了价格 */}
            <BillingMapEditor
              value={editForm.billing_map}
              onChange={(m) => setEditForm({ ...editForm, billing_map: m })}
              prices={prices}
              modelMap={parseModelMap(editForm.model_map_text) ?? {}}
            />
            {/* 未授权 oauth 渠道不提供启停（后端 Update 同步拦截），完成授权后自动启用 */}
            {!(editForm.kind === 'oauth' && !editForm.authorized) && (
              <div className="flex items-center gap-2">
                <input id="ed-enabled" type="checkbox" checked={editForm.enabled} onChange={(e) => setEditForm({ ...editForm, enabled: e.target.checked })} />
                <label htmlFor="ed-enabled" className="text-sm">{t('providers.edit.enableChannel')}</label>
              </div>
            )}
            <div className="flex justify-end gap-2">
              <button className="btn-ghost" onClick={() => setEditForm(null)}>{t('common.cancel')}</button>
              <button className="btn-primary" disabled={!editForm.name} onClick={saveEdit}>{t('common.save')}</button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  )
}

// 计费名映射编辑器 —— 渠道私有别名（如 my-alias）→ 价格表标准名（deepseek-chat）。
// 映射只做名称归一化：目标只能从价格表下拉选（后端同校验），入口名已命中价格表时无需映射。
function BillingMapEditor(props: {
  value: Record<string, string>
  onChange: (m: Record<string, string>) => void
  prices: ModelPrice[]
  /** 当前渠道 model_map（提示哪些勾选模型未定价，可一键映射） */
  modelMap: Record<string, string>
}) {
  const { t } = useTranslation()
  const [newKey, setNewKey] = useState('')
  const entries = Object.entries(props.value)
  const priced = props.prices.filter((p) => p.model !== '*')
  const pricedSet = new Set(priced.map((p) => p.model))

  const addEntry = () => {
    const k = newKey.trim()
    if (!k || props.value[k] !== undefined) return
    props.onChange({ ...props.value, [k]: '' })
    setNewKey('')
  }
  // model_map 里未定价的模型一键带入（映射目标留空待选）
  const unpricedKeys = Object.keys(props.modelMap).filter((k) => !pricedSet.has(k) && props.value[k] === undefined)

  return (
    <div>
      <label className="label">{t('providers.billing.label')}</label>
      <p className="mb-2 text-xs text-muted">
        {t('providers.billing.descPre')} <span className="font-mono">my-alias</span>
        {' '}{t('providers.billing.descMid')} <span className="font-mono">deepseek-chat</span>
        {' '}{t('providers.billing.descPost')}
      </p>
      {entries.length > 0 && (
        <div className="mb-2 space-y-1.5">
          {entries.map(([k, v]) => (
            <div key={k} className="flex items-center gap-1.5">
              <input className="input-sm min-w-0 flex-1 font-mono text-xs" value={k} disabled title={t('providers.billing.renameTitle')} />
              <span className="shrink-0 text-xs text-muted">→</span>
              <select
                className="input-sm min-w-0 flex-1 font-mono text-xs"
                value={v}
                onChange={(e) => props.onChange({ ...props.value, [k]: e.target.value })}
              >
                <option value="" disabled>{t('providers.billing.pickPlaceholder')}</option>
                {priced.map((p) => <option key={p.model} value={p.model}>{p.model}</option>)}
              </select>
              <button className="btn-ghost h-6 shrink-0 px-1.5 text-red-500" title={t('providers.billing.deleteTitle')}
                onClick={() => { const n = { ...props.value }; delete n[k]; props.onChange(n) }}>✕</button>
            </div>
          ))}
          {entries.some(([, v]) => !v) && (
            <p className="text-xs" style={{ color: '#d97706' }}>{t('providers.billing.incomplete')}</p>
          )}
        </div>
      )}
      <div className="flex items-center gap-1.5">
        <input
          className="input-sm min-w-0 flex-1 font-mono text-xs"
          placeholder={t('providers.billing.inputPlaceholder')}
          value={newKey}
          onChange={(e) => setNewKey(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addEntry() } }}
        />
        <button className="btn-ghost h-7 shrink-0" onClick={addEntry} disabled={!newKey.trim()}>{t('providers.addBtn')}</button>
      </div>
      {unpricedKeys.length > 0 && priced.length > 0 && (
        <div className="mt-2 text-xs text-muted">
          {t('providers.billing.unpriced')}
          {unpricedKeys.map((k) => (
            <button key={k} className="badge badge-zinc ml-1 cursor-pointer hover:opacity-80" title={t('providers.billing.addAsMapping')}
              onClick={() => props.onChange({ ...props.value, [k]: '' })}>{k} +</button>
          ))}
        </div>
      )}
      {priced.length === 0 && (
        <p className="mt-2 text-xs text-muted">{t('providers.billing.noPrices')}</p>
      )}
    </div>
  )
}

export function Modal({ title, children, onClose, wide }: { title: string; children: React.ReactNode; onClose: () => void; wide?: boolean }) {
  return (
    <div className="fixed inset-0 z-[80] flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      {/* 限高滚动：超长内容（如上游原始错误）不撑破视口 */}
      <div className={`card modal-panel max-h-[85vh] w-full overflow-y-auto ${wide ? 'max-w-3xl' : 'max-w-md'}`} onClick={(e) => e.stopPropagation()}>
        <div className="mb-4 flex items-center justify-between">
          <h3 className="font-semibold">{title}</h3>
          <button className="text-muted hover:opacity-80" onClick={onClose}>✕</button>
        </div>
        {children}
      </div>
    </div>
  )
}

// ---- 额度展示组件（openai codex /wham/usage）----

// QuotaCard 渠道卡片上的紧凑额度区块：双窗口迷你进度条 + 订阅/重置次数徽标。
// 点击打开详情弹窗。
function QuotaCard({ snap, onOpen }: { snap: QuotaSnapshot; onOpen: () => void }) {
  const { t } = useTranslation()
  const rl = snap.status === 'ok' ? snap.data?.rate_limit : undefined
  return (
    <button className="w-full cursor-pointer rounded-lg border p-2 text-left transition-colors hover:border-violet-500" style={{ borderColor: 'var(--line)' }} onClick={onOpen} title={t('providers.quota.cardTitle')}>
      {snap.status === 'error' ? (
        <p className="text-xs text-red-600 dark:text-red-400">⚠ {t('providers.quota.syncFailed', { error: snap.error ?? t('providers.unknownError') })}</p>
      ) : !rl ? (
        <p className="text-xs text-muted">{t('providers.quota.noData')}</p>
      ) : (
        <div className="space-y-1.5">
          {rl.primary && <QuotaWindowRow label={quotaWindowLabel(rl.primary)} w={rl.primary} />}
          {rl.secondary && <QuotaWindowRow label={quotaWindowLabel(rl.secondary)} w={rl.secondary} />}
          <div className="flex flex-wrap items-center gap-1">
            {snap.data?.plan_type && <span className="badge-zinc uppercase">{snap.data.plan_type}</span>}
            {rl.limit_reached && <span className="badge-yellow">{t('providers.quota.limitReached')}</span>}
            {snap.data?.reset_credits_available != null && (
              <span className="badge-zinc" title={t('providers.quota.resetCreditsTitle')}>{t('providers.quota.resetCredits', { n: snap.data.reset_credits_available })}</span>
            )}
            <span className="ml-auto text-[10px] text-muted">{fmtAgo(snap.fetched_at)}{snap.source === 'relay' ? t('providers.quota.relaySuffix') : ''}</span>
          </div>
        </div>
      )}
    </button>
  )
}

// QuotaWindowRow 单窗口迷你进度条（label 为「本周」时用 primary 的数据）。
function QuotaWindowRow({ label, w }: { label: string; w: QuotaWindow }) {
  const pct = Math.max(0, Math.min(100, Math.round(w.used_percent)))
  const color = pct >= 85 ? 'bg-red-500' : pct >= 60 ? 'bg-amber-500' : 'bg-emerald-500'
  return (
    <div>
      <div className="flex items-center gap-2">
        <span className="w-14 shrink-0 text-xs text-muted">{label}</span>
        <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-black/10 dark:bg-white/10">
          <div className={`h-full rounded-full ${color}`} style={{ width: `${Math.max(2, pct)}%` }} />
        </div>
        <span className="w-9 shrink-0 text-right font-mono text-xs">{pct}%</span>
      </div>
      {w.reset_at ? <div className="mt-0.5 pl-16 text-[10px] text-muted">{fmtResetIn(w.reset_at)}</div> : null}
    </div>
  )
}

// QuotaWindowDetail 详情弹窗内的大进度条（带剩余额度与重置时间）。
function QuotaWindowDetail({ label, w }: { label: string; w: QuotaWindow }) {
  const { t } = useTranslation()
  const pct = Math.max(0, Math.min(100, Math.round(w.used_percent)))
  const color = pct >= 85 ? 'bg-red-500' : pct >= 60 ? 'bg-amber-500' : 'bg-emerald-500'
  return (
    <div>
      <div className="flex items-baseline justify-between">
        <span className="text-sm font-medium">{label}</span>
        <span className="font-mono text-sm">{pct}%</span>
      </div>
      <div className="mt-1 h-2 w-full overflow-hidden rounded-full bg-black/10 dark:bg-white/10">
        <div className={`h-full rounded-full ${color}`} style={{ width: `${Math.max(2, pct)}%` }} />
      </div>
      <div className="mt-1 flex justify-between text-xs text-muted">
        <span>{t('providers.quota.remaining', { n: Math.max(0, 100 - pct) })}</span>
        {w.reset_at ? <span>{fmtResetIn(w.reset_at)}</span> : null}
      </div>
    </div>
  )
}

// QuotaModal 额度详情弹窗：双窗口大进度条 + 重置次数 + credits + 模型级附加限额。
function QuotaModal({ p, snap, refreshing, onClose, onRefresh }: {
  p: Provider
  snap?: QuotaSnapshot
  refreshing: boolean
  onClose: () => void
  onRefresh: () => void
}) {
  const { t } = useTranslation()
  const d: QuotaData | undefined = snap?.status === 'ok' ? snap.data : undefined
  const rl = d?.rate_limit
  return (
    <Modal onClose={onClose} title={t('providers.quota.title', { name: p.name })}>
      {!snap ? (
        <div className="space-y-3 text-center">
          <p className="text-sm text-muted">{t('providers.quota.neverSynced')}</p>
          <button className="btn-primary w-full" disabled={refreshing} onClick={onRefresh}>
            {refreshing ? t('providers.quota.querying') : t('providers.quota.queryNow')}
          </button>
        </div>
      ) : (
        <div className="space-y-4">
          <div className="flex flex-wrap items-center gap-1.5 text-xs">
            {d?.plan_type && <span className="badge-zinc uppercase">{d.plan_type}</span>}
            <span className="badge-zinc">{snap.source === 'relay' ? t('providers.quota.relaySource') : t('providers.quota.proactiveSource')}</span>
            <span className="text-muted">{t('providers.quota.updatedAt', { ago: fmtAgo(snap.fetched_at) })}</span>
            <button className="btn-ghost ml-auto whitespace-nowrap" disabled={refreshing} onClick={onRefresh}>
              {refreshing ? t('providers.quota.querying') : t('common.refresh')}
            </button>
          </div>
          {snap.status === 'error' && (
            <p className="err-well rounded-lg px-2.5 py-2 text-xs text-red-600 dark:text-red-400">
              {t('providers.quota.lastSyncFailed', { error: snap.error ?? t('providers.unknownError') })}
            </p>
          )}
          {rl && (
            <div className="space-y-3 rounded-lg border p-3" style={{ borderColor: 'var(--line)' }}>
              {rl.primary && <QuotaWindowDetail label={quotaWindowLabel(rl.primary)} w={rl.primary} />}
              {rl.secondary && <QuotaWindowDetail label={quotaWindowLabel(rl.secondary)} w={rl.secondary} />}
              <div className="flex flex-wrap gap-1">
                {rl.limit_reached ? <span className="badge-yellow">{t('providers.quota.limitReached')}</span> : rl.allowed ? <span className="badge-green">{t('providers.quota.allowed')}</span> : null}
              </div>
            </div>
          )}
          <div className="flex flex-wrap gap-1.5 text-xs">
            {d?.reset_credits_available != null && (
              <span className="badge-zinc" title={t('providers.quota.resetCreditsTitle')}>{t('providers.quota.resetCreditsAvail', { n: d.reset_credits_available })}</span>
            )}
            {d?.credits && (d.credits.has_credits || !d.credits.unlimited) && (
              <span className="badge-zinc" title={t('providers.quota.creditsTitle')}>{t('providers.quota.credits', { balance: d.credits.unlimited ? t('providers.quota.creditsUnlimited') : d.credits.balance })}</span>
            )}
          </div>
          {(d?.additional?.length ?? 0) > 0 && (
            <div>
              <p className="mb-1 text-xs text-muted">{t('providers.quota.additionalLimits')}</p>
              <div className="space-y-1.5 rounded-lg border p-2" style={{ borderColor: 'var(--line)' }}>
                {d!.additional!.map((a) => (
                  <div key={a.limit_name}>
                    <div className="flex items-center justify-between text-xs">
                      <span className="font-mono">{a.limit_name}</span>
                      {a.rate_limit && (
                        <span className="text-muted">
                          {[
                            a.rate_limit.primary && `${quotaWindowLabel(a.rate_limit.primary)} ${Math.round(a.rate_limit.primary.used_percent)}%`,
                            a.rate_limit.secondary && `5h ${Math.round(a.rate_limit.secondary.used_percent)}%`,
                          ].filter(Boolean).join(' · ')}
                        </span>
                      )}
                    </div>
                    {a.rate_limit?.primary && (
                      <div className="mt-0.5 h-1 w-full overflow-hidden rounded-full bg-black/10 dark:bg-white/10">
                        <div
                          className={`h-full rounded-full ${a.rate_limit.primary.used_percent >= 85 ? 'bg-red-500' : a.rate_limit.primary.used_percent >= 60 ? 'bg-amber-500' : 'bg-emerald-500'}`}
                          style={{ width: `${Math.max(2, Math.min(100, Math.round(a.rate_limit.primary.used_percent)))}%` }}
                        />
                      </div>
                    )}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}
    </Modal>
  )
}

// 模型勾选器 —— 拉取上游模型列表后复选勾选；勾选项写入渠道 model_map（默认同名映射），
// 即网关 /v1/models 对外暴露的模型。高级 JSON 文本可重命名（平台名→上游名）。
function ModelPicker(props: {
  canFetch: boolean
  fetchHint: string
  fetching: boolean
  result: FetchModelsResult | null
  onFetch: () => void
  selected: Record<string, string>
  onToggle: (name: string) => void
  onSelectAll: (names: string[]) => void
  onClear: () => void
  jsonText: string
  onJsonTextChange: (t: string) => void
  manual: string
  onManualChange: (v: string) => void
  onManualAdd: () => void
}) {
  const { t } = useTranslation()
  const [filter, setFilter] = useState('')
  const fetched = props.result?.models ?? []
  const kw = filter.trim().toLowerCase()
  const match = (m: string) => !kw || m.toLowerCase().includes(kw)
  // 上游列表 + 已勾选但不在列表中的（手动添加/预设预填/重命名前的原名）
  const rows = [...fetched.filter(match), ...Object.keys(props.selected).filter((m) => !fetched.includes(m) && match(m))]

  return (
    <div>
      <div className="flex items-center justify-between">
        <label className="label">{t('providers.picker.label')}</label>
        <span className="text-xs text-muted">{t('providers.picker.enabledCount', { n: Object.keys(props.selected).length })}</span>
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        <button
          className="btn-ghost whitespace-nowrap"
          disabled={!props.canFetch || props.fetching}
          onClick={props.onFetch}
          title={props.canFetch ? t('providers.picker.fetchTitle') : props.fetchHint}
        >
          {props.fetching ? t('providers.fetching') : t('providers.fetchBtn')}
        </button>
        {fetched.length > 0 && (
          <>
            <button className="btn-ghost" onClick={() => props.onSelectAll(fetched)}>{t('providers.picker.selectAll')}</button>
            <button className="btn-ghost" onClick={props.onClear}>{t('providers.picker.clear')}</button>
            <input className="input-sm min-w-32 flex-1" placeholder={t('providers.picker.filterPlaceholder')} value={filter} onChange={(e) => setFilter(e.target.value)} />
          </>
        )}
      </div>
      {props.fetching && (
        <p className="mt-1.5 flex items-center gap-2 text-xs text-muted">
          <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-t-violet-500" />
          {t('providers.requestingUpstream')}
        </p>
      )}
      {!props.fetching && props.result && !props.result.ok && (
        <p className="err-well mt-1.5 rounded-lg px-2.5 py-1.5 text-xs text-red-600 dark:text-red-400">
          ❌ {props.result.error ?? t('providers.fetchFailed')}{props.result.status ? ` (HTTP ${props.result.status})` : ''}
        </p>
      )}
      {rows.length > 0 && (
        <div className="mt-1.5 max-h-48 overflow-y-auto rounded-lg border p-1" style={{ borderColor: 'var(--line)' }}>
          {rows.map((m) => (
            <label key={m} className="flex cursor-pointer items-center gap-2 rounded px-1.5 py-1 text-xs hover:bg-black/5 dark:hover:bg-white/5">
              <input type="checkbox" checked={props.selected[m] !== undefined} onChange={() => props.onToggle(m)} />
              <span className="font-mono">{m}</span>
              {props.selected[m] !== undefined && props.selected[m] !== m && (
                <span className="text-muted">→ {props.selected[m]}</span>
              )}
            </label>
          ))}
        </div>
      )}
      {!props.fetching && props.result?.ok && rows.length === 0 && (
        <p className="mt-1.5 text-xs text-muted">{t('providers.picker.noMatch')}</p>
      )}
      <div className="mt-1.5 flex gap-1.5">
        <input
          className="input flex-1 font-mono text-xs"
          placeholder={t('providers.picker.manualPlaceholder')}
          value={props.manual}
          onChange={(e) => props.onManualChange(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); props.onManualAdd() } }}
        />
        <button className="btn-ghost-lg whitespace-nowrap" disabled={!props.manual.trim()} onClick={props.onManualAdd}>{t('providers.addBtn')}</button>
      </div>
      <details className="mt-1.5">
        <summary className="cursor-pointer select-none text-xs text-muted">{t('providers.picker.advanced')}</summary>
        <textarea
          className="input mt-1 font-mono text-xs"
          rows={3}
          value={props.jsonText}
          onChange={(e) => props.onJsonTextChange(e.target.value)}
          placeholder='{"gpt-4o":"upstream-model"}'
        />
      </details>
    </div>
  )
}
