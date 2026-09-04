// Package nfs 演示「网络文件系统」类后端的标准实现（与官方 csi-driver-nfs
// 的核心思路一致）：
//
//   - Provision：controller 容器临时挂载 NFS 根 share → mkdir 子目录 → 卸载。
//     子目录名 = 卷名（pvc-<uuid>），天然幂等。
//   - NodeMount：node 容器把 server:/share/subdir 挂载到 kubelet 指定路径。
//   - 无状态：所有元数据都编码在 VolumeID（server#share#subdir）和
//     VolumeContext 里，driver 重启不丢信息。
//
// 对接其他类 nfs 文件系统（glusterfs、cephfs 的 nfs-ganesha 网关、云厂商
// NAS 的 nfs 协议端点）时，通常只需改挂载参数与 Provision 细节。
package nfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
	"k8s.io/klog/v2"

	"github.com/scaffold/csi-scaffold/pkg/backend"
)

const backendName = "nfs"

// volumeID 编码分隔符：server#share#subdir
const idSep = "#"

type nfsBackend struct {
	mounter mount.Interface
	// 默认 server/share，StorageClass.parameters 未指定时使用。
	defaultServer string
	defaultShare  string
	// archiveOnDelete 为 "true" 时，删除卷改为把子目录改名为 archived-*，
	// 防止误删数据，生产环境强烈建议开启。
	archiveOnDelete bool
	// defaultMountOptions 是挂载时附加的默认选项。容器内没有 rpc.statd，
	// NFSv3 远端锁不可用，必须带 nolock，否则 mount 报 exit status 32。
	defaultMountOptions []string
}

func init() {
	backend.Register(backendName, New)
}

// New 实现 backend.Factory。cfg 来自 --backend-config 的 JSON 文件：
//
//	{"server":"10.0.0.10","share":"/exports","archiveOnDelete":"true","mountOptions":"nolock"}
func New(cfg map[string]string) (backend.Backend, error) {
	b := &nfsBackend{
		mounter:             mount.New(""),
		defaultServer:       cfg["server"],
		defaultShare:        cfg["share"],
		archiveOnDelete:     strings.EqualFold(cfg["archiveOnDelete"], "true"),
		defaultMountOptions: []string{"nolock"},
	}
	if cfg["mountOptions"] != "" {
		b.defaultMountOptions = strings.Split(cfg["mountOptions"], ",")
	}
	return b, nil
}

func (b *nfsBackend) Name() string { return backendName }

// ---------------------------------------------------------------------------
// controller 侧
// ---------------------------------------------------------------------------

func (b *nfsBackend) Provision(ctx context.Context, req *backend.ProvisionRequest) (*backend.Volume, error) {
	server := firstNonEmpty(req.Parameters["server"], b.defaultServer)
	share := firstNonEmpty(req.Parameters["share"], b.defaultShare)
	if server == "" || share == "" {
		return nil, status.Error(codes.InvalidArgument,
			"nfs server/share must be set via StorageClass parameters or backend config")
	}
	// 安全校验：防止参数注入导致挂载到非预期路径。
	if strings.ContainsAny(server, idSep+" \t\n") || strings.Contains(share, idSep) {
		return nil, status.Error(codes.InvalidArgument, "invalid server or share")
	}

	subDir := req.Name // pvc-<uuid>，幂等键
	if err := b.withMountedShare(ctx, server, share, func(root string) error {
		dir := filepath.Join(root, subDir)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return fmt.Errorf("create subdir: %w", err)
		}
		// nfs 上目录属主通常是 root，chmod 0777 保证业务容器可写。
		return os.Chmod(dir, 0o777)
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "provision on nfs: %v", err)
	}

	return &backend.Volume{
		VolumeID:      encodeVolumeID(server, share, subDir),
		CapacityBytes: req.CapacityBytes, // nfs 无配额概念，原样回报
		VolumeContext: map[string]string{
			"server": server,
			"share":  share,
			"subdir": subDir,
		},
	}, nil
}

