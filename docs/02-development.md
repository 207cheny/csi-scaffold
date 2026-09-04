# 02 开发篇：对接一个新文件系统的完整流程

假设目标：对接一个新的类 NFS 文件系统（以 GlusterFS 为例，换成任何
NAS/CephFS-nfs-ganesha/云厂商 NAS 同理）。

## 第 0 步：先想清楚 4 个问题（设计评审清单）

| 问题 | 你的答案会决定 |
|---|---|
| 卷 = 什么？（子目录/共享/export/卷组） | Provision/Deprovision 怎么写 |
| 创建卷需要什么参数？ | StorageClass.parameters 与 backend-config 怎么设计 |
| 挂载命令/客户端是什么？ | NodeMount 怎么写、镜像里装什么包 |
| 删除要保护数据吗？ | archiveOnDelete 类策略 |

对类 NFS 系统，答案几乎都是：卷=share 下的子目录，参数=server/share，
挂载=mount -t <fstype>，删除=可选归档。这正是 `pkg/backend/nfs/` 的实现，
所以大多数时候是**复制 nfs 包再改细节**。

## 第 1 步：创建后端包

```bash
cp -r pkg/backend/nfs pkg/backend/glusterfs
```

实现 `backend.Backend` 接口（`pkg/backend/backend.go`）的 3 个方法，
契约逐条如下：

### 1.1 Provision —— 创建卷

输入：`ProvisionRequest{Name, CapacityBytes, Parameters, Secrets}`

- `Name` 是 provisioner 生成的 `pvc-<uuid>`，**必须用它做幂等键**；
- `Parameters` 来自 StorageClass，后端自定义 key（如 server/share）；
- `Secrets` 是认证信息，**绝不打日志**（grpc 层已对 secret 字段脱敏）。

输出：`Volume{VolumeID, CapacityBytes, VolumeContext}`

- `VolumeID` 要自包含（能从 ID 找回卷），推荐 `server#share#subdir` 编码；
- `VolumeContext` 放 node 侧挂载所需的全部参数。

实现模式（controller 容器临时挂载根 share 操作）：

```go
func (b *glusterBackend) Provision(ctx context.Context, req *backend.ProvisionRequest) (*backend.Volume, error) {
    server := firstNonEmpty(req.Parameters["server"], b.defaultServer)
    // 1. 参数校验（注意防注入：拒绝包含分隔符/空白/.. 的参数）
    // 2. 临时挂载 → mkdir 子目录 → 设置权限 → 卸载
    // 3. 返回 Volume
}
```

### 1.2 Deprovision —— 删除卷

**铁律：卷不存在时返回 nil**，否则 PV 卡在 Terminating 无法回收。

```go
if _, err := os.Stat(dir); os.IsNotExist(err) {
    return nil // 幂等契约
}
```

生产建议加 archive 保护：删除改为 rename 成 `archived-<name>`，
由人工或定时任务清理。

### 1.3 NodeMount —— 挂载到 pod

```go
func (b *glusterBackend) NodeMount(ctx context.Context, req *backend.MountRequest) error {
    // 1. 从 req.VolumeContext 取回 Provision 时写入的参数
    // 2. b.mounter.Mount(source, req.TargetPath, "<fstype>", options)
    // 幂等检查框架已做（目标已是挂载点时不会调到这里），无需重复实现
}
```

`req.MountFlags` 合并了 StorageClass.mountOptions 与 PVC 的 ro 设置，
直接透传给 mount 即可。

### 1.4 卸载 —— 默认不用写任何代码

卸载（判断挂载点 + umount + 清理目录）对所有文件系统后端完全一致，
框架用 `backend.DefaultNodeUnmount` 统一处理。只有存在特殊卸载语义
（如先回收挂载会话、先 sync 数据）时，才实现可选接口 `NodeUnmounter`：

```go
func (b *myBackend) NodeUnmount(ctx context.Context, volumeID, targetPath string) error {
    // 自定义逻辑；普通后端永远不需要这个方法
}
```

### 1.5 可选接口（按需）

| 接口 | 实现效果 |
|---|---|
| `Expander` | controller 自动声明 EXPAND_VOLUME，PVC 可在线扩容（需 StorageClass.allowVolumeExpansion=true） |
| `VolumeValidator` | ValidateVolumeCapabilities 时顺带确认卷存在，静态供应场景防呆 |

