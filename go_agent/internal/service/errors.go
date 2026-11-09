package service

// 领域错误 → 传输层错误的统一映射。
//
// 走 Kratos 官方注册后，handler 不再自己写 HTTP 状态码，而是返回 error：
//   - kratos 的 ErrorEncoder 按 code 输出 HTTP 状态码，body 为
//     {"code":404,"reason":"NOT_FOUND","message":"..."}；
//   - 同一份 error 经 gRPC 返回时会转成对应的 gRPC status，
//     HTTP 与 gRPC 两种调用方式的语义因此保持一致。
//
// 前端读 data.message（见 frontend/src/api/http.js），所以 message 必须放
// 面向用户的中文原文，reason 只做机器可读的分类。

import (
	"errors"

	kerrors "github.com/go-kratos/kratos/v3/errors"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/aiplatform"
	"go_agent/internal/biz/harness"
	"go_agent/internal/biz/llm"
	"go_agent/internal/biz/mcp"
	"go_agent/internal/biz/memory"
	"go_agent/internal/biz/prompt"
	"go_agent/internal/biz/skill"
)

// badRequest 参数/状态不合法（400）
func badRequest(msg string) error {
	return kerrors.BadRequest("INVALID_ARGUMENT", msg)
}

// notFound 资源不存在（404）
func notFound(msg string) error {
	return kerrors.New(404, "NOT_FOUND", msg)
}

// conflict 资源冲突（409）
func conflict(msg string) error {
	return conflictCode("ALREADY_EXISTS", msg)
}

// conflictCode 409 但用自定义错误码：前端据此区分「重名」与「状态已变」，
// 前者改个名字重试、后者必须刷新重新决策
func conflictCode(code, msg string) error {
	return kerrors.New(409, code, msg)
}

// upstreamErr 上游/三方系统故障（502）：平台不可达、鉴权失败、连接被拒
func upstreamErr(msg string) error {
	return kerrors.New(502, "BAD_GATEWAY", msg)
}

// forbidden 无权限（403）：身份可信但这个人不该做这件事
func forbidden(reason, msg string) error {
	return kerrors.New(403, reason, msg)
}

// internalErr 兜底（500）。已经是带 code 的 kratos 错误时原样返回，
// 避免把 400 包成 500。
func internalErr(err error) error {
	if err == nil {
		return nil
	}
	var ke *kerrors.Error
	if errors.As(err, &ke) {
		return ke
	}
	return kerrors.InternalServer("INTERNAL", err.Error())
}

// mapPromptErr 提示词域错误
func mapPromptErr(err error) error {
	var missing *prompt.MissingVarsError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, prompt.ErrNotFound), errors.Is(err, prompt.ErrVersionNotFound):
		return notFound(err.Error())
	case errors.Is(err, prompt.ErrExists):
		return conflict(err.Error())
	case errors.Is(err, prompt.ErrInvalidKey),
		errors.Is(err, prompt.ErrEmptyContent),
		errors.Is(err, prompt.ErrBuiltinProtected),
		errors.Is(err, prompt.ErrNoPrevious),
		errors.As(err, &missing):
		return badRequest(err.Error())
	case errors.Is(err, prompt.ErrStudioOutput):
		return upstreamErr(err.Error())
	default:
		return internalErr(err)
	}
}

// mapLLMErr 大模型数据源域错误
func mapLLMErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, llm.ErrProviderNotFound):
		return notFound(err.Error())
	case errors.Is(err, llm.ErrProviderExists):
		return conflict(err.Error())
	case errors.Is(err, llm.ErrInvalidProvider),
		errors.Is(err, llm.ErrNoProvider),
		errors.Is(err, llm.ErrLastProvider):
		return badRequest(err.Error())
	default:
		return internalErr(err)
	}
}

// mapSkillErr 技能域错误
func mapSkillErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, skill.ErrSkillNotFound), errors.Is(err, skill.ErrHubNotFound):
		return notFound(err.Error())
	case errors.Is(err, skill.ErrInvalidName),
		errors.Is(err, skill.ErrEmptyContent),
		errors.Is(err, skill.ErrInvalidFile),
		errors.Is(err, skill.ErrInvalidURL),
		errors.Is(err, skill.ErrHubResponse),
		errors.Is(err, skill.ErrNoHub):
		return badRequest(err.Error())
	default:
		return internalErr(err)
	}
}

