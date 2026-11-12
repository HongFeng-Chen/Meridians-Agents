// client.go 图片生成厂商客户端（OpenAI 兼容 /images 协议族）。
//
// 统一流程：发请求 → 解析 data[].url / data[].b64_json（兼容部分厂商的
// images[] 字段）→ 每个产物转存本地 → 返回 /api/files/ 地址列表。
package image_generations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/mediastore"
)

// ImageClient 图片生成客户端
type ImageClient struct {
	http *http.Client
}

// NewImageClient 创建客户端
func NewImageClient() *ImageClient {
	return &ImageClient{http: &http.Client{}}
}

var _ llm.ImageGenClient = (*ImageClient)(nil)

// Generate 生成图片并转存本地。请求超时由目标快照携带（媒体生成默认 ≥300s）。
func (c *ImageClient) Generate(ctx context.Context, t llm.MediaTarget, in *llm.ImageGenInput) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	base := strings.TrimRight(t.BaseURL, "/")
	var outputs []imgOutput
	switch {
	case in.Mode == llm.ModeImg2Img && t.Kind == llm.KindOpenAI:
		// gpt-image-1 的图生图走 multipart edits（参考图作为文件上传）
		var err error
		outputs, err = c.generationsEdits(ctx, base, t.APIKey, t.Model, in)
		if err != nil {
			return nil, err
		}
	default:
		var err error
		outputs, err = c.generationsJSON(ctx, base, t.APIKey, t.Model, in)
		if err != nil {
			return nil, err
		}
	}

	// 转存：b64 直接解码；url 下载
	urls := make([]string, 0, len(outputs))
	for i, o := range outputs {
		local, err := c.persist(ctx, o, i)
		if err != nil {
			// 一张失败不拖累整批：已落盘的照常返回
			continue
		}
		urls = append(urls, local)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("厂商未返回可保存的图片产物")
	}
	return urls, nil
}

// imgOutput 厂商返回的单个产物
type imgOutput struct {
	URL string
	B64 string
	Ext string // b64 解码时用（厂商不回格式，默认 png）
}

// generationsJSON 文生图（及非 openai 的图生图）
func (c *ImageClient) generationsJSON(ctx context.Context, base, apiKey, model string, in *llm.ImageGenInput) ([]imgOutput, error) {
	payload := map[string]any{
		"model":  model,
		"prompt": in.Prompt,
		"n":      in.Count,
	}
	if in.Size != "" {
		payload["size"] = in.Size
	}
	if in.Seed != nil {
		payload["seed"] = *in.Seed
	}
	if in.Mode == llm.ModeImg2Img {
		// SiliconFlow Kolors 风格：image 字段为 base64 / data URI / URL；
		// 单图传字符串（兼容面最广），多图传数组
		if len(in.Images) == 1 {
			payload["image"] = in.Images[0]
		} else {
			payload["image"] = in.Images
		}
		// OpenAI 协议的 generations 不接受 image，个别网关会 400；
		// 参数缺失比 400 好排查，故不额外删字段
	}
	out, err := c.postJSON(ctx, base+"/images/generations", apiKey, payload)
	if err != nil {
		return nil, err
	}
	return parseOutputs(out)
}

// generationsEdits OpenAI 图生图（multipart /images/edits）
func (c *ImageClient) generationsEdits(ctx context.Context, base, apiKey, model string, in *llm.ImageGenInput) ([]imgOutput, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("prompt", in.Prompt)
	if in.Size != "" {
		_ = mw.WriteField("size", in.Size)
	}
	_ = mw.WriteField("n", fmt.Sprintf("%d", in.Count))
	for i, img := range in.Images {
		data, ext, err := decodeDataURL(img)
		if err != nil {
			return nil, fmt.Errorf("参考图 %d 无效: %w", i+1, err)
		}
		fw, _ := mw.CreateFormFile("image", fmt.Sprintf("ref-%d%s", i+1, ext))
		_, _ = fw.Write(data)
	}
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/images/edits", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return c.doAndParse(req)
}

