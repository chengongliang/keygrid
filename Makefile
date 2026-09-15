.PHONY: build run test tidy web web-dev \
        docker-up docker-up-prod docker-down docker-logs backup restore

# 前端构建（web/dist → Go embed）
web:
	cd web && npm install --no-audit --no-fund && npm run build

web-dev:
	cd web && npm run dev

test:
	go test ./...

# 完整构建：先前端再 Go 单二进制
build: web
	go build -o bin/keygrid .

run:
	set -a; . ./.env; set +a; go run .

# ---- Docker ----

# 开发/本地：app + postgres + redis（db/redis 端口暴露宿主机，供本地 go run 共用）
docker-up:
	docker compose up -d --build

# 生产：收起 db/redis 端口（追加 --profile tls 启用 Caddy HTTPS；SSRF 内网放行由 .env 的
# SSRF_ALLOW_INTERNAL 控制，默认 0）
docker-up-prod:
	docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f app

# 数据库备份/恢复（restore 需指定文件：make restore FILE=backups/keygrid_xxx.sql.gz）
backup:
	bash scripts/backup.sh

restore:
	bash scripts/restore.sh $(FILE)
