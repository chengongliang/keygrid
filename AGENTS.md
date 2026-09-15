# AGENTS.md

本文件为 AI 编码代理提供 KeyGrid 仓库的工作指引。

## 项目概述

KeyGrid 是团队自用的 LLM 网关:每位员工自助配置**自己的**供应商渠道(API Key 或 OAuth 订阅),平台统一签发 `sk-` API Key,对外暴露 OpenAI 兼容接口。核心特性是**硬隔离**——用户 A 的请求只会路由到 A 自己的渠道,凭据/用量互不可见。

技术栈:Go 1.25(chi router + GORM)+ PostgreSQL + Redis + React 19 / Vite / TypeScript / Tailwind CSS v4 + lucide-react。前端构建产物 embed 进 Go 单二进制部署。

用户文档见 `README.md`。

## 常用命令

```bash
# 测试(Go 全量单测,提交前必须通过)
make test                    # 即 go test ./...

# 构建
make web                     # 前端构建(web/dist → Go embed)
make build                   # 前端 + Go → bin/keygrid 单二进制
make web-dev                 # 前端热更新 dev server(:5173)

# 本地开发(依赖 docker 里的 postgres/redis)
docker compose up -d postgres redis
make run                     # 加载 .env 后 go run .,API :8090
curl http://127.0.0.1:8090/healthz   # {"status":"ok"}

# Docker 部署
make docker-up               # 开发/本地(app+postgres+redis)
make docker-up-prod          # 生产(db/redis 不暴露端口)
make docker-logs             # 跟踪 app 日志

# E2E(依赖 docker compose 起的全套服务,内含 mock 上游)
SSRF_ALLOW_INTERNAL=1 E2E_MODE=1 bash scripts/run_e2e_oauth.sh   # OAuth 全链路
bash scripts/run_e2e_oidc.sh                                     # OIDC SSO 全链路
bash scripts/run_e2e_protocols.sh                                # 多协议矩阵(chat/messages/responses × openai/anthropic 渠道)
bash scripts/e2e_admin.sh                                        # 管理端闭环(注册/禁用/重置密码/注册策略/维护模式;需平台已运行且已有 admin)
```

- 前端类型检查:`cd web && npx tsc -b`(已包含在 `npm run build` 中)。
- 数据库迁移由启动时 `db.Migrate()` 自动执行(GORM AutoMigrate),无独立迁移工具。
- 首个管理员:`.env` 设 `ADMIN_EMAIL=邮箱` → 用该邮箱注册 → 重启一次自动提升为 admin。

## 目录结构

```
main.go                     # 入口:配置→加密初始化→DB 迁移→router→OAuth 调度→优雅退出
internal/
  conf/                     # 配置加载(env),conf.go
  crypto/                   # AES-GCM 封装(凭据加密,MASTER_KEY)
  db/                       # 连接与迁移
  httpx/                    # HTTP 客户端封装(超时/代理)
  ssrf/                     # SSRF 防护(私网拒绝、DNS rebinding 防护)
  model/                    # GORM 实体:user, provider, credential, apikey, usage_log...
  op/                       # 数据操作层(CRUD+缓存),op.go 是主入口;所有查询在此强制带 user_id
  relay/                    # ★ 转发引擎
    handler.go              #   三入口骨架(chat/messages/responses)、failover 主循环
    route.go                #   用户内渠道路由(model_map、priority、failover)
    state.go                #   熔断状态机(连续失败熔断 30s)
    protocol.go             #   openai 上游调用 + SSE 透传 + 通用 usage 提取
    codex.go                #   chat/completions ↔ responses 双向协议转换
    anthropic.go            #   anthropic messages ↔ pivot 双向协议转换
    responses_api.go        #   /v1/responses 入口解析 + 响应生成
    stream_bridge.go        #   流式协议桥(任意上游流 → 入口 SSE)
    ratelimit.go / usage.go #   每 key 限流 / SSE 流式记账
  oauth/                    # ★ OAuth 框架
    types.go                #   Adapter 接口 + TokenSet + FlowType
    registry.go             #   provider 注册表(providers/init.go 空导入触发注册)
    refresh.go              #   刷新调度:提前 5 分钟、Redis 去重锁、错误分类
    flow.go / presets.go    #   授权流程 / 渠道预设模板(GLM、DeepSeek、SiliconFlow 等)
    providers/              #   每个供应商一个文件:kimi.go, openai.go, anthropic.go...
  oidc/                     # OIDC SSO(JWKS 缓存、PKCE、JIT 建号)
  oidc.go 相关状态存储见 model/oidc_state.go
  server/                   # HTTP 层
    handlers/               #   用户端 + 管理端 handler(admin_*.go)
    middleware/             #   auth、cors、RequireRole 等
    resp/                   #   统一响应格式
    router/router.go        #   路由表
  task/                     # 后台任务(OAuth token 刷新扫描等)
web/                        # React SPA(构建产物 dist/ 被 Go embed;仓库内仅提交 dist/.gitkeep 占位,
                            #   保证 fresh clone 可直接 go build/go test)
  src/i18n/                 # react-i18next:index.ts 初始化 + LanguageToggle;locales/{zh-CN,en}/<域>.ts 各域字典
  src/pages/                #   每个页面一个文件:Providers / ApiKeys / Usage / Login / Settings / Admin*
  src/components/           #   ProviderLogo、usage-charts 等
  src/lib/api.ts            #   fetch 封装(统一鉴权/错误处理)
  public/logos/             #   供应商图标素材(来源与商标声明见 docs/provider-logos.md)
scripts/                    # 备份恢复、E2E 脚本、mock 服务(mock_kimi_oauth.py、mock_oidc.py)
deploy/Caddyfile            # 可选 Caddy 自动 HTTPS
docs/                       # 专项说明(供应商图标来源等)
.github/                    # CI(gofmt/vet/test + 前端构建)与 Issue/PR 模板
SECURITY.md                 # 漏洞上报政策(私密渠道 / In-Out of scope)
CONTRIBUTING.md             # 贡献指南(人类贡献者入口;架构细节引用本文件)
THIRD_PARTY_NOTICES.md      # 第三方代码/素材来源与许可声明(9router 等)
```

