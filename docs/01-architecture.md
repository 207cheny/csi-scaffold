# 01 架构篇：CSI 是什么，脚手架怎么设计的

## 1. 为什么需要 CSI

早期 K8s 的存储驱动写在 k8s 源码树里（in-tree），加一个存储厂商要改
kube-controller-manager / kubelet 的代码、跟 k8s 一起发版，厂商与社区都痛苦。
CSI（Container Storage Interface）把存储能力抽象成一组 gRPC 接口，存储厂商
以**独立进程 + 独立发布**的方式实现，K8s 通过标准 sidecar 与之交互。
从 v1.26 起 in-tree 驱动基本全部移除，CSI 是唯一扩展方式。

## 2. CSI 的三个服务（Spec 视角）

CSI spec 定义了三个 gRPC 服务，通常一个二进制同时实现，挂在同一个
unix socket 上：

| 服务 | 核心方法 | 谁调用它 | 干什么 |
|---|---|---|---|
| **Identity** | GetPluginInfo / Probe / GetPluginCapabilities | 所有 sidecar | 自报家门：名字、版本、能力、健康 |
| **Controller** | CreateVolume / DeleteVolume / ControllerExpandVolume / (ControllerPublishVolume) | external-provisioner / external-resizer / (external-attacher) | 卷的生命周期：供应、删除、扩容、（挂到节点） |
| **Node** | NodePublishVolume / NodeUnpublishVolume / NodeGetVolumeStats / NodeGetInfo | kubelet（经 node-driver-registrar 注册后） | 把卷挂进 pod 的挂载点 |

脚手架中对应代码：

```
pkg/identity/identity.go     → Identity 服务
pkg/controller/controller.go → Controller 服务
pkg/node/node.go             → Node 服务
pkg/driver/server.go         → 一个 unix socket 同时注册三个服务
```

## 3. K8s 侧的完整调用链（重点，面试必问）

```
用户创建 PVC
   │
   ▼
external-provisioner（controller pod 里的 sidecar，watch PVC）
   │ 发现 provisioner == scaffold.csi.example.com 且没有对应 PV
   │ 通过 /csi/csi.sock 发起 gRPC
   ▼
Controller.CreateVolume ──→ backend.Provision（在 NFS 上 mkdir 子目录）
   │ 返回 VolumeID + VolumeContext
   ▼
provisioner 创建 PV 对象（volumeHandle=VolumeID, volumeAttributes=VolumeContext）
   │ PVC ↔ PV 绑定完成
   ▼
用户创建 Pod，调度到节点 N
   │
   ▼
kubelet（节点 N 上）通过 /var/lib/kubelet/plugins/<driver>/csi.sock 发起 gRPC
   ▼
Node.NodePublishVolume ──→ backend.NodeMount（mount -t nfs server:/share/subdir target）
   │
   ▼
kubelet 启动业务容器，容器内看到挂载好的卷
```

删除方向正好相反：删 Pod → kubelet 调 `NodeUnpublishVolume`（umount）；
PVC 删除且回收策略为 Delete → provisioner 调 `DeleteVolume`。

**关键认知：driver 本身不 watch 任何 K8s 对象**。所有「事件感知」都由
官方 sidecar 完成，driver 只需要纯 gRPC 接口实现。这就是为什么一个 driver
的部署清单里 sidecar 比业务容器还多。

## 4. sidecar 全家福

| sidecar | 部署位置 | 职责 | 何时需要 |
|---|---|---|---|
| external-provisioner | controller | PVC→PV 动态供应/删除 | 几乎总是 |
| external-attacher | controller | 调 ControllerPublish/Unpublish（attach 阶段） | 块存储（云盘/iSCSI）需要；nfs 类不需要（CSIDriver.attachRequired=false） |
| external-resizer | controller | 在线扩容 | 实现 Expander 接口时 |
| external-snapshotter | controller | 卷快照 | 实现快照接口时 |
| node-driver-registrar | node | 向 kubelet 注册 driver，回写 CSINode | 必须 |
| livenessprobe | 两者 | 调 Probe 暴露 /healthz 给容器探针 | 推荐 |

脚手架默认部署 provisioner + resizer + registrar + livenessprobe 四个。
对接块存储时把 attacher 加回来，并把 `deploy/csidriver.yaml` 的
`attachRequired` 改为 true。

