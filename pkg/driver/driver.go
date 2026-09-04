package driver

import (
	"context"

	"github.com/scaffold/csi-scaffold/pkg/backend"
	"github.com/scaffold/csi-scaffold/pkg/controller"
	"github.com/scaffold/csi-scaffold/pkg/identity"
	"github.com/scaffold/csi-scaffold/pkg/node"
)

// Driver 把三个 CSI 服务组装到一个进程里。
// 生产级 driver 也常拆成 controller / node 两个 Deployment/DaemonSet 各自
// 只启动对应服务，脚手架用单进程即可（通过部署清单控制容器启动参数）。
type Driver struct {
	cfg    *Config
	ids    *identity.Server
	cs     *controller.Server
	ns     *node.Server
}

func New(cfg *Config, be backend.Backend) (*Driver, error) {
	return &Driver{
		cfg: cfg,
		ids: identity.NewServer(cfg.DriverName, cfg.Version, be),
		cs:  controller.NewServer(be),
		ns:  node.NewServer(cfg.NodeID, be),
	}, nil
}

// Run 阻塞直到 ctx 取消，然后优雅停止 gRPC server。
func (d *Driver) Run(ctx context.Context) error {
	s := NewNonBlockingGRPCServer()
	s.Start(d.cfg.Endpoint, d.ids, d.cs, d.ns)

	<-ctx.Done()
	s.GracefulStop()
	s.Wait()
	return nil
}
