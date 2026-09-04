// Package node 实现 CSI Node 服务的通用框架。
//
// 框架负责：kubelet 请求校验、挂载点幂等检查、卸载清理、卷统计；
// 实际的「怎么挂载」委托给 backend.Backend.NodeMount；卸载默认由框架
// 通用实现完成（backend.DefaultNodeUnmount），后端可通过 NodeUnmounter
// 可选接口覆盖。
// 对接新存储系统时本文件不需要改动。
package node

import (
	"context"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
	"k8s.io/klog/v2"

	"github.com/scaffold/csi-scaffold/pkg/backend"
)

type Server struct {
	csi.UnimplementedNodeServer
	nodeID  string
	backend backend.Backend
	mounter mount.Interface
}

func NewServer(nodeID string, be backend.Backend) *Server {
	return &Server{
		nodeID:  nodeID,
		backend: be,
		mounter: mount.New(""),
	}
}

// NodePublishVolume 由 kubelet 在 pod 调度到本节点、容器启动前调用。
// 对 nfs 类文件存储通常不走 stage 阶段，publish 直接做真实挂载。
func (s *Server) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	// 幂等检查 1：目标已是挂载点 → 认为已发布成功，直接返回。
	notMnt, err := mount.IsNotMountPoint(s.mounter, target)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return nil, status.Errorf(codes.Internal, "create target path: %v", err)
			}
			notMnt = true
		} else {
			return nil, status.Errorf(codes.Internal, "check mount point: %v", err)
		}
	}
	if !notMnt {
		klog.V(4).InfoS("volume already published", "volumeID", req.GetVolumeId(), "target", target)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	mountFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	if req.GetReadonly() {
		mountFlags = append(mountFlags, "ro")
	}

	if err := s.backend.NodeMount(ctx, &backend.MountRequest{
		VolumeID:      req.GetVolumeId(),
		TargetPath:    target,
		VolumeContext: req.GetVolumeContext(),
		Secrets:       req.GetSecrets(),
		Readonly:      req.GetReadonly(),
		MountFlags:    mountFlags,
	}); err != nil {
		return nil, err
	}

	klog.InfoS("volume published", "volumeID", req.GetVolumeId(), "target", target)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume 由 kubelet 在 pod 删除后调用。
func (s *Server) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	// 后端实现了 NodeUnmounter 可选接口则用自定义卸载，否则用框架默认实现。
	var unmountErr error
	if u, ok := s.backend.(backend.NodeUnmounter); ok {
		unmountErr = u.NodeUnmount(ctx, req.GetVolumeId(), target)
	} else {
		unmountErr = backend.DefaultNodeUnmount(s.mounter, target)
	}
	if unmountErr != nil {
		return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, unmountErr)
	}

	// 卸载后清理空目录，保持节点整洁。CleanupMountPoint 本身是幂等的。
	if err := mount.CleanupMountPoint(target, s.mounter, false); err != nil {
		return nil, status.Errorf(codes.Internal, "cleanup mount point: %v", err)
	}

	klog.InfoS("volume unpublished", "volumeID", req.GetVolumeId(), "target", target)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats 提供卷用量统计，支撑 kubectl top pod / 驱逐决策。
// 用 statfs 通用实现，文件类后端都适用，无需后端参与。
func (s *Server) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", path)
		}
		return nil, status.Errorf(codes.Internal, "statfs %s: %v", path, err)
	}

	total := int64(stat.Blocks) * int64(stat.Bsize)
	avail := int64(stat.Bavail) * int64(stat.Bsize)
	used := total - int64(stat.Bfree)*int64(stat.Bsize)
	inodesTotal := int64(stat.Files)
	inodesFree := int64(stat.Ffree)

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{Total: total, Available: avail, Used: used, Unit: csi.VolumeUsage_BYTES},
			{Total: inodesTotal, Available: inodesFree, Used: inodesTotal - inodesFree, Unit: csi.VolumeUsage_INODES},
		},
	}, nil
}

func (s *Server) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
	}, nil
}

// NodeGetInfo 被 node-driver-registrar 调用，返回值经 annotation
// csi.volume.kubernetes.io/nodeid 写到 Node 对象上。
// MaxVolumesPerNode=0 表示不限制。
func (s *Server) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
	}, nil
}
