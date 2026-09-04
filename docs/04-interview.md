# 04 面试篇：15 分钟讲透这个项目的完整开发与部署

> 使用方法：第一节是可以直接讲的「主线叙事」，后面是按主题整理的高频
> 追问与参考答案。主线讲 5-8 分钟，剩下时间应对追问。

## 一、主线叙事（直接可讲）

**「这个项目解决什么问题」**

我们团队需要对接多种文件系统类存储（NFS、内部 NAS 等），如果每种都从零
写一个 CSI driver，identity/controller/node 三层服务、gRPC server、sidecar
部署清单这些代码要重复写 N 遍，而它们其实与存储系统无关。我以 K8s 官方
示例 csi-driver-host-path 为蓝本做了架构解耦，沉淀出一个脚手架：
**对接新文件系统只需要实现一个 3 方法的 Backend 接口，其余全部复用。**

**「先讲 CSI 本身」**

CSI 是 K8s 存储扩展的标准接口，把存储能力抽象成三组 gRPC 服务：
Identity 负责自报家门和健康探测；Controller 负责卷的生命周期管理，
比如 CreateVolume、DeleteVolume；Node 负责在节点上把卷挂进 pod，
比如 NodePublishVolume。driver 本身不感知任何 K8s 对象，所有事件监听
由官方 sidecar 完成——provisioner watch PVC 后调我的 CreateVolume，
kubelet 在 pod 启动前调我的 NodePublishVolume。

**「调用链」（建议边画边讲）**

用户创建 PVC → external-provisioner 发现 provisioner 名字匹配 →
gRPC 调 Controller.CreateVolume → 我的 NFS backend 临时挂载根 share、
创建以 pvc-uuid 命名的子目录 → 返回 VolumeID 和 VolumeContext →
provisioner 据此创建 PV，两者绑定。用户建 Pod → 调度 → kubelet 调
Node.NodePublishVolume → node 容器执行 mount -t nfs 把子目录挂到
/var/lib/kubelet/pods 下的目标路径 → 业务容器启动看到数据。
这里有个关键设计：**VolumeContext 是 controller 向 node 传参的唯一通道**，
K8s 会把它存进 PV 的 volumeAttributes，publish 时原样回传。

**「脚手架的分层」**

代码分两层：通用骨架和业务层。骨架层包括非阻塞 gRPC server、参数校验、
并发串行化、CSI 能力声明、挂载点幂等检查、默认卸载实现、statfs 统计；
业务层就是 Backend 接口，只有 Provision、Deprovision、NodeMount 三个
方法——卸载对所有文件系统都一样，所以下沉成了框架默认实现，挂载幂等
检查也由框架统一做，后端只写「怎么建、怎么删、怎么挂」。
可选能力比如扩容，我设计成「可选接口 + 类型断言」：后端实现了 Expander
接口，controller 才声明 EXPAND_VOLUME 能力——**能力与实现必须一致**，
否则 sidecar 会按能力发起调用再无限重试失败。

**「无状态与幂等」**

driver 完全无状态：VolumeID 编码成 server#share#subdir，DeleteVolume
只凭 ID 就能定位卷，不依赖任何本地数据库。所有方法都是幂等的：
Provision 用 pvc-uuid 做天然幂等键；Deprovision 对不存在的卷返回成功，
否则 PV 会永远卡在 Terminating；NodeMount 前先检查是否已是挂载点。
因为 K8s 的所有 controller 都是 level-based 追求终态的，任何调用都可能
重试，幂等是契约不是优化。

**「部署」**

标准的双平面部署：controller 是 Deployment，跑 provisioner、resizer、
livenessprobe 和 driver 四个容器，socket 走 pod 内 emptyDir 共享；
node 是 DaemonSet，每节点一份，跑 node-driver-registrar、livenessprobe
和 driver。registrar 通过宿主机 kubelet 的插件注册目录把 driver 注册进去，
并把 NodeGetInfo 的结果写进 CSINode 对象。打包是多阶段 Dockerfile，
静态编译，运行时镜像装 nfs-common——因为挂载最终是 exec 系统的
mount.nfs 完成的。一条 make image push deploy 完成构建、推送、
按 rbac→csidriver→controller→node 顺序部署并等待就绪。

