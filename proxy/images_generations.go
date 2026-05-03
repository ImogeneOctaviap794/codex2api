package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/api"
)

// ==================== OpenAI 官方 Images API 兼容层 ====================
//
// 实现 POST /v1/images/generations，字段和响应完全遵循 OpenAI 官方规范：
// https://platform.openai.com/docs/api-reference/images/create
//
// 请求字段：
//   - model            (string)  "gpt-image-2" / "gpt-image-1.5" / "gpt-image-1" / "gpt-image-1-mini"
//   - prompt           (string)  必填
//   - n                (int)     生成数量，默认 1（目前只支持 1，n>1 会并发发 N 次）
//   - size             (string)  "1024x1024" / "1024x1536" / "1536x1024" / "auto"
//   - quality          (string)  "low" / "medium" / "high" / "auto"
//   - background       (string)  "opaque" / "transparent" / "auto"
//   - output_format    (string)  "png" / "jpeg" / "webp"
//   - output_compression (int)   0-100（仅 jpeg/webp）
//   - moderation       (string)  "auto" / "low"
//   - response_format  (string)  "url" / "b64_json"，默认 "url"（我们的扩展，
//                                OpenAI gpt-image-1 官方只返回 b64_json）
//   - partial_images   (int)     0-3（流式，本实现暂返回终稿）
//   - user             (string)  终端用户标识（仅透传到日志）
//
// 响应格式（与 OpenAI gpt-image-1 对齐）：
//   {
//     "created": <unix>,
//     "data": [ { "url": "<public_url>"  }  ]   // response_format=url
//     "data": [ { "b64_json": "<base64>" } ]    // response_format=b64_json
//   }

// imagesGenerationsRequest 官方格式请求体
type imagesGenerationsRequest struct {
	Model             string `json:"model"`
	Prompt            string `json:"prompt"`
	N                 int    `json:"n,omitempty"`
	Size              string `json:"size,omitempty"`
	Quality           string `json:"quality,omitempty"`
	Background        string `json:"background,omitempty"`
	OutputFormat      string `json:"output_format,omitempty"`
	OutputCompression int    `json:"output_compression,omitempty"`
	Moderation        string `json:"moderation,omitempty"`
	ResponseFormat    string `json:"response_format,omitempty"`
	PartialImages     int    `json:"partial_images,omitempty"`
	User              string `json:"user,omitempty"`
}

type imagesGenerationsDataItem struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type imagesGenerationsResponse struct {
	Created int64                       `json:"created"`
	Data    []imagesGenerationsDataItem `json:"data"`
	Usage   any                         `json:"usage,omitempty"`
}

// officialImageModels OpenAI 官方画图模型名（作为 image_generation tool 的 model 参数合法值）。
// 客户端传这些值 → 映射到 tools[0].model；其它值保持 metadata.image_model 原样透传。
var officialImageModels = map[string]bool{
	"gpt-image-2":      true,
	"gpt-image-1.5":    true,
	"gpt-image-1":      true,
	"gpt-image-1-mini": true,
}

// internalVirtualModelForImagesAPI 内部走 Chat Completions 通道时使用的虚拟模型名。
// 该名字对应 MODEL_OVERRIDES 中的"万能画图模型"（只开 image_generation tool，
// 不锁任何参数），所有维度都由 metadata.image_* 动态控制。
// 注意：和 OpenAI 官方的 "gpt-image-2" 只是名字相同，层级不同——官方的是底层画图
// 引擎模型，我们的是 codex2api 的虚拟模型别名（base_model=gpt-5.4-mini）。
const internalVirtualModelForImagesAPI = "gpt-image-2"

// imageURLRegex 从 Chat Completions 返回的 markdown 中抽取图像 URL。
// 匹配形如 "https://host[/path]/<hash>.<ext>"，ext 支持 png/jpg/jpeg/webp。
// 历史上 codex2api 内置图床放在 /images/ 子路径下，新图床（如 img.niji.edu.rs）
// 直接将文件挂在根路径，因此不再要求 /images/ 前缀，只要 URL 末段是
// "<hex-hash>.<ext>" 即可识别。
var imageURLRegex = regexp.MustCompile(`https?://[^\s)"\]]+/[a-fA-F0-9]+\.(?:png|jpe?g|webp)`)

