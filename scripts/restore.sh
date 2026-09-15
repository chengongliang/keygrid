#!/usr/bin/env bash
# 从 backup.sh 产物恢复数据库（危险操作：会清空并覆盖当前数据！）
# 用法: bash scripts/restore.sh backups/keygrid_YYYYMMDD_HHMMSS.sql.gz
set -euo pipefail
cd "$(dirname "$0")/.."

FILE="${1:?用法: bash scripts/restore.sh backups/keygrid_YYYYMMDD_HHMMSS.sql.gz}"
[ -f "$FILE" ] || { echo "文件不存在: $FILE" >&2; exit 1; }

echo "⚠  将用 $FILE 覆盖当前数据库，5 秒内 Ctrl+C 可取消..."
sleep 5

# 清空 schema 后重放备份
docker compose exec -T postgres psql -U keygrid -d keygrid \
  -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'

gunzip -c "$FILE" | docker compose exec -T postgres psql -U keygrid -d keygrid -q

echo "✔ restore 完成"
