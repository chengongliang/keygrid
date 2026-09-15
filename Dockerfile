# ---- Stage 1: 前端构建（web/dist 给 Go embed）----
FROM node:22-alpine AS frontend
# 国内网络下 npm 官方源易失败，默认走 npmmirror（海外构建可 --build-arg NPM_REGISTRY=... 覆盖）
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm config set registry ${NPM_REGISTRY} && npm install --no-audit --no-fund
# 只拷源码，不拷本地 dist（防 stale 产物 + emptyOutDir EINVAL）
COPY web/index.html web/tsconfig.json web/vite.config.ts ./
COPY web/src ./src
COPY web/public ./public
RUN npm run build

# ---- Stage 2: Go 构建（embed 前端产物 → 单二进制）----
FROM golang:1.25-alpine AS builder
# 国内网络下 golang.org 依赖拉取慢/失败，默认走 goproxy.cn（海外可 --build-arg GOPROXY=direct 覆盖）
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /web/dist web/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /keygrid .

# ---- Stage 3: 运行镜像（非 root + 内置健康检查）----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 app \
    && adduser -D -u 10001 -G app -H app
COPY --from=builder /keygrid /usr/local/bin/keygrid

LABEL org.opencontainers.image.title="keygrid" \
      org.opencontainers.image.description="Team LLM gateway — OpenAI-compatible API with per-user provider isolation" \
      org.opencontainers.image.source="https://github.com/chengongliang/keygrid"

# 监听 8080（>1024，非 root 可绑）
USER app
EXPOSE 8080
# busybox wget 即可，无需额外安装 curl
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["keygrid"]