// ImagesGenerations 处理 /v1/images/generations 请求（OpenAI 官方兼容）
func (h *Handler) ImagesGenerations(c *gin.Context) {
	var req imagesGenerationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid request body: "+err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		api.SendMissingFieldError(c, "prompt")
		return
	}
	if req.N < 0 || req.N > 10 {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, "n must be between 1 and 10", api.ErrorTypeInvalidRequest))
		return
	}
	if req.N == 0 {
		req.N = 1
	}

	// 默认响应格式：我们用 url（更高效，CDN 友好）。客户端可显式设 b64_json 走官方行为。
	if req.ResponseFormat == "" {
		req.ResponseFormat = "url"
	}
	if req.ResponseFormat != "url" && req.ResponseFormat != "b64_json" {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, "response_format must be 'url' or 'b64_json'", api.ErrorTypeInvalidRequest))
		return
	}

	// 构建 metadata：把官方参数映射到 metadata.image_*（由 codex2api 的
	// mergeImageMetadataIntoTools 合并到 tools[0] 的 image_generation）
	metadata := map[string]any{}
	if req.Size != "" {
		metadata["image_size"] = req.Size
	}
	if req.Quality != "" {
		metadata["image_quality"] = req.Quality
	}
	if req.Background != "" {
		metadata["image_background"] = req.Background
	}
	if req.OutputFormat != "" {
		metadata["image_output_format"] = req.OutputFormat
	}
	if req.OutputCompression > 0 {
		metadata["image_output_compression"] = req.OutputCompression
	}
	if req.Moderation != "" {
		metadata["image_moderation"] = req.Moderation
	}
	if req.PartialImages > 0 {
		metadata["image_partial_images"] = req.PartialImages
	}
	// 客户端传的 model 如果是官方画图模型名 → 映射到 tools[0].model
	// 非官方值（如 "" 或虚拟模型名）不填，让上游用默认
	if req.Model != "" && officialImageModels[req.Model] {
		metadata["image_model"] = req.Model
	}

	// 执行 N 次单图生成（n > 1 串行即可；图少不值得并发）
	items := make([]imagesGenerationsDataItem, 0, req.N)
	for i := 0; i < req.N; i++ {
		url, err := h.generateSingleImageViaInternalChat(c, req.Prompt, metadata)
		if err != nil {
			api.SendError(c, api.NewAPIError(api.ErrCodeUpstreamError, err.Error(), api.ErrorTypeUpstream))
			return
		}
		item := imagesGenerationsDataItem{}
		switch req.ResponseFormat {
		case "b64_json":
			b64, err := readImageAsBase64FromURL(url)
			if err != nil {
				api.SendError(c, api.NewAPIError(api.ErrCodeServerError, "Failed to load image for b64_json: "+err.Error(), api.ErrorTypeServer))
				return
			}
			item.B64JSON = b64
		default: // "url"
			item.URL = url
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, imagesGenerationsResponse{
		Created: time.Now().Unix(),
		Data:    items,
	})
}

// generateSingleImageViaInternalChat 内部 HTTP 打到自己的 /v1/chat/completions，
// 复用已成熟的账号池 + 上游 SSE + 图像落盘流水线。返回公网可访问的图像 URL。
func (h *Handler) generateSingleImageViaInternalChat(c *gin.Context, prompt string, metadata map[string]any) (string, error) {
	body := map[string]any{
		"model":  internalVirtualModelForImagesAPI,
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	if len(metadata) > 0 {
		// 浅拷贝 metadata 避免被内部管线删字段影响外层 N 次循环
		md := make(map[string]any, len(metadata))
		for k, v := range metadata {
			md[k] = v
		}
		body["metadata"] = md
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	// 确定本进程监听端口（与外部 nginx 反代端口一致）
	port := strings.TrimSpace(os.Getenv("CODEX_PORT"))
	if port == "" {
		port = "8080"
	}
	internalURL := "http://127.0.0.1:" + port + "/v1/chat/completions"

	httpReq, err := http.NewRequestWithContext(c.Request.Context(), "POST", internalURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 转发原始认证头（已通过 auth 中间件校验）
	if auth := c.GetHeader("Authorization"); auth != "" {
		httpReq.Header.Set("Authorization", auth)
	}
	if xk := c.GetHeader("x-api-key"); xk != "" {
		httpReq.Header.Set("x-api-key", xk)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// 尽量提取 upstream error message
		if msg := gjson.GetBytes(respBody, "error.message").String(); msg != "" {
			return "", errors.New(msg)
		}
		return "", errors.New("upstream non-200: " + strings.TrimSpace(string(respBody)))
	}

	content := gjson.GetBytes(respBody, "choices.0.message.content").String()
	match := imageURLRegex.FindString(content)
	if match == "" {
		return "", errors.New("image URL not found in upstream response")
	}
	return match, nil
}

// readImageAsBase64FromURL 从本地磁盘读回图像字节并 base64 编码。
// URL 形如 "https://host/images/<hash>.<ext>"，文件位于 IMAGE_STORE_DIR 下。
func readImageAsBase64FromURL(imageURL string) (string, error) {
	_ = initImageStorage()
	dir, _ := imageStorageDir.Load().(string)
	if dir == "" {
		return "", errors.New("image storage directory not configured")
	}
	idx := strings.LastIndex(imageURL, "/")
	if idx < 0 || idx == len(imageURL)-1 {
		return "", errors.New("invalid image URL: " + imageURL)
	}
	filename := imageURL[idx+1:]
	// 安全校验：禁止路径穿越
	if strings.Contains(filename, "..") || strings.ContainsAny(filename, "/\\") {
		return "", errors.New("invalid image filename")
	}
	data, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
