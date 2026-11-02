package server

import (
	agentfactoryv1 "go_agent/api/agentfactory/v1"
	aiplatformv1 "go_agent/api/aiplatform/v1"
	harnessv1 "go_agent/api/harness/v1"
	historyv1 "go_agent/api/history/v1"
	llmv1 "go_agent/api/llm/v1"
	mcpv1 "go_agent/api/mcp/v1"
	memoryv1 "go_agent/api/memory/v1"
	promptv1 "go_agent/api/prompt/v1"
	ragv1 "go_agent/api/rag/v1"
	skillv1 "go_agent/api/skill/v1"
	"go_agent/internal/conf"
	"go_agent/internal/service"

	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"github.com/go-kratos/kratos/v3/middleware/validate"
	"github.com/go-kratos/kratos/v3/transport/grpc"

	"go.einride.tech/aip/fieldbehavior"
	"google.golang.org/protobuf/proto"
)

// NewGRPCServer new a gRPC server.
//
// 与 HTTP 共用同一份 service 实现：proto 生成的方法集既满足
// XxxServiceHTTPServer，也满足 XxxServiceServer，因此两种协议的行为、
// 校验、错误码天然一致。
func NewGRPCServer(
	c *conf.Server,
	ragSvc *service.RagService,
	promptSvc *service.PromptService,
	memorySvc *service.MemoryService,
	llmSvc *service.LLMService,
	skillSvc *service.SkillService,
	mcpSvc *service.MCPService,
	afSvc *service.AgentFactoryService,
	llmCallSvc *service.LLMCallQueryService,
	hisSvc *service.HistoryService,
	aiPlatformSvc *service.AIPlatformService,
	harnessSvc *service.HarnessService,
) *grpc.Server {
	var opts = []grpc.ServerOption{
		grpc.Middleware(
			recovery.Recovery(),
			validate.Validator(func(req any) error {
				if msg, ok := req.(proto.Message); ok {
					if err := fieldbehavior.ValidateRequiredFields(msg); err != nil {
						return err
					}
				}
				return nil
			}),
		),
	}
	if c.Grpc.Network != "" {
		opts = append(opts, grpc.Network(c.Grpc.Network))
	}
	if c.Grpc.Addr != "" {
		opts = append(opts, grpc.Address(c.Grpc.Addr))
	}
	if c.Grpc.Timeout != nil {
		opts = append(opts, grpc.Timeout(c.Grpc.Timeout.AsDuration()))
	}
	srv := grpc.NewServer(opts...)

	ragv1.RegisterRagServiceServer(srv, ragSvc)
	promptv1.RegisterPromptServiceServer(srv, promptSvc)
	memoryv1.RegisterMemoryServiceServer(srv, memorySvc)
	llmv1.RegisterLLMServiceServer(srv, llmSvc)
	mcpv1.RegisterMCPServiceServer(srv, mcpSvc)
	skillv1.RegisterSkillServiceServer(srv, skillSvc)
	agentfactoryv1.RegisterAgentFactoryServiceServer(srv, afSvc)
	llmv1.RegisterLLMCallQueryServiceServer(srv, llmCallSvc)
	historyv1.RegisterHistoryServiceServer(srv, hisSvc)
	aiplatformv1.RegisterAIPlatformServiceServer(srv, aiPlatformSvc)
	harnessv1.RegisterHarnessServiceServer(srv, harnessSvc)
	return srv
}
