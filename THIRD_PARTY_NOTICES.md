# Third-Party Notices（第三方开源声明）

KeyGrid 的部分代码与素材移植/改编自以下开源项目。按照其许可证要求，保留原版权与许可声明如下（许可证全文保持英文原文）。

## 9router

- 仓库：https://github.com/decolua/9router
- 使用范围：
  - `internal/oauth/providers/`：kimi / openai / anthropic / iflow / qoder / trae / codebuddy 的 OAuth 适配器移植（各文件头有 port 来源标注）
  - `internal/oauth/presets.go`：供应商预设与模型目录数据
  - `web/public/logos/`：供应商图标素材
- 许可证全文：

```text
MIT License

Copyright (c) 2024-2026 decolua and contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## CLIProxyAPI

- 仓库：https://github.com/router-for-me/CLIProxyAPI
- 使用范围：`internal/oauth/modelcatalog.go` 的模型目录拉取策略对齐其实践
- 许可证全文：

```text
MIT License

Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## 商标声明

`web/public/logos/` 中的供应商图标（Kimi / DeepSeek / OpenAI / Anthropic / GLM 等）图形与商标权利归其所属公司所有，仅用于平台标识展示。相关权利人如提出要求，可移除对应素材。

## 其他参考项目

以下项目为设计与实现提供参考（未直接复制代码），一并致谢：
[octopus](https://github.com/bestruirui/octopus)
[OpenAI Codex CLI](https://github.com/openai/codex) 等，完整列表见 README 致谢章节。
