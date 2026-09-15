import type { Provider } from '@/lib/api'

// providerIcons.ts —— 供应商平台图标解析（9router 同款策略）。
// 资源：/logos/{platform}.png（来源与对应表见 docs/provider-logos.md）。
// 会话级 404 缓存：一次加载失败后整个会话不再请求该 ID，直接走首字母兜底。

// 平台别名：keygrid 平台 key → 资源文件 ID（不带扩展名）
const ICON_ALIASES: Record<string, string> = {
  qianfan: 'baidu', // 百度千帆
  hunyuan: 'tencent', // 腾讯混元
}

// api_key 渠道按 base_url host 匹配预设平台（与后端 presets.go 的 base_url 对应；
// host 用后缀匹配，兼容 /v1、/api/xx 等路径差异与中国站/国际站域名）
const HOST_HINTS: [suffix: string, platform: string][] = [
  ['open.bigmodel.cn', 'glm'],
  ['api.deepseek.com', 'deepseek'],
  ['siliconflow.', 'siliconflow'],
  ['.volces.com', 'volcengine-ark'],
  ['qianfan.baidubce.com', 'qianfan'],
  ['hunyuan.cloud.tencent.com', 'hunyuan'],
  ['xiaomimimo.com', 'xiaomi-mimo'],
  ['minimaxi.com', 'minimax-cn'],
  ['minimax.chat', 'minimax-cn'],
  ['api.kimi.com', 'kimi'],
  ['chatgpt.com', 'openai'],
  ['api.anthropic.com', 'anthropic'],
]

// 会话级失败缓存（运行时 only，刷新页面即重置）
const failedIds = new Set<string>()

const normalize = (id: string | null | undefined): string =>
  (id ?? '').trim().toLowerCase()

/** 解析图标资源 ID（过别名 + 跳过已失败的）；空串表示走兜底。 */
function resolveIconId(platform: string | null | undefined): string {
  const id = normalize(platform)
  if (!id || failedIds.has(id)) return ''
  const aliased = ICON_ALIASES[id] ?? id
  if (failedIds.has(aliased)) return ''
  return aliased
}

/** `/logos/{id}.png`；空串表示无图标（已失败或平台未知）。 */
export function providerIconSrc(platform: string | null | undefined): string {
  const id = resolveIconId(platform)
  return id ? `/logos/${id}.png` : ''
}

/** img onError 回调：把失败 ID 记入会话缓存（含别名双方）。 */
export function markProviderIconMissing(platform: string | null | undefined): void {
  const id = normalize(platform)
  if (!id) return
  failedIds.add(id)
  const aliased = ICON_ALIASES[id]
  if (aliased) failedIds.add(aliased)
}

/**
 * 推断渠道所属平台 ID（用于选 logo）：
 * 1. OAuth 渠道 → oauth_provider（即 adapter key）
 * 2. api_key 渠道 → base_url host 匹配预设平台
 * 3. 兜底 → 协议名（openai | anthropic | gemini）
 */
export function resolveProviderPlatform(p: Pick<Provider, 'kind' | 'oauth_provider' | 'base_url' | 'protocol'>): string {
  if (p.kind === 'oauth') return normalize(p.oauth_provider) || normalize(p.protocol)
  const host = hostOf(p.base_url)
  if (host) {
    for (const [suffix, platform] of HOST_HINTS) {
      if (host.endsWith(suffix)) return platform
    }
  }
  return normalize(p.protocol)
}

function hostOf(url: string | null | undefined): string {
  if (!url) return ''
  try {
    return new URL(url).host.toLowerCase()
  } catch {
    return ''
  }
}
