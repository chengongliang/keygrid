import { useEffect, useMemo, useRef, useState } from 'react'
import { Copy, Pencil, Settings2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api, type ApiKey, type Provider } from '@/lib/api'
import i18n from '@/i18n'
import { Modal } from './Providers'

// ---- 列设置（参考 new-api「查看」列切换，localStorage 记忆）----
// 列名 → i18n key（非组件常量存 key，渲染处统一 t()；名称/状态/模型复用 common 域）
type ColKey =
  | 'name' | 'status' | 'key' | 'models' | 'providers' | 'ip' | 'quota'
  | 'created' | 'last_used' | 'expires'

const COL_LABELS: Record<ColKey, string> = {
  name: 'common.name',
  status: 'common.status',
  key: 'keys.col.key',
  models: 'common.model',
  providers: 'keys.col.providers',
  ip: 'keys.col.ip',
  quota: 'keys.col.quota',
  created: 'keys.col.created',
  last_used: 'keys.col.lastUsed',
  expires: 'keys.col.expires',
}
const ALL_COLS = Object.keys(COL_LABELS) as ColKey[]
const COLS_LS_KEY = 'keygrid.apikeys.cols'

const colsAllOn = (): Record<ColKey, boolean> =>
  Object.fromEntries(ALL_COLS.map((c) => [c, true])) as Record<ColKey, boolean>

function loadCols(): Record<ColKey, boolean> {
  try {
    const saved = JSON.parse(localStorage.getItem(COLS_LS_KEY) || '{}')
    return { ...colsAllOn(), ...saved }
  } catch {
    return colsAllOn()
  }
}

// ---- 端点接入信息（多协议转发面）----
// OpenAI / Responses 入口的 Base URL 跟随当前部署地址（/v1 前缀）；
// Anthropic SDK 的 base_url 是站点根（SDK 自己拼 /v1/messages）。
// dev 下 vite 代理 /v1 → 后端，同样正确。
const SITE_ORIGIN = location.origin
const API_BASE = `${SITE_ORIGIN}/v1`

type ExProto = 'openai' | 'anthropic' | 'responses'
type ExTab = 'curl' | 'python' | 'js'

const PROTO_TABS: { v: ExProto; label: string }[] = [
  { v: 'openai', label: 'OpenAI' },
  { v: 'anthropic', label: 'Anthropic' },
  { v: 'responses', label: 'Responses' },
]

const EX_TABS: { v: ExTab; label: string }[] = [
  { v: 'curl', label: 'cURL' },
  { v: 'python', label: 'Python' },
  { v: 'js', label: 'Node.js' },
]

const exExample = (proto: ExProto, tab: ExTab) => {
  // 示例占位符跟随当前语言
  const yourKey = i18n.t('keys.example.yourKey')
  const modelName = i18n.t('keys.example.modelName')
  const hello = i18n.t('keys.example.hello')
  if (proto === 'anthropic') {
    // Anthropic SDK：base_url 是站点根（不带 /v1，SDK 自己拼 /v1/messages）
    switch (tab) {
      case 'curl':
        return `curl ${SITE_ORIGIN}/v1/messages \\\n  -H "x-api-key: ${yourKey}" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{
    "model": "${modelName}",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "${hello}"}]
  }'`
      case 'python':
        return `from anthropic import Anthropic

client = Anthropic(
    base_url="${SITE_ORIGIN}",
    api_key="${yourKey}",
)

msg = client.messages.create(
    model="${modelName}",
    max_tokens=1024,
    messages=[{"role": "user", "content": "${hello}"}],
)
print(msg.content[0].text)`
      case 'js':
        return `import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic({
  baseURL: "${SITE_ORIGIN}",
  apiKey: "${yourKey}",
});

const msg = await client.messages.create({
  model: "${modelName}",
  max_tokens: 1024,
  messages: [{ role: "user", content: "${hello}" }],
});
console.log(msg.content[0].text);`
    }
  }
  if (proto === 'responses') {
    switch (tab) {
      case 'curl':
        return `curl ${API_BASE}/responses \\\n  -H "Authorization: Bearer ${yourKey}" \\\n  -H "Content-Type: application/json" \\\n  -d '{
    "model": "${modelName}",
    "input": "${hello}"
  }'`
      case 'python':
        return `from openai import OpenAI

client = OpenAI(
    base_url="${API_BASE}",
    api_key="${yourKey}",
)

resp = client.responses.create(
    model="${modelName}",
    input="${hello}",
)
print(resp.output_text)`
      case 'js':
        return `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${API_BASE}",
  apiKey: "${yourKey}",
});

const resp = await client.responses.create({
  model: "${modelName}",
  input: "${hello}",
});
console.log(resp.output_text);`
    }
  }
  // openai chat/completions
  switch (tab) {
    case 'curl':
      return `curl ${API_BASE}/chat/completions \\\n  -H "Authorization: Bearer ${yourKey}" \\\n  -H "Content-Type: application/json" \\\n  -d '{
    "model": "${modelName}",
    "messages": [{"role": "user", "content": "${hello}"}]
  }'`
    case 'python':
      return `from openai import OpenAI

client = OpenAI(
    base_url="${API_BASE}",
    api_key="${yourKey}",
)

resp = client.chat.completions.create(
    model="${modelName}",
    messages=[{"role": "user", "content": "${hello}"}],
)
print(resp.choices[0].message.content)`
    case 'js':
      return `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${API_BASE}",
  apiKey: "${yourKey}",
});

const resp = await client.chat.completions.create({
  model: "${modelName}",
  messages: [{ role: "user", content: "${hello}" }],
});
console.log(resp.choices[0].message.content);`
  }
}

