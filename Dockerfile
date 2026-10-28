# syntax=docker/dockerfile:1

# 第一阶段：编译环境
# CI、本地检查与发布镜像统一使用固定的 Go 工具链。
FROM golang:1.26.10-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app

ENV CGO_ENABLED=1 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
COPY go.mod go.sum ./

# 先下载 go.mod 中已有依赖
RUN go mod download

# 再复制完整源码
COPY . ./

# SQLite 需要 CGO
ARG VERSION=dev
ARG COMMIT=unknown
RUN GOOS=linux go build -trimpath -ldflags "-extldflags '-static' -s -w -X ccoff-abs-bot/internal/buildinfo.Version=${VERSION} -X ccoff-abs-bot/internal/buildinfo.Commit=${COMMIT}" -o ccoff-abs-bot .

# 第二阶段：运行环境
FROM alpine:3.23.3

RUN apk --no-cache add ca-certificates tzdata su-exec

# 创建非 root 用户
RUN addgroup -S app && adduser -S app -G app -u 10001

WORKDIR /app

COPY --from=builder /app/ccoff-abs-bot /app/ccoff-abs-bot
COPY scripts/docker-entrypoint.sh /app/docker-entrypoint.sh

RUN mkdir -p /app/data /app/data-test && chown -R app:app /app && chmod +x /app/docker-entrypoint.sh

HEALTHCHECK --interval=15s --timeout=10s --start-period=90s --retries=4 CMD ["su-exec", "app", "/app/ccoff-abs-bot", "maintenance", "health"]

ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["./ccoff-abs-bot"]
