# 03 部署篇：从源码到集群可用的完整链路

## 0. 前置条件

| 依赖 | 版本要求 | 说明 |
|---|---|---|
| Go | ≥ 1.22 | 本地编译 |
| Docker / containerd | 任意现代版本 | 构建镜像 |
| K8s 集群 | ≥ 1.25 | sidecar 版本兼容性 |
| NFS 服务器 | v3/v4 | 示例后端依赖（如 192.168.1.10:/exports） |
| 节点操作系统 | Linux | 节点需能访问 NFS 服务器 |

准备 NFS（示例，Ubuntu 服务端）：

```bash
apt-get install -y nfs-kernel-server
mkdir -p /exports && chmod 777 /exports
echo '/exports *(rw,sync,no_subtree_check,no_root_squash)' >> /etc/exports
exportfs -ra
```

**kind/本地集群没有 NFS 服务器时**，可以直接在集群内跑一个测试用服务端：

```bash
kubectl apply -f deploy/test/nfs-server.yaml
kubectl -n csi-scaffold get svc nfs-server   # 拿 ClusterIP 填进 backend-config 和 StorageClass
```

注意该镜像（nfs-server-alpine）导出带 `fsid=0`，即 NFSv4 伪根，
此时 share 要配 `/` 而不是 `/exports`；且容器内挂载需 `nolock`
（脚手架 nfs 后端已默认处理）。

## 1. 打包链路

### 1.1 本地编译（开发验证）

```bash
make build   # → bin/csi-scaffold，版本号通过 ldflags 注入 main.version
make test    # go test + go vet
```

### 1.2 构建镜像

```bash
make image VERSION=v0.1.0
```

Dockerfile 两阶段：

```
阶段1 golang:1.22-bookworm  go mod download（缓存层）→ CGO_ENABLED=0 静态编译
阶段2 debian:bookworm-slim  安装 nfs-common + mount + ca-certificates，拷二进制
```

要点：
- **CGO_ENABLED=0**：静态二进制，避免 glibc 版本地狱；
- **运行时镜像必须装 `nfs-common` 和 `mount`**：mount-utils 是 exec 系统的
  mount(8)/mount.nfs(8) 完成挂载的，缺了会在 NodePublish 报
  `wrong fs type, bad option`；
- 版本号在构建期注入：`--build-arg VERSION=v0.1.0` → `-X main.version=`，
  之后 `GetPluginInfo` 与日志里都能看到，排查线上版本混乱必备。

### 1.3 推送镜像

```bash
# 先改 Makefile 顶部 REGISTRY 为你的仓库
make push VERSION=v0.1.0
```

私有仓库记得给集群配 imagePullSecret，或用节点级 containerd 凭证。

## 2. 部署链路

`make deploy` 内部只有三步：创建 namespace →
`kustomize edit set image` 注入镜像 tag → `kubectl apply -k deploy/`。
kustomize 会**自动按资源依赖关系排序**（RBAC → CSIDriver → ConfigMap →
工作负载），无需手工维护 apply 顺序。以下按逻辑顺序解释每类资源的作用：

### 2.1 namespace + RBAC（`deploy/rbac.yaml`）

```bash
kubectl create namespace csi-scaffold
kubectl apply -f deploy/rbac.yaml
```

创建：controller SA、provisioner ClusterRole（PV/PVC/StorageClass/Node 权限）、
resizer ClusterRole、leader election Role（leases/configmaps）。
**node 侧不需要 apiserver 权限**——registrar 走的是宿主机 kubelet 的
插件注册 socket。

### 2.2 CSIDriver 对象（`deploy/csidriver.yaml`）

```bash
kubectl apply -f deploy/csidriver.yaml
```

这是「声明」不是「运行」：告诉 K8s 这个 driver 不需要 attach 阶段
（`attachRequired: false`）、支持 Persistent 卷。

### 2.3 后端配置（`deploy/backend-config.yaml`）

ConfigMap 挂到 `/etc/csi-backend/config.json`，改配置后 `kubectl rollout
restart` 生效。

### 2.4 controller Deployment

```bash
kubectl apply -n csi-scaffold -f deploy/controller.yaml
kubectl -n csi-scaffold rollout status deployment/csi-scaffold-controller
```

4 容器：csi-provisioner / csi-resizer / livenessprobe / csi-scaffold。
socket 通过 pod 内 emptyDir `/csi` 共享。

**就绪验证**：

```bash
kubectl -n csi-scaffold logs deploy/csi-scaffold-controller -c csi-scaffold
# 期望看到：starting csi driver ... backend=nfs ... listening for connections
kubectl -n csi-scaffold logs deploy/csi-scaffold-controller -c csi-provisioner
# 期望看到：successfully acquired lease / connected to CSI driver
```

