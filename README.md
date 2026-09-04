# csi-scaffold

一个**面向文件系统类存储的 CSI Driver 脚手架**。以 K8s 官方示例
`csi-driver-host-path` 为蓝本提炼：identity / controller / node / gRPC server /
部署清单全部是通用骨架，对接新文件系统**只需实现一个 `Backend` 接口**。

## 5 分钟跑通

```bash
# 1. 本地编译验证
make build

# 2. 构建并推送镜像（先改 Makefile 里的 REGISTRY）
make image push VERSION=v0.1.0

# 3. 部署到集群（改 deploy/backend-config.yaml 里的 nfs server/share）
make deploy

# 4. 创建示例 PVC + Pod 并验证读写
make example verify
```

## 对接一个新文件系统只需 3 步

1. 新建 `pkg/backend/<yourfs>/`，实现 `backend.Backend` 接口的 3 个方法
   （`Provision` / `Deprovision` / `NodeMount`；卸载由框架默认实现，
   无需编写），参考 `pkg/backend/nfs/` 与 `pkg/backend/hostpath/`；
2. 在该包的 `init()` 中 `backend.Register("<yourfs>", New)`，并在
   `cmd/csi-scaffold/main.go` 中匿名 import 该包；
3. 更新 `deploy/backend-config.yaml` 与 `examples/storageclass.yaml` 的参数。

不需要碰：gRPC server、identity、controller、node、RBAC、sidecar 配置。

## 目录结构

```
cmd/csi-scaffold/     入口：解析参数 → 创建后端 → 组装并运行三个 CSI 服务
pkg/
├── driver/           通用骨架：配置、非阻塞 gRPC server、服务组装
├── identity/         通用骨架：GetPluginInfo / Probe / GetPluginCapabilities
├── controller/       通用框架：CreateVolume/DeleteVolume/Expand，委托 backend
├── node/             通用框架：Publish/Unpublish/Stats，委托 backend
└── backend/          ★ 业务抽象层（唯一需要为不同存储改动的部分）
    ├── backend.go      Backend 接口 + 可选接口（Expander/VolumeValidator）+ 注册表
    ├── nfs/            NFS 后端实现（类 nfs 文件系统模板）
    └── hostpath/       本地目录后端（开箱测试 + 最小实现样本）
deploy/               CSIDriver / RBAC / controller Deployment / node DaemonSet
examples/             StorageClass / PVC / Pod 示例
docs/                 架构、开发、部署、面试讲解四份详细文档
```

## 文档

| 文档 | 内容 |
|---|---|
| [docs/01-architecture.md](docs/01-architecture.md) | CSI 架构、调用链、脚手架设计决策 |
| [docs/02-development.md](docs/02-development.md) | 对接新文件系统的完整开发流程 |
| [docs/03-deployment.md](docs/03-deployment.md) | 打包、部署、验证、排障全流程 |
| [docs/04-interview.md](docs/04-interview.md) | 面试讲解稿（含高频追问与答案） |
