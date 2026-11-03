package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go_agent/internal/conf"
)

// Target 一次调用的目标数据源快照
type Target struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
}

// Client LLM 客户端（OpenAI 兼容的 /chat/completions）
type Client struct {
	baseURL   string
	apiKey    string
	model     string
	temp      float64
	maxTokens int
	http      *http.Client
}

// New 从全局配置构建兜底 LLM 客户端
func New(c *conf.Data) *Client {
	return NewClient(c.GetLlm())
}

// NewClient 创建 LLM 客户端；温度取自配置
func NewClient(cfg *conf.Data_LLM) *Client {
	timeout := 120 * time.Second
	if cfg.GetHttpTimeout() != nil && cfg.GetHttpTimeout().AsDuration() > 0 {
		timeout = cfg.GetHttpTimeout().AsDuration()
	}
	return NewTarget(Target{
		BaseURL:     cfg.GetBaseURL(),
		APIKey:      cfg.GetApiKey(),
		Model:       cfg.GetModel(),
		Temperature: cfg.GetTemperature(),
		Timeout:     timeout,
	})
}

// NewTarget 按数据源快照创建客户端
func NewTarget(t Target) *Client {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(t.BaseURL), "/"),
		apiKey:    strings.TrimSpace(t.APIKey),
		model:     strings.TrimSpace(t.Model),
		temp:      t.Temperature,
		maxTokens: t.MaxTokens,
		http:      &http.Client{Timeout: timeout},
	}
}

// Model 返回模型名（管理接口展示用）
func (c *Client) Model() string { return c.model }

type message struct {
	Role    string `json:"role"`
	// Content 纯文本对话为 string；带图片（CallOptions.Images）时为
	// OpenAI 兼容的 content parts 数组（text + image_url）。
	Content any `json:"content"`
}

// contentPart 多模态消息的一个部分（OpenAI 兼容协议）
type contentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

// imageURL 图片地址：https:// 或 data:image/...;base64, 均可
type imageURL struct {
	URL string `json:"url"`
}

// CallOptions 单次调用的生成参数（区别于数据源级默认配置）。
// 零值合法：不启用 JSON 模式、沿用数据源 max_tokens。
type CallOptions struct {
	// JSONMode 开启后请求带 response_format={"type":"json_object"}（OpenAI 兼容协议），
	// 强制模型输出合法 JSON，省掉「输出不合规 → 喂回自纠」的整轮重试。
	// 数据源不支持时由 Manager 自动降级重试（见 ChatOpts）。
	JSONMode bool
	// MaxTokens 限制补全长度（0 表示用数据源默认）。打分/评审类短输出可设上限防啰嗦。
	MaxTokens int
	// Images 附加到 user 消息的图片（data URI 或 https URL，OpenAI 兼容协议）。
	// 数据源/模型不支持视觉时由 Manager 降级为纯文本重试（见 chatWithOpt）。
	Images []string
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Chat 使用数据源默认温度对话
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	return c.ChatTemp(ctx, system, user, c.temp)
}

// ChatTemp 指定温度对话。图谱抽取要求输出稳定，用 0。
func (c *Client) ChatTemp(ctx context.Context, system, user string, temperature float64) (string, error) {
	out, _, err := c.ChatTempDetailed(ctx, system, user, temperature)
	return out, err
}

// ChatTempDetailed 对话并返回 token 用量（OpenAI 兼容协议 usage 字段；
// 数据源未回 usage 时为零值，记录侧显示 0）。
func (c *Client) ChatTempDetailed(ctx context.Context, system, user string, temperature float64) (string, Usage, error) {
	return c.ChatDetailedOpts(ctx, system, user, temperature, CallOptions{})
}

// ChatDetailedOpts 带生成参数（JSON 模式 / max_tokens 上限 / 视觉图片）的对话。
func (c *Client) ChatDetailedOpts(ctx context.Context, system, user string, temperature float64, opt CallOptions) (string, Usage, error) {
	userContent := buildUserContent(user, opt.Images)
	payload := chatRequest{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: userContent},
		},
		Temperature: temperature,
		MaxTokens:   c.maxTokens,
	}
	if opt.MaxTokens > 0 {
		payload.MaxTokens = opt.MaxTokens
	}
	if opt.JSONMode {
		payload.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && c.apiKey == "" {
			return "", Usage{}, fmt.Errorf("llm api %d: 未配置 API Key: %s", resp.StatusCode, truncate(raw, 300))
		}
		// 带上真实请求地址 + 常见 4xx 的可执行排查提示（404 多是 base_url 路径错或模型名错）
		return "", Usage{}, fmt.Errorf("llm api %d（POST %s）: %s%s",
			resp.StatusCode, req.URL.String(), truncate(raw, 300), hintLLMStatus(resp.StatusCode, raw, c.baseURL))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", Usage{}, err
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", Usage{}, fmt.Errorf("llm error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("llm returned no choices")
	}
	// 正常回复是纯文本；个别网关会原样回显 content parts，取回 text 部分拼接
	switch v := cr.Choices[0].Message.Content.(type) {
	case string:
		return v, cr.Usage, nil
	case []any:
		var b strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String(), cr.Usage, nil
	default:
		return "", Usage{}, fmt.Errorf("llm returned unexpected content type %T", cr.Choices[0].Message.Content)
	}
}

// buildUserContent 组装 user 消息内容：无图片为纯字符串；有图片为 content parts。
func buildUserContent(user string, images []string) any {
	if len(images) == 0 {
		return user
	}
	parts := make([]contentPart, 0, len(images)+1)
	parts = append(parts, contentPart{Type: "text", Text: user})
	for _, img := range images {
		img = strings.TrimSpace(img)
		if img == "" {
			continue
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: img}})
	}
	return parts
}

// ListModels 拉取数据源可用模型（GET {base}/models，OpenAI 兼容）
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models api %d（GET %s）: %s%s",
			resp.StatusCode, req.URL.String(), truncate(raw, 300), hintLLMStatus(resp.StatusCode, raw, c.baseURL))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("list models: 解析响应: %w", err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// hintLLMStatus 把常见 HTTP 错误翻译成可执行的排查建议（OpenAI 兼容协议）。
// 新增原因：配置 Qwen 数据源时 base_url 少了 /compatible-mode、模型名大小写不符，
// 平台只回一个 404，凭错误码无法定位，因此在错误里直接给出下一步动作。
func hintLLMStatus(status int, raw []byte, baseURL string) string {
	body := strings.ToLower(string(raw))
	switch status {
	case http.StatusNotFound:
		if strings.Contains(body, "model") {
			return "；模型不存在：请点「拉取模型」取真实 model id（大小写/版本后缀都要一致）"
		}
		return "；接口路径不存在：base_url 需指向 OpenAI 兼容根路径（如 .../compatible-mode/v1），当前 " + baseURL
	case http.StatusUnauthorized, http.StatusForbidden:
		return "；鉴权失败：确认 API Key 属于该平台，且已开通对应模型权限"
	case http.StatusTooManyRequests:
		return "；触发限流：稍后重试或降低并发"
	}
	return ""
}
