#!/usr/bin/env bash
# Runner for scripts/e2e_protocols.sh — 起 mock-upstream（compose mock profile）
# 并等待就绪，然后跑多协议转发矩阵 E2E。
# 用法: bash scripts/run_e2e_protocols.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# ---- 确保 mock-upstream 在跑（compose mock profile；已起则跳过）----
if ! docker compose ps --status running mock-upstream 2>/dev/null | grep -q mock-upstream; then
  echo ">> starting mock-upstream (compose --profile mock)..."
  docker compose --profile mock up -d mock-upstream
  sleep 2
fi

# 网关容器经 compose 网络访问 mock-upstream
export MOCK_UPSTREAM="http://mock-upstream:9999"
export BASE="${BASE:-http://127.0.0.1:8090}"

# 健康检查（网关 + mock 上游宿主机端口）
curl -sf $BASE/healthz >/dev/null || { echo "❌ 网关未运行（make run 或 docker compose up -d app）"; exit 1; }
curl -sf "http://127.0.0.1:${MOCK_UPSTREAM_HOST_PORT:-9999}/v1/models" \
  -H "Authorization: Bearer upstream-secret-key" >/dev/null || {
  echo "❌ mock-upstream 未就绪"; docker compose logs mock-upstream | tail -5; exit 1; }

exec bash "$(dirname "$0")/e2e_protocols.sh"
