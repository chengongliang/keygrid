import { useState } from 'react'
import { markProviderIconMissing, providerIconSrc } from '@/lib/providerIcons'

// ProviderLogo —— 供应商平台 logo（9router 同款交互）：
// 有图渲染 <img loading=lazy decoding=async>；无图/加载失败渲染首字母圆角块，
// 失败 ID 进会话缓存，后续挂载直接走兜底不再发请求。
// 容器白底：logo 多为透明底彩图，暗色主题下也能正常辨识。
export default function ProviderLogo({
  platform,
  name,
  size = 36,
  className = '',
}: {
  platform: string | null | undefined
  name: string
  size?: number
  className?: string
}) {
  const src = providerIconSrc(platform)
  const [errored, setErrored] = useState(false)
  const showImg = src && !errored

  return (
    <span
      className={`inline-flex shrink-0 select-none items-center justify-center overflow-hidden rounded-lg bg-white ring-1 ring-black/10 dark:ring-white/15 ${className}`.trim()}
      style={{ width: size, height: size }}
      title={name}
    >
      {showImg ? (
        <img
          src={src}
          alt={name}
          width={size}
          height={size}
          className="object-contain"
          style={{ width: size - 6, height: size - 6 }}
          loading="lazy"
          decoding="async"
          onError={() => { markProviderIconMissing(platform); setErrored(true) }}
        />
      ) : (
        <span
          className="font-semibold text-neutral-500"
          style={{ fontSize: Math.max(10, Math.floor(size * 0.4)) }}
        >
          {(name || '?').trim().charAt(0).toUpperCase()}
        </span>
      )}
    </span>
  )
}
