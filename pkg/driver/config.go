package driver

import (
	"errors"
	"flag"
	"fmt"
)

// Config 是驱动运行所需的全部静态配置，全部通过命令行参数注入。
type Config struct {
	// DriverName 是 CSI 插件名，必须全局唯一，形如 myfs.csi.example.com。
	// 它会出现在 CSIDriver 对象、StorageClass.provisioner 字段中。
	DriverName string
	// Endpoint 是 CSI gRPC socket 地址，集群内固定为 unix:///csi/csi.sock。
	Endpoint string
	// NodeID 是节点标识，通常用 spec.nodeName 注入。
	NodeID string
	// BackendName 选择对接的存储后端，对应 backend.Register 的名字。
	BackendName string
	// BackendConfigFile 是后端专属配置文件（JSON），内容原样透传给后端工厂。
	BackendConfigFile string
	// Version 由构建期注入，用于 GetPluginInfo 返回 VendorVersion。
	Version string
}

func NewConfig() *Config {
	return &Config{}
}

func (c *Config) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.DriverName, "drivername", "scaffold.csi.example.com", "name of the CSI driver")
	fs.StringVar(&c.Endpoint, "endpoint", "unix:///tmp/csi.sock", "CSI endpoint")
	fs.StringVar(&c.NodeID, "nodeid", "", "node id, required")
	fs.StringVar(&c.BackendName, "backend", "hostpath", "storage backend name (registered in pkg/backend)")
	fs.StringVar(&c.BackendConfigFile, "backend-config", "", "backend specific config file (JSON)")
}

func (c *Config) Validate() error {
	if c.DriverName == "" {
		return errors.New("drivername is required")
	}
	if c.Endpoint == "" {
		return errors.New("endpoint is required")
	}
	if c.NodeID == "" {
		return errors.New("nodeid is required")
	}
	if c.BackendName == "" {
		return errors.New("backend is required")
	}
	if err := validateEndpoint(c.Endpoint); err != nil {
		return fmt.Errorf("invalid endpoint: %w", err)
	}
	return nil
}