### 2.5 node DaemonSet

```bash
kubectl apply -n csi-scaffold -f deploy/node.yaml
kubectl -n csi-scaffold rollout status daemonset/csi-scaffold-node
```

3 容器：node-driver-registrar / livenessprobe / csi-scaffold。
registrar 启动后会调用 `NodeGetInfo`，把结果写入 CSINode 对象：

```bash
kubectl get csinodes
# 期望：每个节点都列出 scaffold.csi.example.com
kubectl describe csinode <node> | grep -A3 scaffold
```

**CSINode 里出现你的 driver 名，是 node 平面部署成功的金标准。**

## 3. 业务验证（端到端）

```bash
make example        # StorageClass + PVC + Pod
make verify         # 在 pod 里写文件再读出来
```

逐步观察（排障也用这条链）：

```bash
# 1. PVC 是否绑定（几秒到几十秒）
kubectl get pvc csi-scaffold-pvc    # STATUS 应为 Bound

# 2. PV 的关键字段
kubectl get pv -o custom-columns=NAME:.metadata.name,DRIVER:.spec.csi.driver,\
HANDLE:.spec.csi.volumeHandle,ATTRS:.spec.csi.volumeAttributes
# HANDLE 应形如 192.168.1.10#/exports#pvc-xxxx

# 3. NFS 服务器上应看到子目录
ls /exports/    # 有 pvc-<uuid> 目录

# 4. Pod 调度后，节点上应看到 nfs 挂载
mount | grep csi-scaffold

# 5. 读写验证
kubectl exec csi-scaffold-demo -- sh -c 'echo hello > /data/a && cat /data/a'
```

### 扩容验证（backend 实现了 Expander）

```bash
kubectl patch pvc csi-scaffold-pvc -p '{"spec":{"resources":{"requests":{"storage":"10Gi"}}}}'
kubectl get pvc csi-scaffold-pvc   # 容量变为 10Gi
```

### 删除验证

```bash
make clean-example
kubectl get pv      # PV 应消失（reclaimPolicy=Delete）
ls /exports/        # archiveOnDelete=true 时子目录变为 archived-pvc-xxx
```

## 4. 升级与回滚

```bash
# 升级：构建新版本 → 推送 → 重新 deploy（kustomize 替换镜像 tag + apply -k）
make image push deploy VERSION=v0.2.0

# 回滚
kubectl -n csi-scaffold rollout undo deployment/csi-scaffold-controller
kubectl -n csi-scaffold rollout undo daemonset/csi-scaffold-node
```

**升级注意事项**：已存在的 PV 的 volumeHandle/volumeAttributes 是创建时
固化的，driver 升级后必须能继续解析旧 ID 格式 → **VolumeID 编码格式一旦
上线不可破坏式变更**，要变就做双格式兼容解析。

## 5. 排障决策树

```
PVC 一直 Pending？
├─ describe pvc 看 Events
│   ├─ "waiting for a volume to be created" → 看 provisioner 日志
│   │   ├─ 连不上 socket → controller pod 是否 Ready、socket-dir 是否共享
│   │   └─ CreateVolume 报错 → 看 csi-scaffold 容器日志（--v=4 有完整请求）
│   └─ "storageclass not found" → provisioner 名字与 SC.provisioner 是否一致
│
Pod 一直 ContainerCreating？
├─ describe pod 看 Events
│   ├─ "driver not found" → kubectl get csinodes 是否有该 driver
│   │   └─ 没有 → 看 registrar 日志（注册路径/权限）
│   └─ MountVolume.SetUp failed → 看 node pod 的 csi-scaffold 日志
│       ├─ permission denied → privileged 是否开启
│       ├─ wrong fs type → 镜像里客户端工具是否安装
│       └─ 挂载成功但 pod 看不到 → mountPropagation: Bidirectional
│
PV 卡在 Terminating？
└─ DeleteVolume 对不存在的卷返回了错误 → 修为返回 nil
```

日志采集速查：

```bash
make logs                                        # driver 本体
kubectl -n csi-scaffold logs deploy/csi-scaffold-controller -c csi-provisioner
kubectl -n csi-scaffold logs ds/csi-scaffold-node -c node-driver-registrar
```

## 6. CI/CD 参考（cloudbuild / GitLab CI 伪码）

```yaml
steps:
  - make test                                  # 单测 + vet
  - docker build --build-arg VERSION=$TAG -t $REG/csi-scaffold:$TAG .
  - docker push $REG/csi-scaffold:$TAG
  - make deploy VERSION=$TAG REGISTRY=$REG     # 目标集群 kubeconfig 由 CI 注入
```

生产建议：镜像 tag 用 git tag/commit，禁止 latest 进生产集群
（latest 无法回滚也无法确定线上版本）。
