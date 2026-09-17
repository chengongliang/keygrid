<div align="center">

<img src="web/public/logo.png" alt="KeyGrid" width="320" />

# KeyGrid

团队自用的 LLM 网关：每位员工自助配置**自己的**供应商渠道（API Key 或 OAuth 订阅），平台统一签发 `sk-` API Key，对外暴露 OpenAI 兼容接口。

硬隔离 —— 你的请求只会路由到你自己的渠道，凭据/用量互不可见。

<a href="https://github.com/chengongliang/keygrid/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/chengongliang/keygrid/actions/workflows/ci.yml/badge.svg"></a>&nbsp;<a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-blue.svg"></a>&nbsp;<img alt="Go" src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white">&nbsp;<img alt="React" src="https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=white">&nbsp;<img alt="Docker" src="https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white">

[✨ 功能](#-功能) · [🚀 快速开始](#-快速开始管理员) · [👤 接入指南](#-员工接入指南3-步) · [📦 部署](#-部署) · [🔍 常见问题](#-常见问题) · [📄 许可证](#-许可证)

[English](README.md) | **简体中文**

</div>

> 技术栈：Go (chi+GORM) + PostgreSQL + Redis + React/Vite/Tailwind（单二进制 embed 部署）。

## ✨ 功能

- 🔑 **API Key 类渠道**：任意 OpenAI 兼容 base_url（DeepSeek、Kimi openapi、中转站…），粘贴 key 即用
- 🌐 **OAuth 订阅类渠道**：Kimi（device_code）、OpenAI/Codex（pkce）、Anthropic/Claude、iFlow、Qoder、Trae、CodeBuddy，平台内完成授权 + token 自动刷新（提前 5 分钟）
- 📋 **渠道预设**：GLM、DeepSeek、SiliconFlow、火山方舟、千帆、混元、小米 MiMo、MiniMax 等开箱模板（自动填 base_url + 模型目录），添加渠道时可拉取上游模型列表直接勾选
- 🛡 **出口代理**：管理员在系统设置统一配置（http/https/socks5），用户仅在渠道上勾选是否启用 —— OpenAI 等需代理站点默认勾选；转发、OAuth 授权/刷新、测连、模型拉取全路径生效
- 🔀 **智能路由**：model_map 映射、priority 优先级、多渠道 failover、连续失败熔断 30s
- 🔌 **多协议入口**：同时暴露 OpenAI `/v1/chat/completions`、Anthropic `/v1/messages`、OpenAI Responses `/v1/responses` 三套客户端协议 —— Claude Code / OpenAI SDK / Responses SDK 任意接入，协议在网关内双向转换路由到你的任意渠道（详见下方「多协议入口」）
- 📊 **用量统计**：按日/模型/Key 聚合、分时热力图、时间范围筛选，SSE 流式记账，/v1/models 聚合本人可用模型
- 👥 **管理后台**：用户管理（禁用/重置密码/角色调整）、全平台用量看板（CSV 导出）、渠道健康总览、公告/维护模式、注册策略（开放/邀请码/关闭）
- 🔐 **OIDC SSO**：Keycloak/Okta/Authentik 等标准 OIDC IdP 单点登录（JIT 自动建号），管理端热配置、保存即生效
- 🔒 **安全**：供应商凭据 AES-GCM 加密落库；平台 API Key 存 sha256 哈希（鉴权）+ AES-GCM 密文副本（“查看/复制”按需解密回显，落库不留明文）——`MASTER_KEY` 同时解锁供应商凭据与全部已签发 Key，务必妥善保管。可配 IP 白名单/可用模型限制、SSRF 私网拒绝（含 DNS rebinding 防护）、登录限速（5 次/分钟/IP）、审计日志、CORS 白名单

## 🚀 快速开始（管理员）

```bash
cp .env.example .env
# 编辑 .env：MASTER_KEY（必填，32+ 随机串）、JWT_SECRET、PUBLIC_BASE_URL
# 每个密钥都必须改：仍保留示例值时应用拒绝启动（`openssl rand -hex 32` 生成）

# 生产启动（app + postgres + redis；mock 服务不会启动）
docker compose up -d
curl http://127.0.0.1:8090/healthz   # {"status":"ok"}

# 本地开发（前端热更新 :5173，API :8090）
docker compose up -d postgres redis
make web-dev   # 另一个终端: go run .
```

> 👑 **成为管理员**：在 `.env` 里设 `ADMIN_EMAIL=你的邮箱` → 用该邮箱注册账号 → 重启一次（启动时自动提升为 admin）。之后也可在管理后台调整其他用户角色。

打开 `http://127.0.0.1:8090` → 注册账号 → 配渠道 → 领 Key。

## 👤 员工接入指南（3 步）

### 第 1 步：登录并配置渠道

浏览器打开网关地址（如 `http://gw.company.com`），注册/登录（若公司配置了 OIDC 单点登录，直接点「SSO 登录」）后进入 **我的供应商**：

- **API Key 类**：添加渠道 → 选预设模板（GLM/DeepSeek/硅基流动…自动填 base_url 和模型）或自定义 base_url → 粘贴你的 API Key → 保存 → 点「测连」确认绿灯；也可一键拉取上游模型列表勾选
- **OAuth 类**（Kimi/Codex/Claude/iFlow/Qoder/Trae/CodeBuddy 订阅）：添加渠道 → 点「授权」→ 跳转/输入 user_code 完成授权 → 状态变 active（token 由平台自动续期，无需再管）
  - **OpenAI Codex**：需 ChatGPT Plus/Pro 订阅。登录后浏览器会跳转到 `localhost:1455`（页面打不开属预期）——复制地址栏完整 URL 回平台粘贴即可完成授权。上游为 Responses 协议，网关已做 chat/completions ↔ responses 双向转换，照常走 `/v1`。选预设会自动填好上游地址（`https://chatgpt.com/backend-api/codex/responses`，必须指向完整 Responses endpoint）；如需自建/镜像端点，可在编辑渠道时改「上游地址」。
- **需代理的站点**（如 OpenAI）：添加/编辑渠道时勾选「通过平台代理访问」即可（OpenAI 预设已默认勾选）；代理地址由管理员在系统设置统一配置，用户不可见不可改。未配置代理时该选项不展示。

### 第 2 步：签发平台 API Key

进入 **API Keys** 页 → 新建 → 创建时即展示完整 Key，请立即复制（列表默认只显示前缀 `sk-xxxx…`；随时可用“查看/复制”按钮解密回显）。

### 第 3 步：接入你的工具

所有工具统一指向网关的 `/v1`，替换 base_url 和 key 即可：

**OpenAI SDK（Python）**
```python
from openai import OpenAI
client = OpenAI(
    base_url="https://gw.company.com/v1",
    api_key="sk-xxxx",              # 平台签发的 Key
)
resp = client.chat.completions.create(
    model="gpt-4o",                # 你渠道里配置的模型名
    messages=[{"role": "user", "content": "hello"}],
)
print(resp.choices[0].message.content)
```

**环境变量方式（Cursor / Claude Code / 任意 SDK 通用）**
```bash
export OPENAI_BASE_URL=https://gw.company.com/v1
export OPENAI_API_KEY=sk-xxxx

# Cursor: Settings → Models → OpenAI API Key 填 sk-xxxx，Base URL 填上面的 /v1
# Claude Code: ANTHROPIC_BASE_URL / OPENAI_BASE_URL 指向网关即可
```

**curl**
```bash
curl https://gw.company.com/v1/chat/completions \
  -H "Authorization: Bearer sk-xxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# 查看你可用的模型列表
curl https://gw.company.com/v1/models -H "Authorization: Bearer sk-xxxx"
```

> 💡 `/v1/models` 返回的是**你自己**渠道的模型集合（来自 model_map 配置）；未配 model_map 的渠道可透传任意模型名。

### 多协议入口

网关同时暴露三套客户端协议，任意入口都能路由到你的任意渠道（协议在网关内双向转换，以 OpenAI chat/completions 为内部枢纽）：

| 入口 | 客户端 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI SDK / 任意 OpenAI 兼容工具 | 主入口，流式 SSE 原生透传 |
| `POST /v1/messages` | Anthropic SDK（Claude Code 等） | `x-api-key: sk-xxxx` 或 Bearer 均可；流式事件、tool_use、thinking 均支持 |
| `POST /v1/responses` | OpenAI Responses SDK | 无状态网关，不支持 `previous_response_id` / `store` |

Anthropic SDK 示例：
```python
from anthropic import Anthropic
client = Anthropic(
    base_url="https://gw.company.com",
    api_key="sk-xxxx",              # 平台签发的 Key
)
msg = client.messages.create(
    model="gpt-4o",                # 路由到你的任意渠道，不限于 Claude
    max_tokens=1024,
    messages=[{"role": "user", "content": "hello"}],
)
print(msg.content[0].text)
```

> 💡 渠道协议（渠道的「协议」字段）决定上游怎么调：openai 渠道转发 `/v1/chat/completions`，
> anthropic 渠道转发 `/v1/messages`，Codex 订阅渠道转发 Responses API——三个入口都能用任意协议渠道。

## 📦 部署

### 开发 / 本地

```bash
# 单二进制（前端已 embed，无需静态文件）
make build   # 产物 bin/keygrid

# 或 docker compose（db/redis 端口暴露宿主机，供本地 go run 共用）
make docker-up
```

### 生产部署

```bash
cp .env.example .env && vim .env
# 必改：MASTER_KEY（32+ 随机串）、JWT_SECRET、POSTGRES_PASSWORD、
#       PUBLIC_BASE_URL（如 https://gw.company.com）

# 启动（db/redis 不暴露宿主机端口；SSRF 内网放行默认关闭，需接入内网服务在 .env 开 SSRF_ALLOW_INTERNAL=1）
make docker-up-prod    # 即 docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build

# 可选：自带 Caddy 自动 HTTPS 反代（需 DOMAIN 已解析到本机 80/443）
# .env 里设 DOMAIN=gw.company.com 后：
# docker compose -f docker-compose.yml -f docker-compose.prod.yml --profile tls up -d --build

curl http://127.0.0.1:8090/healthz   # {"status":"ok"}，容器内置 HEALTHCHECK 走同一路径
```

### 备份 / 恢复 / 升级

```bash
make backup                        # pg_dump → backups/keygrid_时间戳.sql.gz（保留最近 14 份）
make restore FILE=backups/keygrid_20250909_120000.sql.gz   # ⚠ 覆盖当前库

# 升级到新版本：拉代码后重建，pgdata 数据卷不丢
make docker-up-prod
# 迁移由启动时 db.Migrate 自动执行；回滚请先 make backup
```

### 常用运维

```bash
make docker-logs     # 跟踪 app 日志（日志自动轮转）
make docker-down     # 停止（数据卷保留）
docker compose -f docker-compose.yml -f docker-compose.prod.yml down   # 生产模式下停止
```

关键环境变量见 [.env.example](.env.example)：`MASTER_KEY`（凭据加密）、`JWT_SECRET`、`PUBLIC_BASE_URL`（OAuth 回调域名）、`POSTGRES_PASSWORD`、`CORS_ALLOWED_ORIGINS`（仅跨域调用 API 时需配；留空 = 拒绝跨域）、`TRUST_PROXY`（反向代理后设 1，限速/IP 白名单按真实客户端 IP 判断；端口直接暴露时保持 0）、`ADMIN_EMAIL`（首个管理员）、`RATE_LIMIT_RPM` / `RATE_LIMIT_CONCURRENCY`（每 key 限流/并发，0 不限）、`QUOTA_ENFORCE`（key 额度硬限额开关，默认开）。OIDC SSO 推荐在 管理后台 → 系统设置 中热配置，`OIDC_*` 环境变量仅作兜底。

## 💰 计费与额度

- **模型定价**（管理员 → 模型定价）：维护「模型 → 每 1M tokens 单价（USD）」，输入/输出分开定价。
  计费匹配：精确模型名 > `*` 兜底行 > 未定价（免费放行、不计额度）；费用按请求时价格快照落库，调价不追溯历史。
- **Key 额度**（端点与密钥页）：每个 Key 可设独立额度上限（USD），超限后转发返回 **402**；额度实时累计
  （Redis 计数 + DB 批量对账），支持随时重置已用量。额度只挂 Key，用户层面仅展示累计消耗。
- **计费名映射**（渠道编辑）：渠道私有别名（如 `my-alias`）映射到价格表标准名后按标准名计费；
  映射目标必须在价格表内（只做名称归一化，改不了价格）。
- 用量页 / 平台用量均有费用维度（区间费用 + 累计汇总）。

## 🧪 测试

```bash
make test                          # Go 全量单测
SSRF_ALLOW_INTERNAL=1 E2E_MODE=1 bash scripts/run_e2e_oauth.sh
                                   # E2E: 注册→OAuth 渠道→授权→relay→记账 全链路（compose 内含 mock kimi）
bash scripts/run_e2e_oidc.sh       # E2E: mock OIDC IdP → SSO 登录 → JIT 建号 全链路
```

## 🔍 常见问题

| 现象 | 原因/处理 |
|---|---|
| 429 too many attempts | 登录限速（5 次/分钟/IP），稍等再试；relay 报 429 则触发了 Key 限流（`RATE_LIMIT_RPM`） |
| 渠道测连失败 | 私网地址被 SSRF 拦截：`.env` 设 `SSRF_ALLOW_INTERNAL=1`（或 `SSRF_ALLOWED_HOSTS`/`SSRF_ALLOWED_CIDRS` 细粒度放行）后重启 |
| relay 503 | 该模型无可用渠道（检查 enabled、model_map），或平台处于维护模式 |
| relay 402 | Key 额度已用尽：在 端点与密钥 重置已用额度或调大上限 |
| 费用为 0 | 模型未定价（管理员 → 模型定价 补价，或在渠道编辑里做计费名映射） |
| OAuth 状态 revoked | refresh_token 失效（改密/过期），重新授权即可 |

## 🤝 贡献

欢迎提交 Issue 和 Pull Request：

1. Fork 本仓库并创建特性分支（`feat/xxx` / `fix/xxx`）
2. 提交前确保 `make test` 全量单测通过；涉及前端改动请本地 `make web-dev` 自测
3. 提交 PR 并简要说明改动动机与测试方式

完整贡献指南见 [CONTRIBUTING.md](CONTRIBUTING.md)；安全漏洞请按 [SECURITY.md](SECURITY.md) 私密上报，勿开公开 Issue。

## 📄 许可证

本项目基于 [MIT](LICENSE) 许可证开源。

## 🙏 致谢

站在巨人的肩膀上：

**[9router](https://github.com/decolua/9router)**、**[octopus](https://github.com/bestruirui/octopus)**、**[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)**、**[OpenAI Codex CLI](https://github.com/openai/codex)**、**[chi](https://github.com/go-chi/chi)**、**[GORM](https://gorm.io)**、**[React](https://react.dev)**、**[Vite](https://vite.dev)** — 本项目借鉴与依赖的优秀开源项目。

感谢这些作者 —— 没有他们的工作，KeyGrid 不会存在！

第三方代码与素材的归属与许可声明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
