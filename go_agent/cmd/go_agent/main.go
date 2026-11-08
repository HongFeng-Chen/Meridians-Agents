package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/audit"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/egress"
	"go_agent/internal/pkg/identity"
	"go_agent/internal/server"
	"go_agent/internal/service"

	"github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/log"
	"github.com/go-kratos/kratos/v3/transport/grpc"
	"github.com/go-kratos/kratos/v3/transport/http"

	_ "go.uber.org/automaxprocs"
)

// go build -ldflags "-X main.Version=x.y.z"
var (
	// Name is the name of the compiled software.
	Name string
	// Version is the version of the compiled software.
	Version string
	// flagconf is the config flag.
	flagconf string

	id, _ = os.Hostname()
)

func init() {
	flag.StringVar(&flagconf, "conf", "../../configs", "config path, eg: -conf config.yaml")
}

// newApp 组装 kratos 应用；同时托管 AgentFactory worker 池生命周期：
// 应用启动后启动 worker（消费 Redis 任务队列 + 崩溃恢复），停止时优雅退出。
// 放在这里而不是手写进 wire_gen，保证 go generate 重新生成后依然生效。
//
// 另外在 AfterStart 里自注册平台内置 MCP 服务（/mcp/platform）并同步工具：
// 必须等 HTTP 端口起来后再连接（自调用），所以放后台协程 + 有限重试。
func newApp(logger *slog.Logger, gs *grpc.Server, hs *http.Server, afUC *agentfactory.UseCase, platformMCP *service.PlatformMCPService) *kratos.App {
	return kratos.New(
		kratos.ID(id),
		kratos.Name(Name),
		kratos.Version(Version),
		kratos.Metadata(map[string]string{}),
		kratos.Logger(logger),
		kratos.Server(
			gs,
			hs,
		),
		kratos.BeforeStart(func(ctx context.Context) error {
			return afUC.StartWorkers()
		}),
		kratos.AfterStart(func(ctx context.Context) error {
			if platformMCP == nil {
				return nil
			}
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				for attempt := 1; attempt <= 10; attempt++ {
					if err := platformMCP.EnsureServer(bg); err != nil {
						log.Warn("平台 MCP 服务注册/同步失败，稍后重试", "attempt", attempt, "err", err)
						select {
						case <-time.After(2 * time.Second):
							continue
						case <-bg.Done():
							return
						}
					}
					log.Info("平台 MCP 服务已就绪", "endpoint", platformMCP.Endpoint())
					return
				}
			}()
			return nil
		}),
		kratos.BeforeStop(func(ctx context.Context) error {
			// 优雅停机：先停止领取新任务，等执行中的任务跑完（最长
			// agentfactory.shutdownTimeout）。此时 HTTP 还没停，任务状态仍可查。
			return afUC.Drain(ctx)
		}),
		kratos.AfterStop(func(ctx context.Context) error {
			// 兜底硬停：Drain 已完成时立即返回；超时场景负责取消并回收 worker
			afUC.Stop()
			// 审计是异步批量写的，把缓冲区里最近 2s 的事件落库再退出
			if uc := audit.Default(); uc != nil {
				uc.Close()
			}
			return nil
		}),
	)
}

func main() {
	flag.Parse()
	logger := log.NewLogger(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelInfo,
		}),
		log.WithExtractor(tracing.TraceAttrs),
	).With(
		slog.String("service.id", id),
		slog.String("service.name", Name),
		slog.String("service.version", Version),
	)
	log.SetDefault(logger)

	// 配置文件支持 ${ENV:default} 占位符（Kratos 自带 file source 不展开环境变量）
	bc, cleanupConf, err := conf.Load(flagconf)
	if err != nil {
		fatal("加载配置", err)
	}
	defer cleanupConf()

	logStartupInfo(bc)

	// 启动闸门：认证关闭 + 对外监听时拒绝启动（见 server.CheckInsecureExposure）
	if err := server.CheckInsecureExposure(bc.GetSecurity(),
		bc.GetServer().GetHttp().GetAddr(), bc.GetServer().GetGrpc().GetAddr()); err != nil {
		fatal("安全检查", err)
	}

	app, cleanup, err := wireApp(bc.GetServer(), bc.GetData(), bc.GetSecurity(), logger)
	if err != nil {
		fatal("初始化依赖", err)
	}
	defer cleanup()

	if err := app.Run(); err != nil {
		fatal("运行服务", err)
	}
}

