import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

/**
 * 轻量 Markdown 渲染（测活输出等 LLM 文本）。
 * react-markdown 默认不渲染原始 HTML，LLM 输出中的注入内容是安全的；
 * remark-gfm 提供表格 / 删除线 / 任务列表等 LLM 常用语法。
 * 排版样式见 index.css 的 .md-body（紧凑 + 浅深色自适应）。
 */
export function Markdown({ children }: { children: string }) {
  return (
    <div className="md-body">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{children}</ReactMarkdown>
    </div>
  )
}
