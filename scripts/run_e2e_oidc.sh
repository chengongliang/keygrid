#!/usr/bin/env bash
# Runner for scripts/e2e_oidc.sh — 本地起 mock IdP + 带 OIDC env 重启网关容器。
# 用法: bash scripts/run_e2e_oidc.sh   （跑完自动恢复无 OIDC 的原状态）
set -euo pipefail
cd "$(dirname "$0")/.."

CONTAINER=$(docker compose ps -q app)
if [ -z "$CONTAINER" ]; then
  echo "❌ app 容器未运行，先 docker compose up -d app"; exit 1
fi

# ---- 清理旧 mock IdP ----
docker rm -f mock-oidc-e2e 2>/dev/null || true

# ---- 起 mock IdP（host 网络：容器内 issuer 指向 host.docker.internal）----
# 网关在容器里 dial issuer，issuer 必须是容器可达地址；宿主机跑脚本用同一地址
ISSUER="http://host.docker.internal:9902"
docker run -d --name mock-oidc-e2e --rm \
  -e MOCK_OIDC_PORT=9902 \
  -e MOCK_OIDC_ISSUER="$ISSUER" \
  -v "$(pwd)/scripts:/scripts:ro" \
  -w /scripts --network host \
  python:3.12-alpine python3 -c "
import sys
try:
    import cryptography, jwt
except ImportError:
    import subprocess; subprocess.run([sys.executable,'-m','pip','install','-q','cryptography','pyjwt'])
exec(open('/scripts/mock_oidc.py').read())
" >/dev/null

cleanup() {
  docker rm -f mock-oidc-e2e 2>/dev/null || true
  docker rm -f mock-oidc-e2e-deps 2>/dev/null || true
}
trap cleanup EXIT

# pip 装依赖（alpine 镜像默认没有 cryptography/pyjwt）
echo "== 等待 mock IdP 依赖安装 & 就绪 =="
for i in $(seq 1 60); do
  if curl -sf http://127.0.0.1:9902/.well-known/openid-configuration >/dev/null 2>&1; then break; fi
  sleep 1
  [ "$i" = 60 ] && { echo "❌ mock IdP 未就绪"; docker logs mock-oidc-e2e | tail -20; exit 1; }
done
echo "   mock IdP ready on $ISSUER"

# ---- 带 OIDC env 重建 app（临时 .env 注入）----
cat > .env.oidc-e2e <<EOF
OIDC_ENABLED=1
OIDC_ISSUER=$ISSUER
OIDC_CLIENT_ID=keygrid
OIDC_CLIENT_SECRET=mock-secret
OIDC_SCOPES=openid email profile
SSRF_ALLOW_INTERNAL=1
E2E_MODE=1
EOF

echo "== 重建 app 容器（带 OIDC env + e2e extra_hosts）=="
docker compose -f docker-compose.yml -f docker-compose.e2e.yml --env-file .env.oidc-e2e up -d --no-deps --build app >/dev/null 2>&1 || \
docker compose -f docker-compose.yml -f docker-compose.e2e.yml --env-file .env.oidc-e2e up -d --no-deps app
# --env-file 覆盖 MASTER_KEY 等会失败（必需变量），退化到 shell env
if ! docker compose ps app | grep -q running; then
  OIDC_ENABLED=1 OIDC_ISSUER=$ISSUER OIDC_CLIENT_ID=keygrid OIDC_CLIENT_SECRET=mock-secret \
    docker compose -f docker-compose.yml -f docker-compose.e2e.yml up -d --no-deps app
fi

for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:8090/healthz >/dev/null 2>&1 && break
  sleep 1
  [ "$i" = 30 ] && { echo "❌ app 未就绪"; docker logs $(docker compose ps -q app) | tail -30; exit 1; }
done
echo "   app ready with OIDC enabled"

restore() {
  echo "== 恢复：重建无 OIDC 的 app =="
  rm -f .env.oidc-e2e
  docker compose up -d --no-deps --force-recreate app >/dev/null 2>&1 || true
}
trap 'cleanup; restore' EXIT

BASE=http://127.0.0.1:8090 IDP=http://127.0.0.1:9902 bash scripts/e2e_oidc.sh
