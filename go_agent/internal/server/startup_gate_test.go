package server_test

import (
	"path/filepath"
	"testing"

	"go_agent/internal/conf"
	"go_agent/internal/server"
)

// 仓库自带的 configs/config.yaml 必须能在「不设任何环境变量」时启动。
//
// 这是一条真实的回归护栏：默认配置一度是 0.0.0.0 + AUTH_ENABLED=false，
// 每次 `kratos run` 都会被启动闸门拒绝，新人第一眼看到的就是「启动失败」。
// 现在默认监听回环，闸门只拦「真的对外暴露」的配置。
func TestDefaultConfigPassesStartupGate(t *testing.T) {
	// 清空可能存在的同名环境变量，复现「纯默认值」场景
	for _, k := range []string{"AUTH_ENABLED", "AUTH_DEV_INSECURE", "AUTH_JWT_SECRET", "HTTP_ADDR", "GRPC_ADDR"} {
		t.Setenv(k, "")
	}

	bc, cleanup, err := conf.Load(filepath.Join("..", "..", "configs"))
	if err != nil {
		t.Fatalf("加载仓库默认配置失败: %v", err)
	}
	defer cleanup()

	httpAddr := bc.GetServer().GetHttp().GetAddr()
	grpcAddr := bc.GetServer().GetGrpc().GetAddr()
	if err := server.CheckInsecureExposure(bc.GetSecurity(), httpAddr, grpcAddr); err != nil {
		t.Fatalf("默认配置应当直接启动（http=%s grpc=%s）: %v", httpAddr, grpcAddr, err)
	}
}