**「踩过的最深的坑」**

node 平面部署三要素：privileged 提供 mount 所需的 CAP_SYS_ADMIN；
mountPropagation Bidirectional 让容器内挂载传播到宿主机，否则挂载成功
但 pod 里看不到；hostPath 挂 /var/lib/kubelet 让容器与 kubelet 看到同一份
挂载视图。这三个少任何一个，现象都是诡异的挂载失败。

另外在 kind 上实机验证时还踩过两个很典型的坑：一是 node 平面的 socket
目录必须直接 hostPath 到 kubelet 插件目录，用 emptyDir 的话 driver 建的
socket kubelet 根本 dial 不到，registrar 会报 no such file or directory；
二是容器镜像里没有 rpc.statd，NFSv3 挂载必须加 nolock，这个我现在做成
了后端可配置的默认挂载选项。

## 二、高频追问

### Q1：CSI 相比 in-tree / FlexVolume 的优势？

in-tree 驱动代码在 k8s 仓库里，厂商无法独立迭代，v1.26 起基本全部移除；
FlexVolume 是 exec 二进制模式，功能弱（不支持快照/扩容/拓扑）、
要求节点预装驱动文件。CSI 是 out-of-tree 的 gRPC 标准接口，driver 独立
发布、容器化部署、能力完整，是目前唯一推荐方式。

### Q2：external-attacher 是干什么的，你为什么没部署？

attach 是块存储特有的阶段：把云盘「挂到某台节点」这个动作（对应
ControllerPublishVolume），由 attacher 调 driver 完成。NFS 是网络文件
系统，任何节点都能直接挂载，没有 attach 概念，所以 CSIDriver 对象里
`attachRequired: false`，kubelet 直接走 publish。对接块存储时要把
attacher 加回来，Backend 接口要扩展 Attach/Detach 两个方法。

### Q3：Stage 和 Publish 有什么区别，为什么你没有 Stage？

NodeStageVolume 是把卷挂到节点级临时目录（全局一次），
NodePublishVolume 再从 stage 目录 bind mount 到每个 pod 的目录（每 pod
一次）。块设备需要这个两阶段，因为设备要先格式化/挂载一次再供多个 pod
绑定。文件系统直接 publish 挂载即可，所以脚手架不声明
STAGE_UNSTAGE_VOLUME 能力，kubelet 就只会调 publish。

### Q4：controller 多副本怎么做？

sidecar 自带 leader election（--leader-election，基于 leases），多副本
部署时只有 leader 真正发起 CSI 调用。但还要评估后端并发能力：
脚手架 controller 层有一把互斥锁串行化变更操作，后端 Provision 幂等，
两个条件都满足才能安全开多副本。

### Q5：容量是怎么处理的？

对 NFS 这类共享文件系统，单卷配额是伪概念——整个 share 共享空间，
CreateVolume 里的 required bytes 我原样回报给 PV，但不强制隔离。
真正的配额要靠存储侧能力（如 xfs project quota / 存储系统 API），
这是 nfs 类 driver 的通性，csi-driver-nfs 官方也是同样处理。
NodeGetVolumeStats 用 statfs 返回的是整个 share 的用量，供
kubectl top 和驱逐参考。

### Q6：Secrets 怎么管理？

三类：静态配置走 ConfigMap（backend-config）；认证信息走 Secret，
通过 StorageClass 的 csi.storage.k8s.io/provisioner-secret-name 等
标准注解引用，K8s 会把内容注入 CreateVolume/NodePublish 的 secrets
字段；日志侧，CSI proto 对 secret 字段的 String() 自动脱敏，
框架日志打印完整请求也不会泄露。

### Q7：怎么做快照 / 临时卷 / 拓扑感知？

