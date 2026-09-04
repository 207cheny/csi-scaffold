// csi-scaffold 入口：组装 identity / controller / node 三个 CSI 服务。
// 对接新存储系统时，本文件不需要改动 —— 只需实现 pkg/backend 接口并在
// pkg/backend/registry.go 中注册即可。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/scaffold/csi-scaffold/pkg/backend"
	_ "github.com/scaffold/csi-scaffold/pkg/backend/hostpath" // 注册 hostpath 后端
	_ "github.com/scaffold/csi-scaffold/pkg/backend/nfs"      // 注册 nfs 后端
	"github.com/scaffold/csi-scaffold/pkg/driver"
)

var version = "dev" // 构建期注入：-X main.version=v1.0.0

func main() {
	klog.InitFlags(nil)

	cfg := driver.NewConfig()
	cfg.Version = version
	cfg.AddFlags(flag.CommandLine)
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		klog.ErrorS(err, "invalid config")
		os.Exit(1)
	}

	// 通过注册表创建业务后端，这是唯一切换存储系统的地方。
	be, err := backend.New(cfg.BackendName, cfg.BackendConfigFile)
	if err != nil {
		klog.ErrorS(err, "failed to create backend", "backend", cfg.BackendName)
		os.Exit(1)
	}

	d, err := driver.New(cfg, be)
	if err != nil {
		klog.ErrorS(err, "failed to create driver")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()

	klog.InfoS("starting csi driver",
		"driverName", cfg.DriverName,
		"version", version,
		"backend", be.Name(),
		"endpoint", cfg.Endpoint,
		"nodeID", cfg.NodeID)

	if err := d.Run(ctx); err != nil {
		klog.ErrorS(err, "driver exited with error")
		os.Exit(1)
	}
}
