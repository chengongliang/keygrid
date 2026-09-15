# Provider Logos

供应商平台 logo（PNG，正方形透明底），供前端 `ProviderLogo` 组件按平台 ID 渲染。

## 来源

复制自 [9router](https://github.com/decolua/9router)（MIT License）的 `public/providers/` 目录。
各 logo 图形与商标权利归其所属公司所有，仅作平台标识展示用途。

## 平台 ID 对应表

| 文件 | 平台 ID | 说明 |
| --- | --- | --- |
| anthropic.png | `anthropic` | Claude / Anthropic OAuth |
| codex.png | `openai` | OpenAI Codex OAuth（keygrid 内 oauth 平台 key 为 `openai`） |
| kimi.png | `kimi` | Kimi（Moonshot）OAuth |
| iflow.png | `iflow` | 心流 iFlow OAuth |
| qoder.png | `qoder` | Qoder OAuth |
| trae.png | `trae` | Trae OAuth |
| codebuddy-cn.png | `codebuddy-cn` | 腾讯 CodeBuddy OAuth |
| glm.png | `glm` | 智谱 GLM |
| deepseek.png | `deepseek` | DeepSeek |
| siliconflow.png | `siliconflow` | 硅基流动 |
| volcengine-ark.png | `volcengine-ark` | 火山方舟 |
| baidu.png | `qianfan` | 百度千帆（别名 → baidu.png） |
| tencent.png | `hunyuan` | 腾讯混元（别名 → tencent.png） |
| xiaomi-mimo.png | `xiaomi-mimo` | 小米 MiMo |
| minimax-cn.png | `minimax-cn` | MiniMax 中国 |
| openai.png | 协议 `openai` | 自定义渠道协议兜底图标 |
| gemini.png | 协议 `gemini` | 自定义渠道协议兜底图标 |

约定：URL 为 `/logos/{平台ID}.png`；别名（如 `qianfan` → `baidu`）在前端
`web/src/lib/providerIcons.ts` 的 `ICON_ALIASES` 中维护；加载失败的 ID 会进入
会话级缓存，避免重复请求（9router 同款策略）。