都遵循同一模式：实现对应 CSI 方法 + 部署对应 sidecar + 声明对应能力。
快照：实现 CreateSnapshot/DeleteSnapshot + 部署 snapshotter sidecar；
临时卷（ephemeral inline）：CSIDriver 的 volumeLifecycleModes 加
Ephemeral，kubelet 会直接调 NodePublish 而不经过 provisioner；
拓扑：identity 声明 VOLUME_ACCESSIBILITY_CONSTRAINTS，SC 用
WaitForFirstConsumer，provisioner 会把调度结果作为
AccessibilityRequirements 传给 CreateVolume。脚手架预留了这些扩展点，
文件存储场景一般用不上所以没有默认实现。

### Q8：线上升级怎么保证不断数据面？

关键认知：**挂载好的卷不依赖 driver 进程存活**，driver 挂了只是不能
新建/新挂，存量 pod 的 IO 走内核 nfs 客户端不受影响。升级顺序：
先升 controller（卷生命周期短暂不可用，provisioner 会重试），
再滚动升 node DaemonSet（滚动期间逐节点短暂不可新挂载）。
最大的风险点是 VolumeID 格式变更——存量 PV 的 volumeHandle 是固化的，
新 driver 必须能解析旧格式，所以编码格式上线即冻结，变更必须向后兼容。

### Q9：为什么不直接用官方的 csi-driver-nfs？

三个原因：一，它是单一 driver，业务和骨架耦合，我们要对接多种文件系统，
每次 fork 改全量代码维护成本随数量线性增长；二，脚手架把能力声明做成了
按后端实现自动推导，减少人为配置错误；三，脚手架沉淀了部署清单、
RBAC、Makefile 链路和排障文档，新存储接入从「周」降到「天」。

### Q10：怎么测试的？

三层：本机用 csc（CSI 官方 CLI）直接调 gRPC 接口做接口级测试，不依赖
集群；集群层用 make example verify 做端到端读写验证；另外 K8s 社区有
csi-test（sanity 测试套件）和 e2e storage 框架，可以直接对 driver 跑
标准一致性测试，这是合入前的质量门禁。

### Q11：如果让你支持块存储（比如云盘），要改什么？

这是检验架构理解的好问题。需要：1）Backend 接口扩展 Attach/Detach；
2）controller 声明 PUBLISH_UNPUBLISH 能力 + 部署 attacher sidecar +
CSIDriver 改 attachRequired: true；3）node 侧实现 Stage/Unstage
（设备格式化、挂载到 staging 目录）；4）CreateVolume 改为调云厂商
OpenAPI 建盘而不是 mkdir。骨架层（server/identity/部署框架）依然复用，
这正是脚手架分层价值的体现。

### Q12：安全方面做了什么？

1）node 容器 privileged 是刚需，但 controller 若后端走 API 可以不要特权；
2）路径穿越防护：Provision 校验 server/share 参数、hostpath 后端拒绝
含路径分隔符的卷名；3）Secrets 不入日志，依赖 proto 脱敏；4）RBAC 最小
权限：node 平面零 apiserver 权限；5）archiveOnDelete 防误删数据；
6）镜像最小化 debian-slim + 静态编译，减少攻击面。

## 三、一页纸架构图（面试白板用）

```
                 ┌──────────────────────── K8s 集群 ────────────────────────┐
                 │                                                          │
 PVC ─watch─► ┌──┴─────────── Deployment: controller ───────────┐           │
              │ provisioner ──┐                                  │           │
              │ resizer ──────┼── /csi/csi.sock ──► csi-scaffold │──► NFS    │
              │ livenessprobe─┘   (emptyDir)        CreateVolume │  mkdir    │
              └──────────────────────────────────────────────────┘           │
                                     ▲                                       │
                 ┌───────────────────┴───────────────┐                      │
                 │ 每节点 DaemonSet: node             │                      │
 kubelet ──────► │ registrar ──► /var/lib/kubelet/    │                      │
 NodePublish     │ livenessprobe  plugins_registry    │                      │
                 │ csi-scaffold: mount -t nfs ────────┼──► /var/lib/kubelet/ │
                 │   privileged + Bidirectional       │      pods/.../mount  │
                 └────────────────────────────────────┘                      │
                 └──────────────────────────────────────────────────────────┘

 driver 代码分层：cmd → driver/identity/controller/node（骨架）→ backend（业务）
```
