# Security Policy

KeyGrid handles provider credentials, issued API keys and request routing, so security reports are taken seriously. Thank you for helping keep the project and its users safe.

## Supported Versions

Only the latest `main` (and the most recent tagged release, once released) receives security fixes. There is no long-term support branch for older versions.

## Reporting a Vulnerability

**Please do not open a public issue for security problems.**

Report privately via GitHub Security Advisories:

➡️ https://github.com/chengongliang/keygrid/security/advisories/new

If you cannot use GitHub advisories, open a minimal public issue asking for a private contact channel — without disclosing any details of the vulnerability.

Please include:

- Affected version, commit or deployment mode (single binary / docker compose / harbor)
- A clear reproduction (steps, request samples, or a proof-of-concept)
- Impact assessment — what an attacker gains, which trust boundary is crossed
- Any suggested fix or mitigation you have in mind

## Response

- Acknowledgement within **72 hours**
- Triage and initial assessment within **7 days**
- Fix or documented mitigation for confirmed issues as soon as practical, coordinated with your disclosure timeline
- Credit in the release notes / advisory unless you prefer to remain anonymous

## Scope

This project's core security promises (see `AGENTS.md`) define what matters most:

**In scope**

- **Isolation bypass** — user A reaching user B's providers, credentials, API keys or usage data
- **Credential exposure** — plaintext credentials or API keys leaking into logs, error responses, API responses, or the database
- **Auth bypass** — forging sessions/JWTs, privilege escalation to admin, working around IP allowlists, model limits or key quotas
- **SSRF** — reaching private networks through `base_url`, connection tests, model fetching or the egress proxy
- **OAuth flow flaws** — replayable state (must be single-use, 5 min TTL), refresh-token races for single-use providers, token leakage via callback handling
- **Web console issues** — XSS, CSRF, or sensitive data exposure in the admin/user UI

**Out of scope**

- Attacks that require control of the server, the deployment environment, or physical access
- Denial of service through resource exhaustion on a self-hosted instance
- Vulnerabilities in upstream provider APIs (report them to that provider)
- Using OAuth subscription providers outside their official clients — this is a **documented, known gray area**; ToS enforcement by upstreams is not a vulnerability in KeyGrid
- Issues that only reproduce with `SSRF_ALLOW_INTERNAL=1`, `E2E_MODE=1`, `CORS_ALLOWED_ORIGINS=*` or other explicitly opt-in, documented-as-insecure development settings
- Misconfiguration of third-party components (Caddy, PostgreSQL, Redis)

## Safe Harbor

We will not pursue legal action or report you to authorities for good-faith research that stays within this policy: use your own instance or the documented dev setup, avoid privacy violations and service disruption, and give us a reasonable window to fix an issue before public disclosure.

## Hardening Checklist for Operators

KeyGrid refuses to boot with example secrets, but a secure deployment is still your responsibility:

- Generate unique `MASTER_KEY` / `JWT_SECRET` (`openssl rand -hex 32`)
- Keep `SSRF_ALLOW_INTERNAL=0` unless you genuinely need internal upstreams
- Set `CORS_ALLOWED_ORIGINS` only if the API is called cross-origin
- Keep `TRUST_PROXY=0` unless the app port is unreachable except through a trusted reverse proxy
- Terminate TLS at Caddy / your gateway, and back up Postgres regularly (`make backup`)