## 5. 脚手架的分层设计

```
┌─────────────────────────────────────────────────┐
│ cmd/csi-scaffold/main.go  入口：组装 + 信号处理    │
├─────────────────────────────────────────────────┤
│ pkg/driver    gRPC server / config / 组装         │  通
│ pkg/identity  Identity 服务                       │  用
│ pkg/controller Controller 框架（校验/串行化/能力）  │  骨
│ pkg/node      Node 框架（幂等检查/统计/清理）       │  架
├─────────────────────────────────────────────────┤
│ pkg/backend   Backend 接口 + 注册表               │ ★ 业务层
│   ├── nfs/      NFS 实现                          │   对接新存储
│   └── hostpath/ 本地目录实现                       │   只改这里
└─────────────────────────────────────────────────┘
```

### 设计决策 1：Backend 接口收敛到 3 个方法

```go
type Backend interface {
    Name() string
    Provision(ctx, *ProvisionRequest) (*Volume, error)   // controller 侧建卷
    Deprovision(ctx, volumeID string, secrets) error     // controller 侧删卷
    NodeMount(ctx, *MountRequest) error                  // node 侧挂载
}
```

对文件系统类存储，建卷/删卷/挂载就是全部业务。**卸载没有出现在接口里**：
对所有后端它都是同一个动作（判断挂载点 + umount），下沉为框架默认实现
`DefaultNodeUnmount`；有特殊卸载语义的后端才实现可选接口 `NodeUnmounter`。
同理，挂载的幂等检查也由框架统一完成，后端 NodeMount 只需专注「怎么挂」。
接口越小，每个新后端要写的样板代码越少，脚手架的复用价值越大。

### 设计决策 2：可选能力用「接口 + 类型断言」探测

```go
if _, ok := be.(backend.Expander); ok { caps = append(caps, EXPAND_VOLUME) }
```

后端实现了 `Expander` 接口，controller 才声明 `EXPAND_VOLUME` 能力。
**为什么不能先声明能力再返回 Unimplemented？** 因为 sidecar 看到能力声明
就会发起调用，调用失败后按退避策略反复重试，造成无效流量和用户困惑
（PVC 的 resize 卡住）。能力与实现必须一致。

### 设计决策 3：无状态 driver

不维护任何本地状态数据库（对比 hostpath 官方 demo 的 state.json，那只是
为了模拟块设备）。所有定位信息编码在两个通道里：

- **VolumeID**：`server#share#subdir`，DeleteVolume 只拿到 ID，必须能从
  ID 反推出卷在哪 → 无状态删除；
- **VolumeContext**：`{server, share, subdir}`，CreateVolume 返回后由 K8s
  存进 PV，NodePublish 时原样回传 → 这是 controller 向 node 传递挂载参数
  的**唯一标准通道**。

### 设计决策 4：幂等性是契约而不是可选项

K8s 的所有 controller 都是 level-based（追求终态），任何一步都可能重试：

| 方法 | 幂等实现 |
|---|---|
| Provision | 子目录名 = pvc-\<uuid\>（provisioner 保证同名重试），mkdir 已存在即成功 |
| Deprovision | 卷不存在返回 **nil**（返回 NotFound 会让 PV 永远 Terminating） |
| NodeMount | 挂载前先 IsNotMountPoint 检查，已是挂载点直接返回 |
| NodeUnmount | 不是挂载点返回 nil |

### 设计决策 5：controller 与 node 分离部署

同一个二进制，两套清单，靠启动参数区分角色：

- controller（Deployment）：socket 在 pod 内 emptyDir，sidecar 同 pod 直连；
- node（DaemonSet）：socket 要暴露给宿主机 kubelet，走 hostPath + 插件注册。

两者甚至可以用不同 backend 配置——但通常保持一致。

## 6. 与官方 csi-driver-nfs 的对比

| 维度 | csi-driver-nfs | 本脚手架 |
|---|---|---|
| 定位 | 单一 NFS driver | 多文件系统脚手架 |
| 业务/骨架 | 耦合在 driver struct 上 | Backend 接口隔离 |
| 状态 | 无状态（同样 ID 编码） | 无状态 |
| 新增文件系统 | fork 改全部 | 实现 4 个方法 |
| 能力声明 | 硬编码 | 按后端接口自动推导 |
