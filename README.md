<div align="center">

<img src="web/public/logo.png" alt="KeyGrid" width="320" />

# KeyGrid

A self-hosted LLM gateway for teams: every member configures **their own** provider accounts (API keys or OAuth subscriptions), and the platform issues unified `sk-` API keys, exposing an OpenAI-compatible interface.

Hard isolation — your requests are only ever routed to your own providers. Credentials and usage are never visible across users.

<a href="https://github.com/chengongliang/keygrid/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/chengongliang/keygrid/actions/workflows/ci.yml/badge.svg"></a>&nbsp;<a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-blue.svg"></a>&nbsp;<img alt="Go" src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white">&nbsp;<img alt="React" src="https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=white">&nbsp;<img alt="Docker" src="https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white">

[✨ Features](#-features) · [🚀 Quick Start](#-quick-start-admin) · [👤 Employee Guide](#-employee-onboarding-3-steps) · [📦 Deployment](#-deployment) · [🔍 FAQ](#-faq) · [📄 License](#-license)

**English** | [简体中文](README.zh-CN.md)

</div>

> Tech stack: Go (chi + GORM) + PostgreSQL + Redis + React/Vite/Tailwind (single binary with embedded frontend).

## ✨ Features

- 🔑 **API-key providers**: any OpenAI-compatible base_url (DeepSeek, Kimi openapi, third-party relay stations…), just paste your key
- 🌐 **OAuth subscription providers**: Kimi (device_code), OpenAI/Codex (pkce), Anthropic/Claude, iFlow, Qoder, Trae, CodeBuddy — authorize inside the platform with automatic token refresh (5 minutes ahead of expiry)
- 📋 **Provider presets**: GLM, DeepSeek, SiliconFlow, Volcengine Ark, Qianfan, Hunyuan, Xiaomi MiMo, MiniMax and more out-of-the-box templates (auto-fills base_url + model catalog); when adding a provider you can fetch the upstream model list and tick models directly
- 🛡 **Egress proxy**: configured once by admins in System Settings (http/https/socks5); users simply tick "use platform proxy" per provider — OpenAI and other proxy-required sites come pre-ticked; effective across relay, OAuth authorize/refresh, connection tests, and model fetching
- 🔀 **Smart routing**: model_map mapping, priority, multi-provider failover, circuit breaker after consecutive failures (30s)
- 🔌 **Multi-protocol endpoints**: exposes OpenAI `/v1/chat/completions`, Anthropic `/v1/messages`, and OpenAI Responses `/v1/responses` simultaneously — Claude Code / OpenAI SDK / Responses SDK all plug right in; protocols are converted both ways in-gateway and routed to any of your providers (see "Multi-protocol endpoints" below)
- 📊 **Usage analytics**: aggregated by day/model/key, hourly heatmap, time-range filters, SSE streaming accounting, `/v1/models` aggregates your own available models
- 👥 **Admin console**: user management (disable / password reset / role change), platform-wide usage dashboard (CSV export), provider health overview, announcements / maintenance mode, registration policy (open / invite-only / closed)
- 🔐 **OIDC SSO**: single sign-on with Keycloak / Okta / Authentik or any standard OIDC IdP (JIT account provisioning), hot-configured in the admin console — effective on save
- 🔒 **Security**: provider credentials encrypted at rest (AES-GCM); platform API keys store a sha256 hash for auth plus an AES-GCM-encrypted copy behind the copy/reveal button (never plaintext at rest — losing `MASTER_KEY` exposes both provider credentials and issued keys). Optional IP allowlist / model restrictions, SSRF private-network blocking (incl. DNS rebinding protection), login rate limiting (5/min/IP), audit logs, CORS allowlist

## 🚀 Quick Start (Admin)

```bash
cp .env.example .env
# Edit .env: MASTER_KEY (required, 32+ random chars), JWT_SECRET, PUBLIC_BASE_URL
# Every secret must be changed: the app refuses to start while MASTER_KEY/JWT_SECRET
# still hold their example values (`openssl rand -hex 32` generates one)

# Production start (app + postgres + redis; mock services won't start)
docker compose up -d
curl http://127.0.0.1:8090/healthz   # {"status":"ok"}

# Local development (frontend HMR :5173, API :8090)
docker compose up -d postgres redis
make web-dev   # in another terminal: go run .
```

> 👑 **Become an admin**: set `ADMIN_EMAIL=your@email` in `.env` → register with that email → restart once (auto-promoted to admin at startup). You can also adjust other users' roles in the admin console afterwards.

Open `http://127.0.0.1:8090` → register → add providers → grab your key.

## 👤 Employee Onboarding (3 Steps)

### Step 1: Sign in and configure providers

Open the gateway URL (e.g. `http://gw.company.com`) in your browser and register / sign in (if your company has OIDC SSO configured, just click "SSO Login"), then go to **My Providers**:

- **API-key providers**: Add provider → pick a preset (GLM/DeepSeek/SiliconFlow… auto-fills base_url and models) or custom base_url → paste your API key → save → run "Test connection" until it's green; you can also fetch the upstream model list and tick models
- **OAuth providers** (Kimi/Codex/Claude/iFlow/Qoder/Trae/CodeBuddy subscriptions): Add provider → "Authorize" → complete authorization via redirect or user_code → status turns active (tokens are auto-renewed by the platform, zero maintenance)
  - **OpenAI Codex**: requires a ChatGPT Plus/Pro subscription. After login the browser redirects to `localhost:1455` (the page won't load — that's expected). Copy the full URL from the address bar and paste it back into the platform to finish authorization. The upstream speaks the Responses protocol; the gateway converts chat/completions ↔ responses both ways, so just use `/v1` as usual.
- **Proxy-required sites** (e.g. OpenAI): tick "Access via platform proxy" when adding/editing the provider (the OpenAI preset comes pre-ticked); the proxy address is configured centrally by admins in System Settings — invisible and read-only for users. The option is hidden when no proxy is configured.

### Step 2: Issue your platform API key

Go to the **API Keys** page → New → the full key is shown at creation — copy it right away (the list only shows the prefix `sk-xxxx…` by default; use the copy/reveal button anytime to decrypt and view it again).

### Step 3: Connect your tools

Point all tools at the gateway's `/v1`, swapping in the base_url and your key:

**OpenAI SDK (Python)**
```python
from openai import OpenAI
client = OpenAI(
    base_url="https://gw.company.com/v1",
    api_key="sk-xxxx",              # platform-issued key
)
resp = client.chat.completions.create(
    model="gpt-4o",                # a model name configured in your provider
    messages=[{"role": "user", "content": "hello"}],
)
print(resp.choices[0].message.content)
```

**Environment variables (Cursor / Claude Code / any SDK)**
```bash
export OPENAI_BASE_URL=https://gw.company.com/v1
export OPENAI_API_KEY=sk-xxxx

# Cursor: Settings → Models → set OpenAI API Key to sk-xxxx, Base URL to the /v1 above
# Claude Code: point ANTHROPIC_BASE_URL / OPENAI_BASE_URL at the gateway
```

**curl**
```bash
curl https://gw.company.com/v1/chat/completions \
  -H "Authorization: Bearer sk-xxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# List the models available to you
curl https://gw.company.com/v1/models -H "Authorization: Bearer sk-xxxx"
```

> 💡 `/v1/models` returns the model set from **your own** providers (from model_map config); providers without model_map pass through any model name.

### Multi-protocol endpoints

The gateway exposes three client protocols at once; any entry point can route to any of your providers (protocols are converted both ways in-gateway, with OpenAI chat/completions as the internal pivot):

| Endpoint | Clients | Notes |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI SDK / any OpenAI-compatible tool | Main entry; native SSE streaming passthrough |
| `POST /v1/messages` | Anthropic SDK (Claude Code etc.) | `x-api-key: sk-xxxx` or Bearer both work; streaming events, tool_use, thinking all supported |
| `POST /v1/responses` | OpenAI Responses SDK | Stateless gateway; `previous_response_id` / `store` not supported |

Anthropic SDK example:
```python
from anthropic import Anthropic
client = Anthropic(
    base_url="https://gw.company.com",
    api_key="sk-xxxx",              # platform-issued key
)
msg = client.messages.create(
    model="gpt-4o",                # routes to any of your providers, not just Claude
    max_tokens=1024,
    messages=[{"role": "user", "content": "hello"}],
)
print(msg.content[0].text)
```

> 💡 A provider's protocol field decides how the upstream is called: openai providers relay `/v1/chat/completions`,
> anthropic providers relay `/v1/messages`, and Codex subscription providers relay the Responses API — all three entry points can use any protocol provider.

## 📦 Deployment

### Development / Local

```bash
# Single binary (frontend embedded, no static files needed)
make build   # outputs bin/keygrid

# Or docker compose (db/redis ports exposed to the host, shared with local go run)
make docker-up
```

### Production deployment

```bash
cp .env.example .env && vim .env
# Must change: MASTER_KEY (32+ random chars), JWT_SECRET, POSTGRES_PASSWORD,
#       PUBLIC_BASE_URL (e.g. https://gw.company.com)

# Start (db/redis not exposed to the host; SSRF internal-network allowance is off by default —
#       set SSRF_ALLOW_INTERNAL=1 in .env if you need to reach internal services)
make docker-up-prod    # i.e. docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build

# Optional: built-in Caddy auto-HTTPS reverse proxy (DOMAIN must resolve to this host on 80/443)
# Set DOMAIN=gw.company.com in .env, then:
# docker compose -f docker-compose.yml -f docker-compose.prod.yml --profile tls up -d --build

curl http://127.0.0.1:8090/healthz   # {"status":"ok"}; the container HEALTHCHECK hits the same path
```

### Backup / Restore / Upgrade

```bash
make backup                        # pg_dump → backups/keygrid_<timestamp>.sql.gz (keeps the last 14)
make restore FILE=backups/keygrid_20250909_120000.sql.gz   # ⚠ overwrites the current DB

# Upgrade: pull new code and rebuild; the pgdata volume persists
make docker-up-prod
# Migrations run automatically via db.Migrate at startup; run make backup before rolling back
```

### Common operations

```bash
make docker-logs     # follow app logs (auto rotation)
make docker-down     # stop (volumes kept)
docker compose -f docker-compose.yml -f docker-compose.prod.yml down   # stop in production mode
```

Key environment variables: see [.env.example](.env.example) — `MASTER_KEY` (credential encryption), `JWT_SECRET`, `PUBLIC_BASE_URL` (OAuth callback origin), `POSTGRES_PASSWORD`, `CORS_ALLOWED_ORIGINS` (only needed when calling the API cross-origin; empty = cross-origin denied), `TRUST_PROXY` (set 1 behind a reverse proxy so rate limits / IP allowlists see the real client IP; keep 0 when the port is directly exposed), `ADMIN_EMAIL` (first admin), `RATE_LIMIT_RPM` / `RATE_LIMIT_CONCURRENCY` (per-key rate/concurrency limits, 0 = unlimited), `QUOTA_ENFORCE` (key quota hard-limit switch, on by default). OIDC SSO is best configured hot under Admin → System Settings; the `OIDC_*` env vars are fallback only.

## 💰 Billing & Quota

- **Model pricing** (Admin → Model Pricing): maintain "model → price per 1M tokens (USD)", with input/output priced separately.
  Matching: exact model name > `*` fallback row > unpriced (allowed through for free, no quota charged); costs are recorded at request-time price snapshots — price changes never retroact.
- **Key quota** (API Keys page): each key can have its own USD quota cap; once exceeded, relay returns **402**. Usage accumulates in real time (Redis counters + batched DB reconciliation) and can be reset anytime. Quota attaches to keys only; at the user level it's just an accumulated-spend display.
- **Billing name mapping** (provider editor): map private aliases (e.g. `my-alias`) to canonical price-table names for billing; the mapping target must exist in the price table (name normalization only — it can't change prices).
- Usage pages / platform usage both include cost dimensions (range cost + cumulative totals).

## 🧪 Testing

```bash
make test                          # full Go unit tests
SSRF_ALLOW_INTERNAL=1 E2E_MODE=1 bash scripts/run_e2e_oauth.sh
                                   # E2E: register → OAuth provider → authorize → relay → accounting, full flow (compose includes a mock Kimi)
bash scripts/run_e2e_oidc.sh       # E2E: mock OIDC IdP → SSO login → JIT provisioning, full flow
```

## 🔍 FAQ

| Symptom | Cause / Fix |
|---|---|
| 429 too many attempts | Login rate limit (5/min/IP) — wait and retry; a 429 from relay means the key rate limit kicked in (`RATE_LIMIT_RPM`) |
| Provider connection test fails | Private-network address blocked by SSRF: set `SSRF_ALLOW_INTERNAL=1` in `.env` (or fine-grained `SSRF_ALLOWED_HOSTS` / `SSRF_ALLOWED_CIDRS`) and restart |
| relay 503 | No available provider for that model (check enabled, model_map), or the platform is in maintenance mode |
| relay 402 | Key quota exhausted: reset usage or raise the cap under API Keys |
| Cost is 0 | Model unpriced (Admin → Model Pricing, or set up billing name mapping in the provider editor) |
| OAuth status revoked | refresh_token invalid (password changed / expired) — re-authorize |

## 🤝 Contributing

Issues and Pull Requests are welcome:

1. Fork this repo and create a feature branch (`feat/xxx` / `fix/xxx`)
2. Make sure `make test` passes before submitting; for frontend changes, test locally with `make web-dev`
3. Open a PR with a brief note on the motivation and how you tested

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full guide. For security issues, report privately via [SECURITY.md](SECURITY.md) — please do not open public issues for vulnerabilities.

## 📄 License

This project is open source under the [MIT](LICENSE) license.

## 🙏 Acknowledgements

Standing on the shoulders of giants:

**[9router](https://github.com/decolua/9router)**, **[octopus](https://github.com/bestruirui/octopus)**, **[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)**, **[OpenAI Codex CLI](https://github.com/openai/codex)**, **[chi](https://github.com/go-chi/chi)**, **[GORM](https://gorm.io)**, **[React](https://react.dev)**, **[Vite](https://vite.dev)** — excellent open-source projects this one borrows from and depends on.

Third-party code and asset attributions are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Thanks to these authors — without their work, KeyGrid wouldn't exist!
