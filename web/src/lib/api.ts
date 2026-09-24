const BASE = ''

export class ApiError extends Error {
  status: number
  code: number
  constructor(status: number, code: number, message: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  const token = localStorage.getItem('token')
  if (token) headers['Authorization'] = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  const res = await fetch(BASE + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new ApiError(res.status, data.code ?? res.status, data.message ?? data.error?.message ?? res.statusText)
  }
  // 统一响应 {code:0, data}
  return (data.data !== undefined ? data.data : data) as T
}

export const api = {
  get: <T>(p: string) => request<T>('GET', p),
  post: <T>(p: string, b?: unknown) => request<T>('POST', p, b),
  patch: <T>(p: string, b: unknown) => request<T>('PATCH', p, b),
  put: <T>(p: string, b: unknown) => request<T>('PUT', p, b),
  del: <T>(p: string) => request<T>('DELETE', p),
}

// ---- types ----
export interface User { id: number; email: string; name?: string; role: string; /** false = 纯 SSO 账号（无本地密码，禁用改密入口） */ has_password?: boolean }

export interface Provider {
  id: number
  name: string
  kind: 'api_key' | 'oauth'
  protocol: string
  base_url: string
  oauth_provider?: string
  model_map?: Record<string, string>
  /** 计费名映射（P1.5）：{请求名 → 计费标准名}；目标必须在价格表内 */
  billing_map?: Record<string, string>
  priority: number
  enabled: boolean
  /** 上游请求是否走平台代理（代理地址由管理员在系统设置统一配置） */
  use_proxy?: boolean
  /** 是否参与熔断检测（默认 false = 不熔断；只有一条不稳定渠道时建议保持关闭） */
  breaker_check?: boolean
  /** 上游 UA 策略：''（默认透传客户端）/ custom / forward */
  ua_mode?: string
  /** ua_mode=custom 时的 UA 值 */
  user_agent?: string
  /** 授权标记：api_key 恒 true；oauth = 已完成授权（有 active 凭据）。未授权渠道显示"待授权"。 */
  authorized?: boolean
  created_at: string
}

export interface ApiKey {
  id: number
  name?: string
  prefix: string
  ip_whitelist?: string // 逗号分隔 IP/CIDR，空 = 不限
  model_limit?: string // 逗号分隔模型名，空 = 不限
  /** 逗号分隔渠道 ID 白名单，空 = 不限；非空时请求只路由到白名单内渠道 */
  provider_limit?: string
  /** 额度上限 USD；0 = 不限（计费 P2 起硬拦截） */
  quota_limit?: number
  /** 累计已消耗 USD（系统异步增量维护） */
  quota_used?: number
  expires_at?: string | null
  enabled: boolean
  last_used_at?: string | null
  created_at: string
}

export interface UsageAggRow {
  day: string
  api_key_id: number
  model: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  avg_latency_ms: number
  /** 费用 USD（请求时价格快照累计；未定价模型为 0） */
  cost: number
}

export interface UsageHourRow {
  dow: number // 0=周日 … 6=周六
  hour: number // 0-23
  requests: number
  prompt_tokens: number
  completion_tokens: number
}

/** 按 Key 聚合的用量行（/api/usage/by-key；key_name/prefix 为空 = Key 已删除） */
export interface UsageByKeyRow {
  api_key_id: number
  key_name: string
  key_prefix: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  avg_latency_ms: number
  cost: number
  last_used_at?: string | null
}

/** 按模型聚合的用量行（/api/usage/by-model） */
export interface UsageByModelRow {
  model: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  avg_latency_ms: number
  cost: number
}

/** 按渠道聚合的用量行（/api/usage/by-provider；name 为空 = 渠道已删除） */
export interface UsageByProviderRow {
  provider_id: number
  name: string
  kind: string // api_key | oauth
  protocol: string
  oauth_provider?: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  avg_latency_ms: number
  cost: number
  last_used_at?: string | null
}

/** 用户费用汇总（/api/usage/summary；累计维度，与区间筛选无关） */
export interface UsageCostSummary {
  total_cost: number
  cost_30d: number
  total_tokens: number
  requests: number
}

export interface AuditLog {
  id: number
  user_id: number
  event: string
  detail: string
  ip: string
  user_agent: string
  created_at: string
}

export interface BeginResult {
  user_code?: string
  verification_uri?: string
  verification_uri_complete?: string
  interval?: number
  device_code_expires_in?: number
  authorize_url?: string
  // pkce 专用：本次授权的 redirect_uri。以 http://localhost 开头时平台收不到回调
  // （OpenAI Codex 固定 localhost:1455），前端展示「粘贴回调 URL」输入框。
  redirect_uri?: string
}

// ---- 额度快照（openai codex /wham/usage 同步结果）----
/** 单个限速窗口 */
export interface QuotaWindow {
  used_percent: number
  window_seconds: number
  /** 5h / daily / weekly / monthly / yearly；空 = 窗口大小未知 */
  window_label?: string
  reset_after_seconds?: number
  /** 重置时刻 unix 秒；0 = 未知 */
  reset_at?: number
}

export interface QuotaRateLimit {
  allowed: boolean
  limit_reached: boolean
  /** 长窗口（新版语义为周限额） */
  primary?: QuotaWindow
  /** 短窗口（通常 5h） */
  secondary?: QuotaWindow
}

export interface QuotaData {
  plan_type?: string
  rate_limit?: QuotaRateLimit
  credits?: { has_credits: boolean; unlimited: boolean; balance: string }
  /** 限速重置积分剩余次数（消耗一次可立即清零当前窗口，即「重置次数」） */
  reset_credits_available?: number
  additional?: { limit_name: string; rate_limit?: QuotaRateLimit }[]
}

export interface QuotaSnapshot {
  id: number
  provider_id: number
  user_id: number
  platform: string
  /** ok=最近一次同步成功；error=失败（data 保留上次成功数据） */
  status: 'ok' | 'error'
  error?: string
  data?: QuotaData
  /** probe=主动查询 / relay=转发响应头被动观察 */
  source: string
  fetched_at: string
  updated_at: string
}

export interface TestResult {
  ok: boolean
  status: number
  latency_ms: number
  error?: string
  credential_status: string
  channel_name: string
}

// ---- 上游模型探测 ----
export interface FetchModelsResult {
  ok: boolean
  status: number
  models?: string[]
  error?: string
  channel_name: string
}

export interface TestModelResult {
  ok: boolean
  status: number
  latency_ms: number
  first_token_ms?: number
  model: string
  stream?: boolean
  prompt?: string
  content?: string
  reasoning_content?: string
  finish_reason?: string
  prompt_tokens?: number
  completion_tokens?: number
  error?: string
  channel_name: string
}

// ---- provider 预设模板（/api/providers/meta）----
export interface ProviderPreset {
  key: string
  kind: 'api_key' | 'oauth'
  protocol: string
  base_url: string
  api_key_url?: string
  signup_url?: string
  models: string[]
  default_priority: number
  auth_mode?: string
  description?: string
  /** 该站点通常需代理才能访问（如 OpenAI）：选预设时默认勾选「通过平台代理访问」 */
  needs_proxy?: boolean
}

export interface ProvidersMeta {
  protocols: string[]
  oauth_keys: string[]
  oauth_flows: Record<string, string>
  presets: ProviderPreset[]
  /** 平台出口代理（密码脱敏后）；空 = 未配置代理，向导不展示代理勾选框 */
  proxy_url?: string
}


// ---- M4 admin ----
export interface AdminUserRow {
  id: number
  email: string
  name: string
  role: string
  status: string
  api_key_count: number
  provider_count: number
  total_requests: number
  total_tokens: number
  last_activity_at?: string | null
  created_at: string
}

export interface PlatformUsageAggRow {
  day: string
  user_id: number
  email: string
  model: string
  requests: number
  failed_requests: number
  prompt_tokens: number
  completion_tokens: number
  avg_latency_ms: number
  cost: number
}

export interface TopUserRow {
  user_id: number
  email: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  avg_latency_ms: number
  cost: number
}

export interface ProviderHealthRow {
  id: number
  user_id: number
  user_email: string
  name: string
  kind: string
  protocol: string
  enabled: boolean
  breaker_check: boolean
  cred_status: string
  cred_expires_at?: string | null
  last_error: string
  req_24h: number
  fail_24h: number
  success_rate: number
  avg_latency_ms: number
  breaker_state: string
}

/** 失败请求详情（渠道健康视图的排查数据；保留 7 天，单渠道每分钟最多 30 条采样） */
export interface RequestErrorRow {
  id: number
  provider_id: number
  provider_name: string
  api_key_name: string
  model: string
  /** 失败分类：no_channel / circuit_open / credential / transform / proxy /
   *  upstream_4xx / upstream_429 / upstream_5xx / transport / stream_aborted */
  kind: string
  status_code: number
  /** 上游错误摘要（已截断；不含 prompt 与响应正文） */
  message: string
  latency_ms: number
  created_at: string
}

// ---- OIDC SSO 系统设置（参考 new-api：管理员动态配置，保存即热生效）----
export interface OidcSettings {
  enabled: boolean
  display_name: string
  issuer: string
  client_id: string
  client_secret: string
  scopes: string
  callback_url: string
}

export interface PlatformSettings {
  announcement: string
  maintenance_mode: boolean
  registration_policy: string
  /** 平台出口代理（http/https/socks5），空 = 未配置 */
  proxy_url?: string
  oidc?: OidcSettings
}

export interface Invitation {
  id: number
  code: string
  created_by: number
  used_by?: number | null
  used_at?: string | null
  expires_at: string
  created_at: string
}

// ---- 计费 P1：模型价格表 ----

/** admin 全局模型价格表条目（单价：USD / 1M tokens） */
export interface ModelPrice {
  id: number
  /** 精确模型名；"*" 为兜底价行 */
  model: string
  prompt_price: number
  completion_price: number
  remark: string
  updated_at: string
  created_at: string
}

export interface ModelPriceUpsert {
  model: string
  prompt_price: number
  completion_price: number
  remark?: string
}

/** 定价一键同步（OpenRouter）预览条目 */
export interface PriceSyncPreviewItem {
  model: string
  /** OpenRouter 原始 id（如 openai/gpt-4o），溯源展示用 */
  source_id: string
  prompt_price: number
  completion_price: number
  status: 'new' | 'update' | 'same'
  current_prompt_price?: number
  current_completion_price?: number
}
