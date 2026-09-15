#!/usr/bin/env bash
# Runner for scripts/e2e_oauth.sh — mock kimi runs as compose service `mock-kimi`.
# The gateway container dials `http://mock-kimi:9901` inside the compose network,
# so the provider base_url (upstream) and approve endpoint use that name too.
export MOCK_OAUTH_UPSTREAM="http://mock-kimi:9901"
# approve is called from THIS host via the published port
APPROVE_URL="${APPROVE_URL:-http://127.0.0.1:9901}"
export BASE="${BASE:-http://127.0.0.1:8090}"
exec bash "$(dirname "$0")/e2e_oauth.sh"