// fatal 打印可读的失败原因后退出。
// 用 panic 会把真正的原因埋在几十行 goroutine 堆栈里，这里只输出结论和排查方向。
func fatal(stage string, err error) {
	log.Error("启动失败", "stage", stage, "err", err)
	fmt.Fprintf(os.Stderr, "\n启动失败（%s）: %v\n排查：docker compose ps 确认 mysql/redis/milvus 已 healthy；"+
		"配置在 configs/config.yaml，支持 ${ENV:default} 覆盖。\n", stage, err)
	os.Exit(1)
}

// logStartupInfo 启动前打印关键依赖（密钥脱敏），便于排查“卡在哪一步”
func logStartupInfo(bc *conf.Bootstrap) {
	d := bc.GetData()
	log.Info("bootstrap",
		"http", bc.GetServer().GetHttp().GetAddr(),
		"grpc", bc.GetServer().GetGrpc().GetAddr(),
		"mysql", maskDSN(d.GetDatabase().GetSource()),
		"redis", d.GetRedis().GetAddr(),
		"redis_auth", hasAuth(d.GetRedis().GetPassword()),
		"milvus", d.GetMilvus().GetUri(),
		"collection", d.GetMilvus().GetCollection(),
		"embed", d.GetEmbedding().GetModel(),
		"embed_dim", d.GetEmbedding().GetDimension(),
		"embed_key", maskKey(d.GetEmbedding().GetApiKey()),
		"llm", d.GetLlm().GetModel(),
		"llm_key", maskKey(d.GetLlm().GetApiKey()),
	)
	sec := bc.GetSecurity()
	log.Info("security",
		"auth_enabled", sec.GetAuth().GetEnabled(),
		"public_paths", len(sec.GetAuth().GetPublicPaths()),
		"static_tokens", len(sec.GetAuth().GetStaticTokens()),
		"ratelimit_enabled", sec.GetRateLimit().GetEnabled(),
		"ratelimit_qps", sec.GetRateLimit().GetQps(),
		"budget_tokens", sec.GetBudget().GetMaxTokens(),
		"budget_parallel", sec.GetBudget().GetMaxParallel(),
	)
	if !sec.GetAuth().GetEnabled() {
		log.Warn("认证未启用：任何能访问本端口的人都可以读写全部会话/记忆/Agent 定义并消耗你的 token。" +
			"对外暴露前请设置 AUTH_ENABLED=true AUTH_JWT_SECRET=<密钥>")
	}
	if sec.GetAuth().GetEnabled() && sec.GetAuth().GetJwtSecret() == "" && len(sec.GetAuth().GetStaticTokens()) == 0 {
		log.Error("认证已启用但没有可用凭证：jwtSecret 与 staticTokens 都为空，所有请求都会被拒绝")
	}

	// 安全策略是环境变量驱动的（不走 config.yaml），启动时打一遍，避免「以为配了」
	ep := egress.CurrentPolicy()
	ap := mcp.CurrentApprovalPolicy()
	threshold := string(ap.Threshold)
	if threshold == "" {
		threshold = "none"
	}
	log.Info("security policies",
		"egress_allowed_hosts", len(ep.AllowedHosts),
		"egress_allow_private", ep.AllowPrivate,
		"egress_allowed_ports", len(ep.AllowedPorts),
		"egress_max_redirects", ep.MaxRedirects,
		"tool_approval_level", threshold,
		"tool_approval_ttl", ap.TTL.String(),
		"mcp_stdio_enabled", mcp.CurrentStdioPolicy().Enabled,
		"dev_trust_all", identity.DevTrustAll(),
	)

	if d.GetLlm().GetApiKey() == "" {
		log.Warn("LLM_API_KEY 未配置：问答与图谱抽取会失败（健康检查仍可用）")
	}
}

// maskDSN 隐去 DSN 里的密码，避免启动日志泄露凭据
func maskDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	head := dsn[:at]
	if colon := strings.Index(head, ":"); colon >= 0 {
		head = head[:colon] + ":***"
	}
	return head + dsn[at:]
}

func hasAuth(password string) string {
	if password == "" {
		return "无密码"
	}
	return "已配置"
}

// maskKey 脱敏显示密钥：仅保留头尾各 4 位
func maskKey(k string) string {
	switch {
	case k == "":
		return "未设置"
	case len(k) <= 10:
		return "已设置"
	default:
		return k[:4] + "***" + k[len(k)-4:]
	}
}
