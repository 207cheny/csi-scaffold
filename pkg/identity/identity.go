// Package identity 实现 CSI Identity 服务，通用骨架，对接新后端无需改动。
// 作用：向 k8s sidecar（registrar/provisioner 等）自报家门 ——
// 我是谁（GetPluginInfo）、我活着吗（Probe，livenessprobe 依赖它）、
// 我能干什么（GetPluginCapabilities）。
package identity

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/scaffold/csi-scaffold/pkg/backend"
)

type Server struct {
	csi.UnimplementedIdentityServer
	name    string
	version string
	backend backend.Backend
}

func NewServer(name, version string, be backend.Backend) *Server {
	return &Server{name: name, version: version, backend: be}
}

// GetPluginInfo 返回插件名与版本。插件名必须与 CSIDriver 对象、
// StorageClass.provisioner 完全一致，否则 provisioner 不会认领 PVC。
func (s *Server) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if s.name == "" {
		return nil, status.Error(codes.Unavailable, "driver name not configured")
	}
	return &csi.GetPluginInfoResponse{
		Name:          s.name,
		VendorVersion: s.version,
	}, nil
}

// Probe 被 livenessprobe sidecar 周期性调用，返回 Ready=true 表示健康。
func (s *Server) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{
		Ready: &wrapperspb.BoolValue{Value: true},
	}, nil
}

// GetPluginCapabilities 声明插件级能力：
//   - CONTROLLER_SERVICE：本插件提供 controller 服务（提供应/删除）；
//   - VOLUME_ACCESSIBILITY_CONSTRAINTS：支持拓扑感知（有 statefulset
//     时 provisioner 才会传 AccessibilityRequirements）。
func (s *Server) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
					},
				},
			},
		},
	}, nil
}
