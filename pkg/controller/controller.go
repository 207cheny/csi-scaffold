// Package controller 实现 CSI Controller 服务的通用框架。
//
// 框架负责：参数校验、并发串行化、CSI 能力声明、错误码规范；
// 业务逻辑（怎么建卷/删卷）全部委托给 backend.Backend。
// 对接新存储系统时本文件不需要改动。
package controller

import (
	"context"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/scaffold/csi-scaffold/pkg/backend"
)

type Server struct {
	csi.UnimplementedControllerServer
	backend backend.Backend
	// mu 把所有变更类调用串行化。后端实现简单时这是最省心的并发模型；
	// 后端自身保证幂等/并发安全时可以去掉。
	mu sync.Mutex
	// caps 根据后端实现的可选接口动态计算，避免声明了能力却返回
	// Unimplemented（这会让 sidecar 反复重试）。
	caps []csi.ControllerServiceCapability_RPC_Type
}

func NewServer(be backend.Backend) *Server {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	}
	if _, ok := be.(backend.Expander); ok {
		caps = append(caps, csi.ControllerServiceCapability_RPC_EXPAND_VOLUME)
	}
	return &Server{backend: be, caps: caps}
}

// CreateVolume 由 external-provisioner 在用户创建 PVC 时调用。
func (s *Server) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	capacity := req.GetCapacityRange().GetRequiredBytes()
	vol, err := s.backend.Provision(ctx, &backend.ProvisionRequest{
		Name:          req.GetName(),
		CapacityBytes: capacity,
		Parameters:    req.GetParameters(),
		Secrets:       req.GetSecrets(),
	})
	if err != nil {
		return nil, err
	}

	klog.InfoS("volume provisioned", "name", req.GetName(), "volumeID", vol.VolumeID)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      vol.VolumeID,
			CapacityBytes: vol.CapacityBytes,
			VolumeContext: vol.VolumeContext,
		},
	}, nil
}

// DeleteVolume 在 PV 回收策略为 Delete 且 PV 释放时由 provisioner 调用。
func (s *Server) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 契约：backend.Deprovision 对不存在的卷必须返回 nil。
	if err := s.backend.Deprovision(ctx, req.GetVolumeId(), req.GetSecrets()); err != nil {
		return nil, err
	}
	klog.InfoS("volume deleted", "volumeID", req.GetVolumeId())
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerExpandVolume 由 external-resizer 调用。后端未实现 Expander
// 接口时不会声明该能力，正常不会被调用；兜底返回 Unimplemented。
func (s *Server) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	exp, ok := s.backend.(backend.Expander)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "backend does not support expansion")
	}
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	size, err := exp.Expand(ctx, req.GetVolumeId(), req.GetCapacityRange().GetRequiredBytes(), req.GetSecrets())
	if err != nil {
		return nil, err
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         size,
		NodeExpansionRequired: false, // 文件存储不需要 node 侧二次扩容
	}, nil
}

// ValidateVolumeCapabilities 被 provisioner 用于静态供应场景的校验。
func (s *Server) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	// 若后端支持卷校验，则顺带确认卷真实存在。
	if v, ok := s.backend.(backend.VolumeValidator); ok {
		if err := v.ValidateVolume(ctx, req.GetVolumeId(), req.GetSecrets()); err != nil {
			return nil, err
		}
	}

	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: err.Error(),
		}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

func (s *Server) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := make([]*csi.ControllerServiceCapability, 0, len(s.caps))
	for _, c := range s.caps {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// validateVolumeCapabilities 只接受「文件系统挂载 + 单点读写」，
// 这是 nfs 类文件存储的标准能力集。块设备类后端应放宽这里的校验。
func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	for _, cap := range caps {
		if cap.GetMount() == nil {
			return status.Error(codes.InvalidArgument, "only mount volumes are supported")
		}
		switch cap.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
			csi.VolumeCapability_AccessMode_UNKNOWN:
		default:
			return status.Errorf(codes.InvalidArgument, "unsupported access mode: %v", cap.GetAccessMode().GetMode())
		}
	}
	return nil
}