// mapMemoryErr 记忆域错误：目前只有"不存在"需要区分状态码
func mapMemoryErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, memory.ErrNotFound):
		return notFound(err.Error())
	default:
		return internalErr(err)
	}
}

// mapMCPErr MCP 域错误
func mapMCPErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mcp.ErrServerNotFound), errors.Is(err, mcp.ErrToolNotFound):
		return notFound(err.Error())
	case errors.Is(err, mcp.ErrServerExists):
		return conflict(err.Error())
	case errors.Is(err, mcp.ErrInvalidServer),
		errors.Is(err, mcp.ErrServerDisabled),
		errors.Is(err, mcp.ErrToolDisabled),
		errors.Is(err, mcp.ErrNotConnected),
		errors.Is(err, mcp.ErrEmptyQuery):
		return badRequest(err.Error())
	default:
		// 连接失败、协议错误的原始信息对排障很重要，保留在 message 里
		return internalErr(err)
	}
}

// mapAgentFactoryErr AgentFactory 域错误
func mapAgentFactoryErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, agentfactory.ErrDefNotFound), errors.Is(err, agentfactory.ErrTaskNotFound):
		return notFound(err.Error())
	case errors.Is(err, agentfactory.ErrDefExists):
		return conflict(err.Error())
	case errors.Is(err, agentfactory.ErrTaskStateDeny):
		// 状态机 CAS 输了：不是参数错，是「你看到的旧状态已经不是现在这个」
		return conflictCode("TASK_STATE_CONFLICT", err.Error())
	case errors.Is(err, agentfactory.ErrApprovalSelfApprove):
		return forbidden("APPROVAL_SELF_APPROVE", err.Error())
	case errors.Is(err, agentfactory.ErrApprovalForbidden):
		return forbidden("APPROVAL_FORBIDDEN", err.Error())
	case errors.Is(err, agentfactory.ErrInvalidDef), errors.Is(err, agentfactory.ErrInvalidTask),
		errors.Is(err, agentfactory.ErrCapabilityDeny), errors.Is(err, agentfactory.ErrBootStrapDeny),
		errors.Is(err, agentfactory.ErrQuotaExceeded),
		errors.Is(err, agentfactory.ErrNoChatter), errors.Is(err, agentfactory.ErrSandboxDeny),
		errors.Is(err, agentfactory.ErrCapabilityUnset):
		return badRequest(err.Error())
	default:
		return internalErr(err)
	}
}

// mapHarnessErr 评测域错误映射
func mapHarnessErr(err error) error {
	switch {
	case errors.Is(err, harness.ErrSetNotFound),
		errors.Is(err, harness.ErrCaseNotFound),
		errors.Is(err, harness.ErrRunNotFound):
		return notFound(err.Error())
	case errors.Is(err, harness.ErrRunActive):
		return conflict(err.Error())
	case errors.Is(err, harness.ErrEmptySet),
		errors.Is(err, harness.ErrEmptyCase),
		errors.Is(err, harness.ErrInvalidCheck),
		errors.Is(err, harness.ErrInvalidTarget),
		errors.Is(err, harness.ErrNoCases):
		return badRequest(err.Error())
	case errors.Is(err, harness.ErrNoRunner):
		return upstreamErr(err.Error())
	default:
		return internalErr(err)
	}
}

// mapAIPlatformErr 外部 AI 平台域错误
//
// ErrImportFailed 表示「平台那边连不上 / 鉴权没过」，属于上游故障，
// 用 502 而不是 400，让调用方能区分「我参数传错了」和「平台不可用」。
func mapAIPlatformErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, aiplatform.ErrPlatformNotFound):
		return notFound(err.Error())
	case errors.Is(err, aiplatform.ErrPlatformExists):
		return conflict(err.Error())
	case errors.Is(err, aiplatform.ErrInvalidPlatform),
		errors.Is(err, aiplatform.ErrNoPlatform),
		errors.Is(err, aiplatform.ErrCatalogUnsupported):
		return badRequest(err.Error())
	case errors.Is(err, aiplatform.ErrImportFailed), errors.Is(err, aiplatform.ErrAuthFailed):
		return upstreamErr(err.Error())
	default:
		return internalErr(err)
	}
}
