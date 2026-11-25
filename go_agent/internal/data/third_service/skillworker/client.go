// Package skillworker Skill Worker HTTP 客户端。
//
// 协议只有一个端点，语言无关：
//
//	POST {endpoint}/execute
//	Content-Type: application/json
//	Authorization: Bearer <SKILL_WORKER_TOKEN>   （可选，服务端配置了才校验）
//
//	请求体 = SkillRequest：{task_id, execution_id, skill_name, skill_version,
//	                        tenant_id, user_id, agent_key, session_id,
//	                        idempotency_key, timeout_ms, trace_id, input}
//	响应体 = SkillResponse：{success, output, error, worker_id, latency_ms}
//
// 之所以不直接复用 MCP：MCP 的语义是「工具发现 + 调用」，面向 LLM；
// Skill Worker 面向平台内部调度（租户上下文、幂等键、超时预算都要显式传递），
// 混在一套协议里会让两边的演进互相牵制。
package skillworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go_agent/internal/biz/skill"
	"go_agent/internal/conf"
	"go_agent/internal/pkg/egress"
)

// maxResponseSize Worker 回包上限：结果可以是文档摘要/结构化数据，但不该是文件本体
const maxResponseSize = 8 << 20

// Client Worker 客户端
type Client struct {
	http  *http.Client
	token string
}

var _ skill.WorkerClient = (*Client)(nil)

// New 创建客户端。
//
// http.Client 的超时只做兜底（15 分钟）：真正的超时预算来自调用方 context
// （技能配置的 timeout_sec），长任务不该被传输层提前掐断。
func New(c *conf.Data) *Client {
	timeout := 15 * time.Minute
	if c != nil {
		if t := c.GetSkill().GetHttpTimeout().AsDuration(); t > 0 && t < timeout {
			timeout = t
		}
	}
	return &Client{
		http:  egress.Client(timeout, egress.Options{}),
		token: strings.TrimSpace(os.Getenv("SKILL_WORKER_TOKEN")),
	}
}

// Execute 调用一次 Worker
func (c *Client) Execute(ctx context.Context, inst skill.Instance, req skill.WorkerRequest) (*skill.WorkerResponse, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(inst.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("%w: 实例 %s 没有 endpoint", skill.ErrNoWorker, inst.ID)
	}
	// 出站闸门：技能配置里的地址是管理面填的，但实例注册也可能来自外部 Worker，
	// 统一按出站策略校验（内网地址需显式 allowlist）。
	if err := egress.ValidateURL(endpoint); err != nil {
		return nil, fmt.Errorf("%w: endpoint %q（%v）", skill.ErrInvalidURL, endpoint, err)
	}
	url := endpoint
	if !strings.HasSuffix(url, "/execute") {
		url += "/execute"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化技能请求失败: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造技能请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Skill-Name", req.SkillName)
	httpReq.Header.Set("X-Skill-Task-ID", req.TaskID)
	httpReq.Header.Set("X-Skill-Execution-ID", req.ExecutionID)
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("调用技能 Worker 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("读取技能响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("技能 Worker 返回 HTTP %d：%s", resp.StatusCode, snippet(raw))
	}
	out := &skill.WorkerResponse{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("技能 Worker 响应不是合法 JSON（%v）：%s", err, snippet(raw))
	}
	// 兼容「直接返回业务 JSON」的 Worker：没有 success 字段就当成成功结果整体包一层。
	// 要求每个 Worker 都实现信封会把接入成本推给业务方——先能跑通，再补字段。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		if _, hasSuccess := probe["success"]; !hasSuccess {
			generic := map[string]any{}
			_ = json.Unmarshal(raw, &generic)
			return &skill.WorkerResponse{
				TaskID:      req.TaskID,
				ExecutionID: req.ExecutionID,
				Success:     true,
				Output:      generic,
				WorkerID:    inst.ID,
			}, nil
		}
	}
	if out.ExecutionID == "" {
		out.ExecutionID = req.ExecutionID
	}
	if out.TaskID == "" {
		out.TaskID = req.TaskID
	}
	return out, nil
}

// snippet 错误响应片段（截断，避免把整页 HTML 打进日志或返回给 Agent）
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