## 硬性规则(安全与隔离,不可违反)

1. **数据隔离**:所有对 `providers / credentials / api_keys / usage_logs` 的读写必须带 `WHERE user_id = 当前用户`。新查询一律封装进 `op/` 层,不要在 handler 里直接写 GORM 查询绕过隔离。relay 路由只能在 `user_id = token.user_id` 的渠道池里选。
2. **凭据加密**:供应商凭据 AES-GCM 加密落库(`crypto/`),任何代码路径不得明文落库、不得写入日志。admin 也只能看状态,不能读凭据明文。
3. **API Key 存储**:平台签发的 `sk-` key 存 sha256 哈希(鉴权)+ AES-GCM 加密的明文副本(支持"查看/复制"按需解密回显),落库不留明文;`MASTER_KEY` 属最高敏感配置——它同时解锁供应商凭据与全部已签发 key,不得泄露或写入日志。
4. **日志不落内容**:usage/审计日志只记元数据(模型、tokens、状态码、延迟),不落 prompt 与响应内容。
5. **SSRF 防护**:任何接受用户输入 URL 的功能(base_url、测连、模型拉取)必须经过 `ssrf/` 包校验。
6. **OAuth 安全**:state 一次性 + 5min 过期;refresh_token 单次使用型 provider 必须在 Redis 锁内原子完成「换新→落库」。
7. `.env` 含真实密钥,永远不提交;新增配置项时同步更新 `.env.example`。

## 代码约定

- **语言/注释**:代码注释与文档用中文,与现有代码风格保持一致。
- **Go**:遵循标准 Go 规范;错误用 `fmt.Errorf("...: %w", err)` 包装;HTTP 响应统一走 `server/resp`;新增接口先在 `server/router/router.go` 注册,再实现 handler。
- **前端**:TypeScript;API 调用统一走 `web/src/lib/api.ts`;UI 用 Tailwind 工具类 + 品牌紫主色,图标用 lucide-react;页面级组件放在 `src/pages/`。
- **前端 i18n**:用户可见文案(按钮/badge/placeholder/title/confirm/alert/错误提示)一律 `t()` 调用,不硬编码中文;初始化在 `web/src/i18n/index.ts`(defaultNS='translation',fallback zh-CN),字典按域拆在 `locales/{zh-CN,en}/`,新建域文件需在两语言的 `locales/<lang>/index.ts` 注册;页面专属文案放各自域,通用词优先复用 `common`;key 用英文 camelCase、层级 ≤2 层,同一文案复用同一 key;**zh 与 en 的 key 结构必须完全一致**(zh 为主语言,en 同步直译);非组件上下文(confirm/模块级常量)可用 `import i18n from '@/i18n'` 后 `i18n.t()`,或常量存语义 key、渲染处 `t()`;改完跑 `cd web && npx tsc --noEmit`。
- **提交信息**:Conventional Commits + 中文描述,如 `feat(proxy): 渠道级出口代理 + SSRF 白名单`、`fix(auth): ...`、`docs: ...`。分支命名 `feat/xxx` / `fix/xxx`。
- **测试**:Go 单测与被测文件同目录(`*_test.go`),涉及 relay 路由、OAuth 刷新、协议转换的改动需补测试;改完跑 `make test`。

## 常见改动怎么下手

- **新增 OAuth 供应商**:在 `internal/oauth/providers/` 新建文件实现 `oauth.Adapter` 接口(`Key/Flow/BeginAuth/Resolve/Refresh`),在 `init.go` 注册,参考 `kimi.go`(device_code)或 `openai.go`(pkce);补单测并跑 `scripts/run_e2e_oauth.sh`。
- **新增平台 API**:model 实体(如需)→ `op/` 方法 → `server/handlers/` handler → `router/router.go` 注册路由;管理端接口需挂 admin 中间件,并留审计日志(event 前缀 `admin.`)。
- **改动 relay 转发逻辑**:注意流式(SSE)与非流式两条路径都要覆盖,记账(`usage.go`)在流式下从最后 chunk 的 usage 或累计 delta 取值。
- **前端新增页面/功能**:改 `web/src/pages/`,文案同步写进 `web/src/i18n/locales/{zh-CN,en}/` 对应域字典(zh/en 两份都要写),跑 `make web-dev` 自测;涉及路由改 `App.tsx`。
- **新增渠道预设模板**:改 `internal/oauth/presets.go`(自动填 base_url + 模型目录)。

## 注意事项

- 修改前端后必须 `make web` 重新构建,否则 embed 的还是旧产物;`bin/` 下的二进制是构建产物,不要手工编辑。
- 本地 `go run` 需先 `docker compose up -d postgres redis`(compose 已把 db/redis 端口暴露给宿主机),并加载 `.env`(`make run` 已处理)。
- 登录有限速(5 次/分钟/IP),E2E 或本地调试遇到 429 先想到这个;渠道测连失败多是被 SSRF 拦截,本地联调内网地址需 `SSRF_ALLOW_INTERNAL=1`。
- 升级/改表前建议 `make backup`;回滚依赖 pg_dump 备份。
