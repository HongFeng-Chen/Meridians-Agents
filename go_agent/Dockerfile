# go_agent 运行镜像：多阶段构建，静态二进制（CGO 关闭）
#   docker build -t go-agent:dev .
#   docker run --rm -p 8000:8000 -v $PWD/configs:/app/configs \
#     -e AUTH_DEV_INSECURE=true go-agent:dev          # 本地试跑（关闭认证的容器）
#   docker run --rm -p 8000:8000 -v $PWD/configs:/app/configs \
#     -e AUTH_ENABLED=true -e AUTH_JWT_SECRET=<密钥> go-agent:dev   # 任何对外环境
#
# 说明：配置通过 -conf 目录挂载（k8s 里是 ConfigMap 挂到 /app/configs）；
# 配置值支持 ${ENV:default} 展开，所以密钥一律用环境变量注入，配置文件中不落明文。

FROM golang:1.26-alpine AS builder

WORKDIR /src

# 依赖层单独缓存：go.mod/go.sum 不变时不重复下载
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    GOPROXY=https://goproxy.cn,direct go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/go_agent ./cmd/go_agent

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app

ENV TZ=Asia/Shanghai
# 容器内必须监听 0.0.0.0，端口映射 / Service 才能转发进来。
# config.yaml 默认的 127.0.0.1 是给本机 `kratos run` 用的（认证关闭时的安全默认），
# 这里用环境变量覆盖，不影响二进制在宿主机的行为。
ENV HTTP_ADDR=0.0.0.0:8000 \
    GRPC_ADDR=0.0.0.0:9000

WORKDIR /app
COPY --from=builder /out/go_agent /app/go_agent

# 技能落盘目录（k8s 里挂 PVC）
RUN mkdir -p /app/data/skills && chown -R app:app /app

USER app

EXPOSE 8000 9000

ENTRYPOINT ["/app/go_agent"]
CMD ["-conf", "/app/configs"]
