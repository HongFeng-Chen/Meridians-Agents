package service

import "github.com/google/wire"

// ProviderSet is service providers.
var ProviderSet = wire.NewSet(
	NewRagService,
	NewPromptService,
	NewMemoryService,
	NewLLMService,
	NewSkillService,
	NewMCPService,
	NewAgentFactoryService,
	NewLLMCallQueryService,
	NewHistoryService,
	NewConversationService,
	NewPlatformMCPService,
	NewAIPlatformService,
	NewHarnessService,
	NewAdminService,
	NewAuthService,
	NewPolicyService,
)
