# 贡献指南

感谢你对 KeyGrid 感兴趣！欢迎提交 Issue 与 Pull Request。

> English contributions are welcome as well — feel free to open issues or PRs in English.

---

## 开始之前：两条红线

KeyGrid 的核心价值是**硬隔离**，任何改动都不要破坏它：

1. **数据隔离**——所有 `providers / credentials / api_keys / usage_logs` 的读写必须带 `WHERE user_id = 当前用户`，查询一律封装进 `internal/op/` 层，不要在 handler 里裸写 GORM 绕过隔离。
2. **凭据与内容不进日志**——供应商凭据、API Key 明文、prompt/响应内容不得落库明文、不得写入日志或错误响应。

完整的硬性规则（共 7 条）与架构说明见 [AGENTS.md](AGENTS.md)，改动前建议先过一遍。

安全漏洞请勿走公开 Issue，按 [SECURITY.md](SECURITY.md) 私密上报。

## 本地开发环境

依赖：Go 1.25+、Node 22+、Docker（Postgres / Redis 直接跑在 compose 里）。

```bash
# 1. 起依赖组件
docker compose up -d postgres redis

# 2. 配置（必须改掉示例密钥，否则应用拒绝启动）
cp .env.example .env
openssl rand -hex 32   # 生成后填入 MASTER_KEY / JWT_SECRET

# 3. 后端（:8090，加载 .env）
make run

# 4. 前端热更新（:5173，代理到 :8090）
make web-dev
```

常用命令：`make test`（Go 全量单测，提交前必须通过）、`make build`（单二进制）、`cd web && npx tsc --noEmit`（前端类型检查）。

E2E（需要完整 compose 与 mock 上游，详见 README）：`bash scripts/run_e2e_oauth.sh`、`bash scripts/run_e2e_oidc.sh` 等。

## 代码规范

**Go**

- `gofmt` 必须无输出（CI 会检查）；`go vet ./...` 干净
- 错误用 `fmt.Errorf("...: %w", err)` 包装；HTTP 响应统一走 `internal/server/resp`
- 新增接口先在 `internal/server/router/router.go` 注册，再实现 handler；管理端接口挂 admin 中间件并留审计日志（事件前缀 `admin.`）
- 注释与文档使用中文，与现有代码风格保持一致

**前端**

- API 调用统一走 `web/src/lib/api.ts`；图标用 lucide-react；样式用 Tailwind 工具类 + 品牌紫主色
- 用户可见文案一律 `t()`，**zh-CN 与 en 两份字典必须同步**（key 结构完全一致）；新建 i18n 域文件需在两语言的 `locales/<lang>/index.ts` 注册

## 提交规范

- 提交信息：[Conventional Commits](https://www.conventionalcommits.org/) + **中文描述**
  - 例：`feat(proxy): 渠道级出口代理 + SSRF 白名单`、`fix(auth): 修复 OIDC 回调 state 校验`
- 分支命名：`feat/xxx`、`fix/xxx`

## 测试要求

- 修 bug / 加功能都需要补对应单测，测试文件与被测文件同目录（`*_test.go` / `*.test.tsx`）
- 涉及 **relay 路由、OAuth 刷新、协议转换** 的改动必须补测试并说明覆盖点
- 测试不得依赖真实外部服务或真实凭据（用 `httptest` + 环境变量覆盖端点，参考 `internal/oauth/providers/*_test.go`）
- 提交前跑 `make test`；改了前端再跑 `cd web && npx tsc --noEmit`

## Pull Request 流程

1. Fork 本仓库，从 `main` 拉出特性分支
2. 提交改动（建议小而聚焦，一个 PR 只做一件事）
3. 按 PR 模板填写：变更说明、动机、自测方式；涉及安全边界的改动请显式说明影响
4. 等 CI 通过（`gofmt` / `go vet` / `go test` / 前端构建）与维护者 review

## 常见改动怎么下手

新增 OAuth 供应商、新增渠道预设、新增管理端接口、改造 relay 转发逻辑的具体落点，见 [AGENTS.md](AGENTS.md) 的「常见改动怎么下手」一节（`internal/oauth/providers/`、`internal/oauth/presets.go`、`router.go`、`relay/` 等）。

## 许可证

贡献的代码将以本项目的 [MIT 许可证](LICENSE) 发布。引入第三方代码/素材时请确认许可证兼容，并按需更新 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