## 第 2 步：注册后端

```go
// pkg/backend/glusterfs/glusterfs.go
func init() {
    backend.Register("glusterfs", New) // New 是 backend.Factory 签名
}
```

```go
// cmd/csi-scaffold/main.go
import (
    _ "github.com/scaffold/csi-scaffold/pkg/backend/glusterfs" // 注册 glusterfs 后端
)
```

## 第 3 步：更新配置与清单

1. `deploy/backend-config.yaml`：填默认 server/share 等静态参数；
2. `deploy/controller.yaml` / `deploy/node.yaml`：`--backend=glusterfs`；
3. `Dockerfile`：安装对应客户端包（如 `glusterfs-client`）——**没有客户端
   二进制，mount 会报 `wrong fs type`**，这是最常见的运行期错误；
4. `examples/storageclass.yaml`：更新 parameters；
5. 若新后端需要认证：Secret + StorageClass 的
   `csi.storage.k8s.io/provisioner-secret-name` 等注解，Secrets 会进入
   `ProvisionRequest.Secrets` / `MountRequest.Secrets`。

## 第 4 步：本地验证（不依赖集群）

脚手架可以在本机直接跑，用 `csc`（CSI 官方 CLI，
`go install github.com/rexray/gocsi/csc@latest`）手测 gRPC 接口：

```bash
# 启动 driver（hostpath 后端，无需真实存储）
go run ./cmd/csi-scaffold \
  --endpoint=unix:///tmp/csi.sock --nodeid=dev-node \
  --backend=hostpath --backend-config=./dev/hostpath.json --v=4

# 另开终端：建卷 → 发布 → 统计 → 取消发布 → 删卷
export CSI_ENDPOINT=/tmp/csi.sock
csc identity plugin-info
csc controller new -cap 1,mnt/ext3 --req-bytes 1073741824 test-vol-1
csc node publish --target-path /tmp/mnt-test --cap 1,mnt/ext3 --vol-context dir=... <volume-id>
csc node get-info
```

全流程跑通后再上集群，能把 90% 的问题挡在集群外。

## 第 5 步：集群验证

按 [03-deployment.md](03-deployment.md) 执行 `make image push deploy example verify`。

## 开发期的常见坑（血泪清单）

1. **NodeMount 报 permission denied**：node 容器没加 `privileged: true`；
2. **挂载了但 pod 里看不到**：volumeMount 没加
   `mountPropagation: Bidirectional`；
3. **kubelet 找不到 socket**：registrar 的 `--kubelet-registration-path`
   必须与 plugin-dir hostPath 一致；
4. **DeleteVolume 无限重试**：Deprovision 对不存在的卷返回了 NotFound；
5. **mount: wrong fs type**：镜像里没装对应文件系统的客户端工具；
6. **容器重启后 socket listen 失败**：unix socket 残留文件没清理
   （脚手架 server.go 已处理）；
7. **controller 多副本并发建卷冲突**：确认开了 `--leader-election`，
   且 backend 的 Provision 幂等；
8. **Secrets 泄露到日志**：不要自定义打印 request 全量字段，CSI proto 对
   secret 字段的 String() 已脱敏，框架日志依赖这一点；
9. **mount.nfs 报 `rpc.statd is not running`（exit status 32）**：容器里
   没有 NFS 锁服务，NFSv3 挂载必须加 `nolock`（脚手架 nfs 后端已默认追加，
   可用 backend-config 的 `mountOptions` 覆盖）——kind 实机验证时实踩；
10. **NFSv4 挂载报 `No such file or directory`**：服务端 export 带
    `fsid=0` 时该目录成为 v4 伪根，客户端挂载路径要用 `/` 而不是导出路径
    （kind 测试用的 nfs-server-alpine 即此情况，share 配 `/`）；
11. **registrar 报 `dial unix .../csi.sock: no such file or directory`**：
    node 平面的 socket-dir **必须用宿主机插件目录的 hostPath**（不能用
    emptyDir），否则 driver 在 emptyDir 里建的 socket kubelet 看不到。
    controller 平面用 emptyDir 没问题（sidecar 同 pod 共享）。
