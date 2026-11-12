// client.go 视频生成客户端入口：按数据源 kind 路由到对应厂商协议实现。
//
// 各厂商协议见同目录的 siliconflow.go / ark.go / openai.go。
// 状态归一：厂商的排队/生成中状态统一映射成 queued / running，
// 成功时把成品转存本地并返回 /api/files/ 地址；失败时把厂商原因放在 output 位返回。
package video_generations

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
)

// VideoClient 视频生成客户端
type VideoClient struct {
	http *http.Client
}

// NewVideoClient 创建客户端
func NewVideoClient() *VideoClient {
	return &VideoClient{http: &http.Client{}}
}

var _ llm.VideoGenClient = (*VideoClient)(nil)

// Submit 提交任务，返回厂商任务 ID
func (c *VideoClient) Submit(ctx context.Context, t llm.MediaTarget, in *llm.VideoGenInput) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	base := strings.TrimRight(t.BaseURL, "/")

	switch protocolOf(t.Kind) {
	case protoArk:
		return c.submitArk(ctx, base, t.APIKey, t.Model, in)
	case protoOpenAI:
		return c.submitOpenAI(ctx, base, t.APIKey, t.Model, in)
	default: // siliconflow 及未识别 kind 的默认
		return c.submitSiliconFlow(ctx, base, t.APIKey, t.Model, in)
	}
}

// Poll 查询任务状态。返回 (status, output, err)：
//   - succeeded：output = 已落盘的本地地址
//   - failed：  output = 失败原因
//   - 其它：    output = 空，err = nil（仍在生成中）
func (c *VideoClient) Poll(ctx context.Context, t llm.MediaTarget, taskID string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	base := strings.TrimRight(t.BaseURL, "/")

	switch protocolOf(t.Kind) {
	case protoArk:
		return c.pollArk(ctx, base, t.APIKey, taskID)
	case protoOpenAI:
		return c.pollOpenAI(ctx, base, t.APIKey, t.Model, taskID)
	default:
		return c.pollSiliconFlow(ctx, base, t.APIKey, taskID)
	}
}

// —— 协议选择 ——

type videoProto string

const (
	protoSiliconFlow videoProto = "siliconflow"
	protoArk         videoProto = "ark"
	protoOpenAI      videoProto = "openai"
)

// protocolOf 按数据源 kind 选协议。火山引擎的 base_url 常见
// https://ark.cn-beijing.volces.com/api/v3，kind 可能为 custom；
// 用 host 兜底识别，识别不出统一走 SiliconFlow 协议（国内视频厂商的事实标准）。
func protocolOf(kind string) videoProto {
	switch kind {
	case llm.KindOpenAI:
		return protoOpenAI
	case "ark", "volcengine", "doubao":
		return protoArk
	}
	return protoSiliconFlow
}
