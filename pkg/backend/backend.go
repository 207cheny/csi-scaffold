// Package backend 是脚手架的核心抽象层。
//
// 对接一个新的文件系统/存储系统时，【只需要】实现本包的 Backend 接口，
// 并通过 Register 注册，identity / controller / node / 部署清单全部复用。
//
// 设计要点：
//   - Backend 是必须实现的最小接口（供应/删除/节点挂载/卸载）；
//   - 可选能力（扩容、卷校验、统计）通过「可选接口 + 类型断言」暴露，
//     controller/node 框架会根据后端实际实现的接口自动声明 CSI 能力，
//     未实现的接口对应的方法返回 codes.Unimplemented，符合 CSI 规范。
//   - 所有方法必须幂等：同一请求重复调用结果一致（见各方法注释）。
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	mount "k8s.io/mount-utils"
)

// Volume 是供应成功后返回给 controller 框架的卷描述。
type Volume struct {
	// VolumeID 全局唯一，必须能从 ID 本身反推出定位卷所需的全部信息
	// （例如 nfs 用 server#share#subdir），这样 DeleteVolume 才能无状态工作。
	VolumeID string
	// CapacityBytes 回报给 k8s 的容量，写入 PV.spec.capacity。
	CapacityBytes int64
	// VolumeContext 会被 k8s 存入 PV.spec.csi.volumeAttributes，
	// 之后 NodePublishVolume 时原样传回 node 侧 —— 挂载所需参数
	// （server/share/subdir 等）都通过它传递，是 controller→node 的
	// 标准数据通道。
	VolumeContext map[string]string
}

// ProvisionRequest 对应 CreateVolume 请求中与业务相关的部分。
type ProvisionRequest struct {
	// Name 是 external-provisioner 生成的唯一卷名（pvc-<uuid>），
	// 天然适合做幂等键：同名重复请求应返回同一个卷。
	Name          string
	CapacityBytes int64
	// Parameters 来自 StorageClass.parameters，是后端最重要的静态配置来源
	// （例如 nfs 的 server/share）。
	Parameters map[string]string
	// Secrets 来自 StorageClass 引用的 Secret，用于认证信息，绝不要写日志。
	Secrets map[string]string
}

// MountRequest 对应 NodeStage/NodePublish 请求中与业务相关的部分。
type MountRequest struct {
	VolumeID string
	// TargetPath 是 kubelet 要求挂载到的路径
	// （publish 时为 /var/lib/kubelet/pods/<poduid>/volumes/.../mount）。
	TargetPath string
	// VolumeContext 即 Provision 时返回的 VolumeContext，由 PV 透传。
	VolumeContext map[string]string
	// Secrets 来自 NodePublishSecret，用于挂载期认证。
	Secrets    map[string]string
	Readonly   bool
	MountFlags []string
}

// Backend 是对接存储系统必须实现的最小接口 —— 只有 3 个方法。
// 所有方法都必须幂等。
//
// 卸载动作（判断挂载点 + umount）对所有文件系统后端完全一致，已下沉为
// 框架默认实现 DefaultNodeUnmount；只有存在特殊卸载语义（如先回收
// 挂载会话）时才需要实现 NodeUnmounter 可选接口。
type Backend interface {
	// Name 返回后端名称，与注册名一致。
	Name() string

	// Provision 创建卷。幂等要求：同 Name 重复调用返回相同 Volume；
	// 若已存在同名但参数冲突的卷，返回 codes.AlreadyExists 错误。
	Provision(ctx context.Context, req *ProvisionRequest) (*Volume, error)

	// Deprovision 删除卷。幂等要求：卷不存在时返回 nil（视为成功），
	// 绝不能返回 NotFound —— 否则 provisioner 会无限重试导致 PV 卡在
	// Terminating。
	Deprovision(ctx context.Context, volumeID string, secrets map[string]string) error

	// NodeMount 把卷挂载到 TargetPath。框架已完成幂等检查（目标已是
	// 挂载点时不会调用本方法）并创建了 TargetPath 目录，实现只需专注
	// 「怎么挂」这一个动作。
	NodeMount(ctx context.Context, req *MountRequest) error
}

// DefaultNodeUnmount 是框架提供的默认卸载实现：幂等检查 + umount。
// node 框架在后端未实现 NodeUnmounter 时调用它。
func DefaultNodeUnmount(mounter mount.Interface, targetPath string) error {
	notMnt, err := mount.IsNotMountPoint(mounter, targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 幂等契约：路径不存在视为已卸载
		}
		return fmt.Errorf("check mount point %s: %w", targetPath, err)
	}
	if notMnt {
		return nil
	}
	return mounter.Unmount(targetPath)
}

// ---------------------------------------------------------------------------
// 以下为可选接口。后端按需实现，框架通过类型断言探测并自动声明对应 CSI 能力。
// ---------------------------------------------------------------------------

// VolumeValidator 可选：校验卷是否仍然存在，用于 ValidateVolumeCapabilities。
type VolumeValidator interface {
	ValidateVolume(ctx context.Context, volumeID string, secrets map[string]string) error
}

// Expander 可选：在线扩容。实现后框架自动声明 EXPAND_VOLUME 能力。
type Expander interface {
	// Expand 把卷扩到 capacityBytes，返回实际生效的容量。
	// 文件存储（如 nfs）通常无配额概念，可直接原样返回。
	Expand(ctx context.Context, volumeID string, capacityBytes int64, secrets map[string]string) (int64, error)
}

// NodeUnmounter 可选：自定义卸载逻辑。不实现时框架使用 DefaultNodeUnmount。
type NodeUnmounter interface {
	// NodeUnmount 卸载 targetPath 上的卷。幂等要求：目标不是挂载点时返回 nil。
	NodeUnmount(ctx context.Context, volumeID string, targetPath string) error
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// Factory 由后端包实现。cfg 来自 --backend-config 指向的 JSON 文件，
// 内容完全由后端自定义（例如 nfs 的默认 server/share）。
type Factory func(cfg map[string]string) (Backend, error)

var registry = map[string]Factory{}

// Register 由各后端包的 init() 调用。
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("backend %q registered twice", name))
	}
	registry[name] = f
}

// New 按名称创建后端实例，并加载 JSON 配置文件（可为空）。
func New(name, configFile string) (Backend, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q, available: %s", name, strings.Join(Names(), ","))
	}
	cfg := map[string]string{}
	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("read backend config: %w", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse backend config %s: %w", configFile, err)
		}
	}
	return f(cfg)
}

// Names 返回所有已注册后端，用于报错提示与文档。
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
