# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
FROM golang:1.24-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

# 依赖层缓存：go.mod/go.sum 不变时无需重新下载。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# TARGETARCH 由 buildx/BuildKit 注入：交叉编译代替 QEMU 模拟编译，速度差一个量级；
# 未注入时（无 BuildKit 的现场构建）回退到容器自身架构，即宿主架构。
RUN GOARCH="${TARGETARCH:-$(go env GOARCH)}" go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/devpilot-relay ./cmd/relay \
    && mkdir -p /empty

# ---------- 运行阶段 ----------
# distroless 无 shell，HEALTHCHECK 使用二进制自带的 healthcheck 子命令。
FROM gcr.io/distroless/static-debian12:nonroot

# 许可证随镜像分发：FSL-1.1-ALv2 要求再分发时携带条款。
LABEL org.opencontainers.image.licenses="FSL-1.1-ALv2" \
      org.opencontainers.image.source="https://github.com/devpilot-app/apns"
COPY --from=build /src/LICENSE.md /licenses/LICENSE.md

COPY --from=build /out/devpilot-relay /usr/local/bin/devpilot-relay
# 预创建 /data 并交给 nonroot：命名卷首次挂载会继承该目录的属主。
COPY --from=build --chown=nonroot:nonroot /empty /data

ENV LISTEN_ADDR=:8080 \
    DATA_DIR=/data

EXPOSE 8080
VOLUME ["/data"]

USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/devpilot-relay", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/devpilot-relay"]
CMD ["serve"]