func (b *nfsBackend) Deprovision(ctx context.Context, volumeID string, _ map[string]string) error {
	server, share, subDir, err := decodeVolumeID(volumeID)
	if err != nil {
		return err
	}

	return b.withMountedShare(ctx, server, share, func(root string) error {
		dir := filepath.Join(root, subDir)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return nil // 幂等契约：卷不存在视为删除成功
		}
		if b.archiveOnDelete {
			archived := filepath.Join(root, "archived-"+subDir)
			klog.InfoS("archiving volume instead of deleting", "from", dir, "to", archived)
			_ = os.RemoveAll(archived)
			return os.Rename(dir, archived)
		}
		return os.RemoveAll(dir)
	})
}

// ValidateVolume 实现 backend.VolumeValidator（可选接口）。
func (b *nfsBackend) ValidateVolume(ctx context.Context, volumeID string, _ map[string]string) error {
	server, share, subDir, err := decodeVolumeID(volumeID)
	if err != nil {
		return err
	}
	return b.withMountedShare(ctx, server, share, func(root string) error {
		if _, err := os.Stat(filepath.Join(root, subDir)); err != nil {
			if os.IsNotExist(err) {
				return status.Errorf(codes.NotFound, "volume %s not found", volumeID)
			}
			return status.Errorf(codes.Internal, "stat volume: %v", err)
		}
		return nil
	})
}

// Expand 实现 backend.Expander（可选接口）：nfs 无配额，直接确认新容量。
func (b *nfsBackend) Expand(_ context.Context, volumeID string, capacityBytes int64, _ map[string]string) (int64, error) {
	if _, _, _, err := decodeVolumeID(volumeID); err != nil {
		return 0, err
	}
	return capacityBytes, nil
}

// ---------------------------------------------------------------------------
// node 侧
// ---------------------------------------------------------------------------

func (b *nfsBackend) NodeMount(ctx context.Context, req *backend.MountRequest) error {
	server := req.VolumeContext["server"]
	share := req.VolumeContext["share"]
	subDir := req.VolumeContext["subdir"]
	if server == "" || share == "" {
		return status.Errorf(codes.InvalidArgument, "volume context missing server/share for volume %s", req.VolumeID)
	}

	source := fmt.Sprintf("%s:%s", server, filepath.Join(share, subDir))
	options := mergeOptions(b.defaultMountOptions, req.MountFlags)
	if err := b.mounter.Mount(source, req.TargetPath, "nfs", options); err != nil {
		return status.Errorf(codes.Internal, "mount %s to %s: %v", source, req.TargetPath, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// withMountedShare 把 NFS 根 share 临时挂载到本地临时目录执行 fn，再卸载。
// controller 侧所有「在远端文件系统上操作目录」的需求都走这个模式，
// 避免在 controller 容器里常驻挂载。
func (b *nfsBackend) withMountedShare(_ context.Context, server, share string, fn func(root string) error) error {
	tmp, err := os.MkdirTemp("", "nfs-controller-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	source := fmt.Sprintf("%s:%s", server, share)
	if err := b.mounter.Mount(source, tmp, "nfs", b.defaultMountOptions); err != nil {
		return status.Errorf(codes.Internal, "temporarily mount %s: %v", source, err)
	}
	defer func() {
		if err := mount.CleanupMountPoint(tmp, b.mounter, true); err != nil {
			klog.ErrorS(err, "failed to cleanup temp mount", "path", tmp)
		}
	}()

	return fn(tmp)
}

func encodeVolumeID(server, share, subDir string) string {
	return strings.Join([]string{server, share, subDir}, idSep)
}

func decodeVolumeID(id string) (server, share, subDir string, err error) {
	parts := strings.SplitN(id, idSep, 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", status.Errorf(codes.InvalidArgument, "malformed volume id: %q", id)
	}
	return parts[0], parts[1], parts[2], nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// mergeOptions 合并默认挂载选项与请求侧选项（StorageClass.mountOptions / ro），
// 按 key 去重，请求侧优先。
func mergeOptions(defaults, extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(defaults)+len(extra))
	for _, o := range append(extra, defaults...) {
		key := strings.SplitN(o, "=", 2)[0]
		if o == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, o)
	}
	return out
}
