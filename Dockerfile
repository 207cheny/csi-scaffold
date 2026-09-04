# ---------- 构建阶段 ----------
FROM golang:1.23-bookworm AS builder

ARG VERSION=dev
WORKDIR /workspace

# 先拷依赖清单，利用 Docker layer cache 加速增量构建。
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

# 静态编译：运行时镜像无需 libc；版本号通过 ldflags 注入 main.version。
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /csi-scaffold ./cmd/csi-scaffold

# ---------- 运行阶段 ----------
# 必须包含 mount/nfs 工具：node 侧挂载依赖系统的 mount(8) 与 mount.nfs(8)。
# 对接其他文件系统时在这里安装对应客户端（如 ceph-common、glusterfs-client）。
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       nfs-common \
       mount \
       ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /csi-scaffold /csi-scaffold

# 注册到 registry.k8s.io 的官方规范标签（可选，便于镜像管理）。
LABEL org.opencontainers.image.title="csi-scaffold" \
      org.opencontainers.image.description="CSI driver scaffold for filesystem backends"

ENTRYPOINT ["/csi-scaffold"]