// postJSON 发 JSON 请求并解析响应（错误体带厂商原话，便于排查）
func (c *ImageClient) postJSON(ctx context.Context, url, apiKey string, payload map[string]any) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return c.doAndParseMap(req)
}

// doAndParseMap 通用响应处理
func (c *ImageClient) doAndParseMap(req *http.Request) (map[string]any, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("图片生成请求: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("images api %d（POST %s）: %s",
			resp.StatusCode, req.URL.Path, truncateForErr(raw, 300))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("图片生成响应解析: %w", err)
	}
	if e, ok := out["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		return nil, fmt.Errorf("厂商错误: %s", msg)
	}
	return out, nil
}

func (c *ImageClient) doAndParse(req *http.Request) ([]imgOutput, error) {
	out, err := c.doAndParseMap(req)
	if err != nil {
		return nil, err
	}
	return parseOutputs(out)
}

// parseOutputs 兼容两种响应形态：OpenAI 的 data[]、部分厂商的 images[]
func parseOutputs(out map[string]any) ([]imgOutput, error) {
	for _, key := range []string{"data", "images"} {
		arr, ok := out[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		var outputs []imgOutput
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			o := imgOutput{Ext: ".png"}
			if u, ok := m["url"].(string); ok && u != "" {
				o.URL = u
			}
			if b, ok := m["b64_json"].(string); ok && b != "" {
				o.B64 = b
			}
			// 个别厂商把 MIME 放在 content_type / mime_type
			if mt, ok := m["content_type"].(string); ok {
				if ext := mediastore.ExtByMIME(mt); ext != "" {
					o.Ext = ext
				}
			}
			if o.URL != "" || o.B64 != "" {
				outputs = append(outputs, o)
			}
		}
		if len(outputs) > 0 {
			return outputs, nil
		}
	}
	return nil, fmt.Errorf("响应里没有图片产物（data / images 均为空）")
}

// persist 产物落盘，返回本地 URL
func (c *ImageClient) persist(ctx context.Context, o imgOutput, idx int) (string, error) {
	if o.B64 != "" {
		data, err := base64.StdEncoding.DecodeString(o.B64)
		if err != nil {
			return "", fmt.Errorf("b64 解码: %w", err)
		}
		return mediastore.SaveBytes(o.Ext, bytes.NewReader(data))
	}
	return mediastore.SaveFromURL(ctx, o.URL)
}

// decodeDataURL data URI → 原始字节 + 扩展名；https URL 会原地下载
func decodeDataURL(img string) ([]byte, string, error) {
	if strings.HasPrefix(img, "data:") {
		i := strings.Index(img, ",")
		if i < 0 {
			return nil, "", fmt.Errorf("data URI 缺少逗号分隔")
		}
		head, payload := img[:i], img[i+1:]
		ext := ".png"
		if j := strings.Index(head, "image/"); j >= 0 {
			if e := mediastore.ExtByMIME("image/" + head[j+len("image/"):]); e != "" {
				ext = e
			}
		}
		var data []byte
		var err error
		if strings.Contains(head, ";base64") {
			data, err = base64.StdEncoding.DecodeString(payload)
		} else {
			data = []byte(payload)
		}
		if err != nil {
			return nil, "", err
		}
		return data, ext, nil
	}
	// 远端参考图：下载成字节再上传
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("参考图下载 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, "", err
	}
	ext := mediastore.ExtByMIME(resp.Header.Get("Content-Type"))
	if ext == "" {
		ext = strings.ToLower(mediastore.ExtByMIME(""))
		if e := extFromURL(img); e != "" {
			ext = e
		}
	}
	if ext == "" {
		ext = ".png"
	}
	return data, ext, nil
}

func extFromURL(u string) string {
	if i := strings.LastIndexByte(u, '.'); i > 0 {
		cand := u[i:]
		if j := strings.IndexByte(cand, '?'); j > 0 {
			cand = cand[:j]
		}
		if len(cand) <= 6 {
			return cand
		}
	}
	return ""
}

func truncateForErr(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
