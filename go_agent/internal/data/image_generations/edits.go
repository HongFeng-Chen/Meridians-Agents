// edits.go OpenAI 图生图：multipart POST /images/edits（gpt-image-1 等）。
// 参考图作为文件上传；data URI / 远端 URL 统一解码下载成字节再传。
package image_generations

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"go_agent/internal/biz/llm"
	"go_agent/internal/pkg/mediastore"
)

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