// ---- 小工具 ----

// RFC3339 → datetime-local 本地格式（YYYY-MM-DDTHH:mm）
const toLocalInput = (iso?: string | null) => {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// 渠道绑定（provider_limit）：逗号分隔渠道 ID ↔ number[]（非法项过滤）
const parseProviderLimit = (s?: string): number[] =>
  (s || '')
    .split(',')
    .map((x) => Number(x.trim()))
    .filter((n) => Number.isInteger(n) && n > 0)

export default function ApiKeys() {
  const { t } = useTranslation()
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [err, setErr] = useState('')
  const [copiedId, setCopiedId] = useState<number | null>(null)
  const [busy, setBusy] = useState(false)

  // 渠道列表（渠道绑定选择器 + 绑定列名称展示）
  const [providers, setProviders] = useState<Provider[]>([])
  const [providersLoaded, setProvidersLoaded] = useState(false)

  // 列显隐（查看）
  const [cols, setCols] = useState<Record<ColKey, boolean>>(loadCols)
  const [colMenu, setColMenu] = useState(false)
  const colMenuRef = useRef<HTMLDivElement>(null)

  // 新建
  const [creating, setCreating] = useState(false)
  const [nName, setNName] = useState('')
  const [nExpiresAt, setNExpiresAt] = useState('')
  const [nIP, setNIP] = useState('')
  const [nModels, setNModels] = useState('')
  const [nQuota, setNQuota] = useState('')
  const [nProv, setNProv] = useState<number[]>([])

  // 编辑
  const [editing, setEditing] = useState<ApiKey | null>(null)
  const [eName, setEName] = useState('')
  const [eEnabled, setEEnabled] = useState(true)
  const [eExpiresAt, setEExpiresAt] = useState('')
  const [eNoExpiry, setENoExpiry] = useState(true)
  const [eIP, setEIP] = useState('')
  const [eModels, setEModels] = useState('')
  const [eQuota, setEQuota] = useState('')
  const [eProv, setEProv] = useState<number[]>([])
  const [eResetQuota, setEResetQuota] = useState(false)

  // 创建成功弹窗
  const [created, setCreated] = useState<string | null>(null)
  const [createdCopied, setCreatedCopied] = useState(false)

  // 接入示例
  const [exProto, setExProto] = useState<ExProto>('openai')
  const [exTab, setExTab] = useState<ExTab>('curl')
  const [copiedBase, setCopiedBase] = useState(false)
  const [copiedEx, setCopiedEx] = useState(false)

  const load = () => {
    api.get<ApiKey[]>('/api/keys').then((d) => setKeys(d ?? [])).catch((e) => setErr(e.message))
    // 渠道列表：失败不标记 loaded —— 编辑保存时原样提交绑定，交由后端校验归属
    api.get<Provider[]>('/api/providers')
      .then((d) => { setProviders(d ?? []); setProvidersLoaded(true) })
      .catch((e) => setErr(e.message))
  }
  useEffect(load, [])

  // 点击外部关闭列设置
  useEffect(() => {
    if (!colMenu) return
    const close = (e: MouseEvent) => {
      if (colMenuRef.current && !colMenuRef.current.contains(e.target as Node)) setColMenu(false)
    }
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [colMenu])

  const toggleCol = (c: ColKey) => {
    const next = { ...cols, [c]: !cols[c] }
    setCols(next)
    localStorage.setItem(COLS_LS_KEY, JSON.stringify(next))
  }

  const create = async () => {
    setBusy(true); setErr('')
    try {
      const body: Record<string, unknown> = {
        name: nName,
        ip_whitelist: nIP.trim(),
        model_limit: nModels.trim(),
        provider_limit: nProv.join(','),
      }
      // 额度上限（USD）：留空 = 不限
      if (nQuota.trim() !== '') body.quota_limit = Number(nQuota)
      if (nExpiresAt) body.expires_at = new Date(nExpiresAt).toISOString()
      const r = await api.post<{ api_key: string }>('/api/keys', body)
      setCreating(false)
      setCreated(r.api_key)
      setCreatedCopied(false)
      setNName(''); setNExpiresAt(''); setNIP(''); setNModels(''); setNQuota(''); setNProv([])
      load()
    } catch (e: any) { setErr(e.message) } finally { setBusy(false) }
  }

  const openEdit = (k: ApiKey) => {
    setEditing(k)
    setEName(k.name || '')
    setEEnabled(k.enabled)
    setEIP(k.ip_whitelist || '')
    setEModels(k.model_limit || '')
    setEQuota(k.quota_limit ? String(k.quota_limit) : '')
    setEProv(parseProviderLimit(k.provider_limit))
    setEResetQuota(false)
    if (k.expires_at) { setEExpiresAt(toLocalInput(k.expires_at)); setENoExpiry(false) }
    else { setEExpiresAt(''); setENoExpiry(true) }
  }

  const saveEdit = async () => {
    if (!editing) return
    if (!eNoExpiry && !eExpiresAt) { setErr(t('keys.pleasePickExpiry')); return }
    setBusy(true); setErr('')
    try {
      // 渠道列表已加载 → 清洗已删除渠道的悬空绑定；未加载 → 原样提交（后端校验归属）
      const providerLimit = (providersLoaded ? eProv.filter((id) => knownProviderIds.has(id)) : eProv).join(',')
      const body: Record<string, unknown> = {
        name: eName,
        enabled: eEnabled,
        expires_at: eNoExpiry ? '' : new Date(eExpiresAt).toISOString(),
        ip_whitelist: eIP.trim(),
        model_limit: eModels.trim(),
        provider_limit: providerLimit,
        // 额度上限：留空 = 不限（0）
        quota_limit: eQuota.trim() === '' ? 0 : Number(eQuota),
      }
      if (eResetQuota) body.reset_quota = true
      await api.patch(`/api/keys/${editing.id}`, body)
      setEditing(null)
      load()
    } catch (e: any) { setErr(e.message) } finally { setBusy(false) }
  }

  // 启用/禁用快捷切换（状态列点击）
  const toggleEnabled = async (k: ApiKey) => {
    setErr('')
    try {
      await api.patch(`/api/keys/${k.id}`, { enabled: !k.enabled })
      load()
    } catch (e: any) { setErr(e.message) }
  }

  // 查看明文并复制（AES-GCM 解密接口）
  const revealAndCopy = async (k: ApiKey) => {
    setErr('')
    try {
      const r = await api.get<{ api_key: string }>(`/api/keys/${k.id}/reveal`)
      await navigator.clipboard.writeText(r.api_key)
      setCopiedId(k.id)
      setTimeout(() => setCopiedId((cur) => (cur === k.id ? null : cur)), 1500)
    } catch (e: any) { setErr(e.message) }
  }

  const copyCreated = async (v: string) => {
    await navigator.clipboard.writeText(v)
    setCreatedCopied(true)
    setTimeout(() => setCreatedCopied(false), 1500)
  }

  const copyBase = async () => {
    await navigator.clipboard.writeText(API_BASE)
    setCopiedBase(true)
    setTimeout(() => setCopiedBase(false), 1500)
  }

  const copyExample = async () => {
    await navigator.clipboard.writeText(exExample(exProto, exTab))
    setCopiedEx(true)
    setTimeout(() => setCopiedEx(false), 1500)
  }

  const remove = async (k: ApiKey) => {
    if (!confirm(t('keys.deleteConfirm', { name: k.name || k.prefix }))) return
    setErr('')
    try {
      await api.del(`/api/keys/${k.id}`)
      load()
    } catch (e: any) { setErr(e.message) }
  }

  const th = (c: ColKey, label?: string) =>
    cols[c] ? <th className="px-4 py-3 whitespace-nowrap">{label ?? t(COL_LABELS[c])}</th> : null

  // 渠道绑定辅助：ID → 展示名；已加载渠道列表时识别"悬空绑定"（渠道已删除）
  const providerNames = useMemo(
    () => new Map(providers.map((p) => [p.id, p.name || `#${p.id}`])),
    [providers],
  )
  const knownProviderIds = useMemo(() => new Set(providers.map((p) => p.id)), [providers])
  const eProvOrphan = providersLoaded ? eProv.filter((id) => !knownProviderIds.has(id)) : []

  return (
    <div>
      <div className="mb-4">
        <h2 className="text-lg font-semibold">{t('keys.title')}</h2>
      </div>
      {err && <div className="mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400 err-well">{err}</div>}

      {/* 接入信息：三协议端点 + 接入方法 */}
      <div className="card mb-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="text-sm font-medium">{t('keys.apiEndpoint')} <span className="text-muted font-normal">· OpenAI / Anthropic / Responses</span></div>
          {/* 示例切换：协议 × 语言 */}
          <div className="flex flex-wrap items-center gap-2">
            <div className="flex items-center gap-1 rounded-lg p-0.5" style={{ background: 'color-mix(in oklab, var(--muted) 10%, transparent)' }}>
              {PROTO_TABS.map((t) => (
                <button
                  key={t.v}
                  onClick={() => setExProto(t.v)}
                  className={`rounded-md px-2 py-0.5 text-xs transition ${exProto === t.v ? 'tab-active font-medium' : 'text-muted hover:opacity-80'}`}
                >
                  {t.label}
                </button>
              ))}
            </div>
            <div className="flex items-center gap-1 rounded-lg p-0.5" style={{ background: 'color-mix(in oklab, var(--muted) 10%, transparent)' }}>
              {EX_TABS.map((t) => (
                <button
                  key={t.v}
                  onClick={() => setExTab(t.v)}
                  className={`rounded-md px-2 py-0.5 text-xs transition ${exTab === t.v ? 'tab-active font-medium' : 'text-muted hover:opacity-80'}`}
                >
                  {t.label}
                </button>
              ))}
            </div>
          </div>
        </div>

        {/* 端点信息 */}
        <div className="mt-3 space-y-2 text-xs">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="w-16 shrink-0 text-muted">Base URL</span>
            <code className="mono-well rounded-md px-2 py-1 font-mono">{API_BASE}</code>
            <button
              className="rounded-md p-1 text-muted transition hover:text-violet-600 dark:hover:text-violet-400"
              title={t('keys.copyBaseUrl')}
              onClick={copyBase}
            >
              {copiedBase ? <span className="text-xs text-emerald-600 dark:text-emerald-400">{t('common.copied')} ✓</span> : <Copy size={13} />}
            </button>
            <span className="text-soft">{t('keys.anthropicSdkTip')}</span>
          </div>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="w-16 shrink-0 text-muted">{t('keys.auth')}</span>
            <code className="mono-well rounded-md px-2 py-1 font-mono">Authorization: Bearer sk-…</code>
            <span className="text-soft">{t('keys.or')}</span>
            <code className="mono-well rounded-md px-2 py-1 font-mono">x-api-key: sk-…</code>
            <span className="text-soft">{t('keys.authBothTip')}</span>
          </div>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="w-16 shrink-0 text-muted">{t('keys.chat')}</span>
            <code className="mono-well rounded-md px-2 py-1 font-mono">
              {exProto === 'openai' ? 'POST /v1/chat/completions' : exProto === 'anthropic' ? 'POST /v1/messages' : 'POST /v1/responses'}
            </code>
            <span className="w-16 shrink-0 text-muted" style={{ marginLeft: '1rem' }}>{t('keys.modelsEndpoint')}</span>
            <code className="mono-well rounded-md px-2 py-1 font-mono">GET /v1/models</code>
          </div>
        </div>

        {/* 接入示例 */}
        <div className="relative mt-3">
          <pre className="mono-well max-h-64 overflow-auto rounded-lg p-3 font-mono text-xs leading-relaxed whitespace-pre">{exExample(exProto, exTab)}</pre>
          <button
            className="absolute right-2 top-2 rounded-md p-1 text-muted transition hover:text-violet-600 dark:hover:text-violet-400"
            title={t('keys.copyExample')}
            onClick={copyExample}
          >
            {copiedEx ? <span className="text-xs text-emerald-600 dark:text-emerald-400">{t('common.copied')} ✓</span> : <Copy size={13} />}
          </button>
        </div>
        <p className="mt-2 text-xs text-soft">
          {t('keys.introOpenai')}
          <code className="mono-well rounded px-1 font-mono">ANTHROPIC_BASE_URL={SITE_ORIGIN}</code>
          {t('keys.introAnthropic')}
        </p>
      </div>

      {/* Key 表格上方工具行：列设置（查看）+ 新建 */}
      <div className="mb-3 flex items-center justify-end gap-2">
        {/* 列设置（参考 new-api「查看」） */}
        <div className="relative" ref={colMenuRef}>
          <button className="btn-ghost" onClick={() => setColMenu((v) => !v)}>
            <Settings2 size={15} /> {t('keys.view')}
          </button>
          {colMenu && (
            <div className="modal-panel absolute right-0 z-20 mt-1 w-44 rounded-xl border p-1 shadow-lg" style={{ borderColor: 'var(--line)' }}>
              <div className="px-3 py-2 text-xs font-medium text-muted">{t('keys.showCols')}</div>
              {ALL_COLS.map((c) => (
                <label key={c} className="flex cursor-pointer items-center justify-between rounded-lg px-3 py-1.5 text-sm hover:bg-black/5 dark:hover:bg-white/5">
                  {t(COL_LABELS[c])}
                  <input type="checkbox" checked={cols[c]} onChange={() => toggleCol(c)} className="accent-violet-600" />
                </label>
              ))}
            </div>
          )}
        </div>
        <button className="btn-primary" onClick={() => setCreating(true)}>{t('keys.newKey')}</button>
      </div>

      <div className="card overflow-x-auto p-0">
        <table className="w-full text-sm">
          <thead>
            <tr className="thead-row text-left text-xs">
              {th('name')}
              {th('status')}
              {th('key')}
              {th('models')}
              {th('providers')}
              {th('ip')}
              {th('quota')}
              {th('created')}
              {th('last_used')}
              {th('expires')}
              <th className="sticky-col px-4 py-3"></th>
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => {
              return (
                <tr key={k.id} className="tbody-row">
                  {cols.name && (
                    <td className="px-4 py-3 whitespace-nowrap">{k.name || <span className="text-muted">{t('keys.unnamed')}</span>}</td>
                  )}
                  {cols.status && (
                    <td className="px-4 py-3">
                      <button title={t('keys.clickToggle')} onClick={() => toggleEnabled(k)}>
                        {k.enabled ? <span className="badge-green">active</span> : <span className="badge-red">disabled</span>}
                      </button>
                    </td>
                  )}
                  {cols.key && (
                    <td className="px-4 py-3">
                      <div className="flex items-center gap-1.5">
                        <span className="font-mono text-xs text-muted">{k.prefix}…</span>
                        <button
                          className="rounded-md p-1 text-muted transition hover:text-violet-600 dark:hover:text-violet-400"
                          title={t('keys.copyFullKey')}
                          onClick={() => revealAndCopy(k)}
                        >
                          {copiedId === k.id ? <span className="text-xs text-emerald-600 dark:text-emerald-400">{t('common.copied')} ✓</span> : <Copy size={13} />}
                        </button>
                      </div>
                    </td>
                  )}
                  {cols.models && (
                    <td className="max-w-40 px-4 py-3 text-xs text-muted">
                      {k.model_limit
                        ? <span className="line-clamp-2 break-all" title={k.model_limit}>{k.model_limit}</span>
                        : t('keys.unlimited')}
                    </td>
                  )}
                  {cols.providers && (
                    <td className="max-w-40 px-4 py-3 text-xs text-muted">
                      {k.provider_limit
                        ? (
                          <span
                            className="line-clamp-2 break-all"
                            title={parseProviderLimit(k.provider_limit).map((id) => providerNames.get(id) ?? `#${id}`).join(', ')}
                          >
                            {parseProviderLimit(k.provider_limit).map((id) => providerNames.get(id) ?? `#${id}`).join(', ')}
                          </span>
                        )
                        : t('keys.unlimited')}
                    </td>
                  )}
                  {cols.ip && (
                    <td className="max-w-36 px-4 py-3 font-mono text-xs text-muted">
                      {k.ip_whitelist
                        ? <span className="line-clamp-2 break-all" title={k.ip_whitelist}>{k.ip_whitelist}</span>
                        : t('keys.unlimited')}
                    </td>
                  )}
                  {cols.quota && <QuotaCell k={k} />}
                  {cols.created && <td className="px-4 py-3 text-xs text-muted whitespace-nowrap">{new Date(k.created_at).toLocaleString()}</td>}
                  {cols.last_used && <td className="px-4 py-3 text-xs text-muted whitespace-nowrap">{k.last_used_at ? new Date(k.last_used_at).toLocaleString() : <span className="text-soft">{t('keys.neverUsed')}</span>}</td>}
                  {cols.expires && (
                    <td className="px-4 py-3 text-xs text-muted whitespace-nowrap">
                      {k.expires_at ? new Date(k.expires_at).toLocaleString() : <span className="text-soft">{t('keys.noExpiry')}</span>}
                    </td>
                  )}
                  <td className="sticky-col px-4 py-3 text-right whitespace-nowrap">
                    <div className="flex justify-end gap-1">
                      <button className="rounded-md p-1.5 text-muted transition hover:text-violet-600 dark:hover:text-violet-400" title={t('common.edit')} onClick={() => openEdit(k)}>
                        <Pencil size={14} />
                      </button>
                      <button className="btn-danger" onClick={() => remove(k)}>{t('common.delete')}</button>
                    </div>
                  </td>
                </tr>
              )
            })}
            {keys.length === 0 && (
              <tr><td colSpan={11} className="px-4 py-10 text-center text-muted">{t('keys.empty')}</td></tr>
            )}
          </tbody>
        </table>
      </div>

      {/* 新建 */}
      {creating && (
        <Modal onClose={() => setCreating(false)} title={t('keys.createTitle')}>
          <div className="space-y-4">
            <div>
              <label className="label">{t('common.name')}</label>
              <input className="input" value={nName} onChange={(e) => setNName(e.target.value)} placeholder="my-cursor" />
            </div>
            <div>
              <label className="label">{t('keys.expiryOptional')}</label>
              <input className="input" type="datetime-local" value={nExpiresAt} onChange={(e) => setNExpiresAt(e.target.value)} />
            </div>
            <div>
              <label className="label">{t('keys.modelLimitOptional')}</label>
              <input className="input" value={nModels} onChange={(e) => setNModels(e.target.value)} placeholder={t('keys.modelLimitPlaceholder')} />
            </div>
            <div>
              <label className="label">{t('keys.providerLimitOptional')}</label>
              <ProviderPicker providers={providers} selected={nProv} onChange={setNProv} />
              <p className="mt-1 text-xs text-muted">{t('keys.providerLimitHint')}</p>
            </div>
            <div>
              <label className="label">{t('keys.ipWhitelistOptional')}</label>
              <input className="input" value={nIP} onChange={(e) => setNIP(e.target.value)} placeholder={t('keys.ipWhitelistPlaceholder')} />
            </div>
            <div>
              <label className="label">{t('keys.quotaLimitOptional')}</label>
              <input className="input font-mono" type="number" min="0" step="any" value={nQuota} onChange={(e) => setNQuota(e.target.value)} placeholder={t('keys.quotaPlaceholder')} />
            </div>
            <div className="flex justify-end gap-2">
              <button className="btn-ghost" onClick={() => setCreating(false)}>{t('common.cancel')}</button>
              <button className="btn-primary" disabled={busy} onClick={create}>{t('common.create')}</button>
            </div>
          </div>
        </Modal>
      )}

      {/* 编辑 */}
      {editing && (
        <Modal onClose={() => setEditing(null)} title={t('keys.editTitle', { prefix: editing.prefix })}>
          <div className="space-y-4">
            <div>
              <label className="label">{t('common.name')}</label>
              <input className="input" value={eName} onChange={(e) => setEName(e.target.value)} placeholder="my-cursor" />
            </div>
            <div className="flex items-center gap-6">
              <label className="flex cursor-pointer items-center gap-1.5 text-sm">
                <input type="checkbox" checked={eEnabled} onChange={(e) => setEEnabled(e.target.checked)} className="accent-violet-600" />
                {t('common.enable')}
              </label>
              <label className="flex cursor-pointer items-center gap-1.5 text-sm">
                <input type="checkbox" checked={eNoExpiry} onChange={(e) => setENoExpiry(e.target.checked)} className="accent-violet-600" />
                {t('keys.noExpiry')}
              </label>
            </div>
            {!eNoExpiry && (
              <div>
                <label className="label">{t('keys.expiryTime')}</label>
                <input className="input" type="datetime-local" value={eExpiresAt} onChange={(e) => setEExpiresAt(e.target.value)} />
              </div>
            )}
            <div>
              <label className="label">{t('keys.modelLimit')}</label>
              <input className="input" value={eModels} onChange={(e) => setEModels(e.target.value)} placeholder={t('keys.modelLimitPlaceholder')} />
            </div>
            <div>
              <label className="label">{t('keys.providerLimit')}</label>
              <ProviderPicker providers={providers} selected={eProv} onChange={setEProv} />
              <p className="mt-1 text-xs text-muted">{t('keys.providerLimitHint')}</p>
              {eProvOrphan.length > 0 && (
                <p className="mt-1 text-xs text-amber-600 dark:text-amber-400">
                  {t('keys.providerOrphanHint', { count: eProvOrphan.length })}
                </p>
              )}
            </div>
            <div>
              <label className="label">{t('keys.ipWhitelist')}</label>
              <input className="input" value={eIP} onChange={(e) => setEIP(e.target.value)} placeholder={t('keys.ipWhitelistPlaceholder')} />
            </div>
            <div>
              <label className="label">{t('keys.quotaLimit')}</label>
              <input className="input font-mono" type="number" min="0" step="any" value={eQuota} onChange={(e) => setEQuota(e.target.value)} placeholder={t('keys.quotaPlaceholderShort')} />
              <p className="mt-1 text-xs text-muted">{t('keys.quotaHint')}</p>
            </div>
            {(editing.quota_used ?? 0) > 0 && (
              <label className="flex cursor-pointer items-center gap-1.5 text-sm">
                <input type="checkbox" checked={eResetQuota} onChange={(e) => setEResetQuota(e.target.checked)} className="accent-violet-600" />
                {t('keys.resetQuota', { used: fmtMoney(editing.quota_used ?? 0) })}
              </label>
            )}
            <div className="flex justify-end gap-2">
              <button className="btn-ghost" onClick={() => setEditing(null)}>{t('common.cancel')}</button>
              <button className="btn-primary" disabled={busy} onClick={saveEdit}>{t('common.save')}</button>
            </div>
          </div>
        </Modal>
      )}

      {/* 创建成功 */}
      {created && (
        <Modal onClose={() => setCreated(null)} title={t('keys.createdTitle')}>
          <div className="space-y-4">
            <div className="break-all mono-well rounded-lg p-3 font-mono text-sm text-emerald-600 dark:text-emerald-400">
              {created}
            </div>
            <div className="flex justify-end">
              <button className="btn-primary" onClick={() => copyCreated(created)}>{createdCopied ? `${t('common.copied')} ✓` : t('common.copy')}</button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  )
}

// ProviderPicker 渠道绑定多选：勾选 = 该 Key 只路由到这些渠道（不勾 = 不限）。
// selected 中不在渠道列表里的 ID（渠道已删除，或列表尚未加载）单独渲染为「已删除」行，
// 用户可取消；保存时清除悬空绑定（见 saveEdit）。
function ProviderPicker({ providers, selected, onChange }: {
  providers: Provider[]
  selected: number[]
  onChange: (ids: number[]) => void
}) {
  const { t } = useTranslation()
  const known = new Set(providers.map((p) => p.id))
  const orphans = selected.filter((id) => !known.has(id))
  const toggle = (id: number) =>
    onChange(selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id])
  if (providers.length === 0 && orphans.length === 0) {
    return <p className="text-xs text-muted">{t('keys.noProviderForBinding')}</p>
  }
  const row = 'flex cursor-pointer items-center gap-2 rounded-md px-2 py-1 text-sm transition hover:bg-black/5 dark:hover:bg-white/5'
  return (
    <div className="max-h-44 space-y-0.5 overflow-auto rounded-lg border p-1.5" style={{ borderColor: 'var(--line)' }}>
      {providers.map((p) => (
        <label key={p.id} className={row}>
          <input type="checkbox" className="accent-violet-600" checked={selected.includes(p.id)} onChange={() => toggle(p.id)} />
          <span className={'min-w-0 truncate' + (p.enabled ? '' : ' text-muted')}>{p.name || `#${p.id}`}</span>
          <span className="ml-auto shrink-0 text-xs text-soft">{p.kind === 'oauth' ? 'OAuth' : 'API Key'}</span>
        </label>
      ))}
      {orphans.map((id) => (
        <label key={id} className={row}>
          <input type="checkbox" className="accent-violet-600" checked onChange={() => toggle(id)} />
          <span className="text-muted">#{id}</span>
          <span className="ml-auto shrink-0 text-xs text-soft">{t('keys.providerDeleted')}</span>
        </label>
      ))}
    </div>
  )
}

// QuotaCell 额度列（完整 <td>，不能返回裸 span/div —— tr 内非法子元素会被包成匿名单元格导致错位）：
// 已用/上限进度条（≥80% 橙、100% 红 + 超限标记；不限 = 显示累计已用）
function QuotaCell({ k }: { k: ApiKey }) {
  const { t } = useTranslation()
  const used = k.quota_used ?? 0
  const limit = k.quota_limit ?? 0
  if (limit <= 0) {
    return <td className="whitespace-nowrap px-4 py-3 text-xs text-muted" title={t('keys.noQuotaTitle')}>{t('keys.usedAmount', { amount: fmtMoney(used) })}</td>
  }
  const pct = Math.min(100, (used / limit) * 100)
  const over = used >= limit
  const warn = !over && pct >= 80
  const color = over ? '#dc2626' : warn ? '#d97706' : '#7c3aed'
  return (
    <td className="px-4 py-3" title={t('keys.quotaCellTitle', { used: `$${used.toFixed(4)}`, limit: `$${limit.toFixed(2)}` })}>
      <div className="flex items-center gap-2">
        <div className="track h-1.5 w-20 shrink-0 overflow-hidden rounded-full">
          <div className="h-full rounded-full transition-all" style={{ width: `${pct}%`, background: color }} />
        </div>
        <span className={'whitespace-nowrap text-xs tabular-nums ' + (over ? 'font-medium text-red-500' : warn ? 'text-amber-600 dark:text-amber-400' : 'text-muted')}>
          {fmtMoney(used)}/{fmtMoney(limit)}{over ? ` ${t('keys.overQuota')}` : ''}
        </span>
      </div>
    </td>
  )
}

// fmtMoney 费用展示：≥$100 保留 2 位，否则 4 位并去尾零
function fmtMoney(n: number) {
  const v = Math.abs(n) >= 100 ? n.toFixed(2) : n.toFixed(4)
  return '$' + v.replace(/0+$/, '').replace(/\.$/, '')
}
