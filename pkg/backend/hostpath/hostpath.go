// Package hostpath 是最简单的本地目录后端，主要用于：
//  1. 验证脚手架骨架开箱可用（无需真实存储系统即可跑通全流程）；
//  2. 作为「实现一个新后端」的最小参照样本。
//
// 仅支持单节点测试，不具备生产意义。
package hostpath

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"

	"github.com/scaffold/csi-scaffold/pkg/backend"
)

const backendName = "hostpath"

type hostpathBackend struct {
	mounter mount.Interface
	dataDir string
}

func init() {
	backend.Register(backendName, New)
}

// New 实现 backend.Factory。cfg：{"dataDir":"/var/lib/csi-scaffold/hostpath"}
func New(cfg map[string]string) (backend.Backend, error) {
	dataDir := cfg["dataDir"]
	if dataDir == "" {
		dataDir = "/var/lib/csi-scaffold/hostpath"
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &hostpathBackend{mounter: mount.New(""), dataDir: dataDir}, nil
}

func (b *hostpathBackend) Name() string { return backendName }

func (b *hostpathBackend) Provision(_ context.Context, req *backend.ProvisionRequest) (*backend.Volume, error) {
	if err := sanitizeName(req.Name); err != nil {
		return nil, err
	}
	dir := filepath.Join(b.dataDir, req.Name)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, status.Errorf(codes.Internal, "create volume dir: %v", err)
	}
	return &backend.Volume{
		VolumeID:      req.Name,
		CapacityBytes: req.CapacityBytes,
		VolumeContext: map[string]string{"dir": dir},
	}, nil
}

func (b *hostpathBackend) Deprovision(_ context.Context, volumeID string, _ map[string]string) error {
	if err := sanitizeName(volumeID); err != nil {
		return err
	}
	// RemoveAll 对不存在的路径返回 nil，天然满足幂等契约。
	return os.RemoveAll(filepath.Join(b.dataDir, volumeID))
}

func (b *hostpathBackend) NodeMount(_ context.Context, req *backend.MountRequest) error {
	source := req.VolumeContext["dir"]
	if source == "" {
		return status.Errorf(codes.InvalidArgument, "volume context missing dir for volume %s", req.VolumeID)
	}

	options := []string{"bind"}
	if req.Readonly {
		options = append(options, "ro")
	}
	if err := b.mounter.Mount(source, req.TargetPath, "", options); err != nil {
		return status.Errorf(codes.Internal, "bind mount %s to %s: %v", source, req.TargetPath, err)
	}
	return nil
}

// sanitizeName 防止路径穿越：卷名/卷 ID 只能是不含路径分隔符的单段名字。
func sanitizeName(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return status.Errorf(codes.InvalidArgument, "invalid volume name/id: %q", name)
	}
	return nil
}
