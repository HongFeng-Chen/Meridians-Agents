//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"log/slog"

	"go_agent/internal/biz"
	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/aiplatform"
	"go_agent/internal/biz/conversation"
	"go_agent/internal/biz/harness"
	"go_agent/internal/biz/history"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/platformmcp"
	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
	"go_agent/internal/data"
	repo "go_agent/internal/data/db/repo"
	"go_agent/internal/server"
	"go_agent/internal/service"

	"github.com/go-kratos/kratos/v3"
	"github.com/google/wire"
)

// wireApp init kratos application.
func wireApp(*conf.Server, *conf.Data, *conf.Security, *slog.Logger) (*kratos.App, func(), error) {
	panic(wire.Build(
		server.ProviderSet, data.ProviderSet, biz.ProviderSet, service.ProviderSet,
		// 跨层接口绑定：实现由 data 层构造，接口声明在 biz，只能在合并后的图里绑定
		wire.Bind(new(aiplatform.MCPRegistry), new(*mcp.UseCase)),
		wire.Bind(new(aiplatform.SkillMarkets), new(*skill.UseCase)),
		wire.Bind(new(agentfactory.SkillPort), new(*skill.UseCase)),
		wire.Bind(new(platformmcp.TaskSource), new(*agentfactory.UseCase)),
		wire.Bind(new(platformmcp.ConvSource), new(*conversation.UseCase)),
		wire.Bind(new(platformmcp.HistorySource), new(*history.UseCase)),
		wire.Bind(new(harness.ScoreRecorder), new(*prompt.UseCase)),
		// 策略管理接口的存储实现指向 Casbin 引擎
		wire.Bind(new(service.PolicyStore), new(*repo.CasbinEngine)),
		newApp,
	))
}
