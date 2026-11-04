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

# 只复制应用源码，文档、部署脚本等改动不会使编译输入层失效。
COPY main.go ./
COPY internal/ ./internal/

# SQLite 需要 CGO
ARG VERSION=dev
ARG COMMIT=unknown
# 本地由 BuildKit 保留；CI 单独导入/导出此挂载，供下一次构建增量编译。
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=linux go build -trimpath -ldflags "-extldflags '-static' -s -w -X ccoff-abs-bot/internal/buildinfo.Version=${VERSION} -X ccoff-abs-bot/internal/buildinfo.Commit=${COMMIT}" -o ccoff-abs-bot .

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
