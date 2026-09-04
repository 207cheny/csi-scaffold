package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

// NonBlockingGRPCServer 复用 hostpath 官方实现模式：
// Start 立即返回，真正的 Serve 在后台 goroutine 中执行，
// 便于 main 中统一的信号处理与优雅退出。
type NonBlockingGRPCServer struct {
	wg     sync.WaitGroup
	server *grpc.Server
}

func NewNonBlockingGRPCServer() *NonBlockingGRPCServer {
	return &NonBlockingGRPCServer{}
}

// Start 在同一个 socket 上注册 identity / controller / node 三个服务。
// CSI 允许一个 socket 承载多个服务，sidecar 通过服务名区分调用。
func (s *NonBlockingGRPCServer) Start(endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, ns csi.NodeServer) {
	s.wg.Add(1)
	go s.serve(endpoint, ids, cs, ns)
}

func (s *NonBlockingGRPCServer) GracefulStop() {
	if s.server != nil {
		s.server.GracefulStop()
	}
}

func (s *NonBlockingGRPCServer) Wait() {
	s.wg.Wait()
}

func (s *NonBlockingGRPCServer) serve(endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, ns csi.NodeServer) {
	defer s.wg.Done()

	proto, addr, err := parseEndpoint(endpoint)
	if err != nil {
		klog.ErrorS(err, "failed to parse endpoint", "endpoint", endpoint)
		os.Exit(1)
	}
	if proto == "unix" {
		// 清理残留 socket 文件，否则容器重启后 Listen 会失败。
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			klog.ErrorS(err, "failed to remove stale socket", "addr", addr)
			os.Exit(1)
		}
	}

	listener, err := net.Listen(proto, addr)
	if err != nil {
		klog.ErrorS(err, "failed to listen", "addr", addr)
		os.Exit(1)
	}

	opts := []grpc.ServerOption{grpc.UnaryInterceptor(logGRPC)}
	server := grpc.NewServer(opts...)
	s.server = server

	if ids != nil {
		csi.RegisterIdentityServer(server, ids)
	}
	if cs != nil {
		csi.RegisterControllerServer(server, cs)
	}
	if ns != nil {
		csi.RegisterNodeServer(server, ns)
	}

	klog.InfoS("listening for connections", "addr", listener.Addr())
	if err := server.Serve(listener); err != nil {
		klog.ErrorS(err, "grpc server stopped with error")
	}
}

// logGRPC 统一打印每个 CSI 调用的请求/响应/错误，排障时极有价值。
// 注意脱敏：Secrets 字段在 csi 请求 proto 中被标记为 secret，protoc-gen-go
// 生成的 String() 会自动打码，这里直接打印是安全的。
func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(4).InfoS("GRPC call", "method", info.FullMethod)
	klog.V(5).InfoS("GRPC request", "method", info.FullMethod, "request", req)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.ErrorS(err, "GRPC error", "method", info.FullMethod)
	} else {
		klog.V(5).InfoS("GRPC response", "method", info.FullMethod, "response", resp)
	}
	return resp, err
}

func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("invalid endpoint: %v", ep)
}

func validateEndpoint(ep string) error {
	_, _, err := parseEndpoint(ep)
	return err
}
