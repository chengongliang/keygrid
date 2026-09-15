#!/usr/bin/env bash
# PostgreSQL 逻辑备份 → backups/keygrid_时间戳.sql.gz
# 用法: bash scripts/backup.sh [输出目录]   （默认 ./backups）
# crontab 每日凌晨 3 点备份示例:
#   0 3 * * * cd /opt/keygrid && bash scripts/backup.sh >> backups/cron.log 2>&1
set -euo pipefail
cd "$(dirname "$0")/.."

OUT_DIR="${1:-./backups}"
mkdir -p "$OUT_DIR"

STAMP="$(date +%Y%m%d_%H%M%S)"
FILE="$OUT_DIR/keygrid_$STAMP.sql.gz"

docker compose exec -T postgres pg_dump -U keygrid -d keygrid | gzip > "$FILE"

echo "✔ backup → $FILE ($(du -h "$FILE" | cut -f1))"

# 只保留最近 14 份（可按需调整）
ls -1t "$OUT_DIR"/keygrid_*.sql.gz 2>/dev/null | tail -n +15 | xargs -r rm --
