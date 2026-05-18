package proxy

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 输入结构体（OpenAI Chat Completions 格式） ====================

// openAIRequest 表示 OpenAI Chat Completions 请求（仅解析翻译所需字段）
type openAIRequest struct {
	Model           string            `json:"model"`
	Messages        []openAIMessage   `json:"messages"`
	Tools           []json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage   `json:"tool_choice,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort"`
	ServiceTier     string            `json:"service_tier"`
	ServiceTierAlt  string            `json:"serviceTier"` // 兼容驼峰命名
	Metadata        map[string]any    `json:"metadata,omitempty"`
}

// openAIMessage 表示一条 OpenAI 消息
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"` // string 或 []contentPart
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIToolCall 表示 assistant 消息中的工具调用
type openAIToolCall struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIToolParsed 表示解析后的工具定义
type openAIToolParsed struct {
	Type     string          `json:"type"`
	Function *openAIToolFunc `json:"function,omitempty"`
}

// openAIToolFunc 表示工具的函数描述
type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// openAIContentPart 表示多部分内容中的一项
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ==================== 输出结构体（OpenAI 流式/非流式响应格式） ====================

// openAIStreamChunk 流式响应块
type openAIStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *UsageInfo     `json:"usage,omitempty"`
}

// streamChoice 流式块中的选项
type streamChoice struct {
	Index        int          `json:"index"`
	Delta        *streamDelta `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// streamDelta 流式块中的增量内容
type streamDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

// toolCallDelta 工具调用增量
type toolCallDelta struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function toolCallFuncDelta `json:"function"`
}

// toolCallFuncDelta 工具函数增量
type toolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// openAICompactResponse 非流式完整响应
type openAICompactResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created,omitempty"`
	Model   string          `json:"model"`
	Choices []compactChoice `json:"choices"`
	Usage   *UsageInfo      `json:"usage,omitempty"`
}

// compactChoice 非流式响应中的选项
type compactChoice struct {
	Index        int            `json:"index"`
	Message      compactMessage `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// compactMessage 非流式响应中的消息
type compactMessage struct {
	Role      string               `json:"role"`
	Content   *string              `json:"content"`
	ToolCalls []compactToolCallOut `json:"tool_calls,omitempty"`
}

// compactToolCallOut 非流式响应中的工具调用
type compactToolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIErrorResponse 错误响应
type openAIErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ==================== LRU 请求解析缓存 ====================

const requestCacheSize = 256

// maxTools 上游 Codex API 允许的最大工具数量
const maxTools = 128

type requestCacheEntry struct {
	key [32]byte
	req openAIRequest
}

type requestCache struct {
	mu    sync.Mutex
	order *list.List
	items map[[32]byte]*list.Element
}

var globalRequestCache = &requestCache{
	order: list.New(),
	items: make(map[[32]byte]*list.Element, requestCacheSize),
}

func (c *requestCache) get(key [32]byte) (openAIRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return openAIRequest{}, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*requestCacheEntry).req, true
}

func (c *requestCache) put(key [32]byte, req openAIRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*requestCacheEntry).req = req
		return
	}
	elem := c.order.PushFront(&requestCacheEntry{key: key, req: req})
	c.items[key] = elem
	if c.order.Len() <= requestCacheSize {
		return
	}
	tail := c.order.Back()
	if tail == nil {
		return
	}
	c.order.Remove(tail)
	delete(c.items, tail.Value.(*requestCacheEntry).key)
}

// cachedOrParse 从缓存获取或解析请求，返回结构体（Unmarshal 至多一次）
func cachedOrParse(rawJSON []byte) openAIRequest {
	if len(rawJSON) == 0 {
		return openAIRequest{}
	}
	key := sha256.Sum256(rawJSON)
	if req, ok := globalRequestCache.get(key); ok {
		return req
	}
	var req openAIRequest
	_ = json.Unmarshal(rawJSON, &req)
	globalRequestCache.put(key, req)
	return req
}

// ==================== 请求翻译: OpenAI Chat Completions → Codex Responses ====================

// TranslateRequest 将 OpenAI Chat Completions 请求转换为 Codex Responses 格式
// 采用 Unmarshal→构造 map→Marshal 模式，只做一次 JSON 序列化
func TranslateRequest(rawJSON []byte) ([]byte, error) {
	req := cachedOrParse(rawJSON)

	// 构建输出 map（只包含 Codex 需要的字段）
	out := map[string]any{
		"model":   req.Model,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}

	// 1. messages → input
	out["input"] = convertMessagesToInputSlice(req.Messages)

	// 2. reasoning effort
	if effort := normalizeReasoningEffort(req.ReasoningEffort); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	}

	// 3. service tier（保留合法值，丢弃不支持的；fast 映射为上游接受的 priority）
	tier := req.ServiceTier
	if tier == "" {
		tier = req.ServiceTierAlt
	}
	tier = strings.TrimSpace(tier)
	if isAllowedServiceTier(tier) {
		out["service_tier"] = upstreamServiceTier(tier)
	}

	// 4. tools 格式转换 + schema 清理
	if len(req.Tools) > 0 {
		if tools := convertToolsToCodexFormat(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}

	// 5. tool_choice 原样透传（支持 string "auto"/"none"/"required" 或对象形式
	// 例如 {"type":"image_generation"} / {"type":"function","name":"..."}）
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var tc any
		if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
			out["tool_choice"] = tc
		}
	}

	// 6. metadata.image_* 运行时覆盖：将客户端通过 metadata 传入的
	// image_size / quality / background / … 合并到 tools[] 中的
	// image_generation 条目，允许虚拟模型 inject 的默认被单次覆写。
	if len(req.Metadata) > 0 {
		out["metadata"] = cloneMetadata(req.Metadata)
		mergeImageMetadataIntoTools(out)
	}

	return json.Marshal(out)
}

// imageMetadataToolFields 描述 metadata 上的 image_* 键 ⇒ image_generation tool 字段。
// 仅非空值（字符串不能为""）会被合并进 tool。
var imageMetadataToolFields = map[string]string{
	"image_size":               "size",
	"image_quality":            "quality",
	"image_background":         "background",
	"image_output_format":      "output_format",
	"image_output_compression": "output_compression",
	"image_moderation":         "moderation",
	"image_input_fidelity":     "input_fidelity",
	"image_partial_images":     "partial_images",
	"image_model":              "model",
}

// imageMetadataAliases 允许客户端使用 OpenAI Images API 的原生无前缀键名
// （size / quality / background / …），mergeImageMetadataIntoTools 会在入口处将它们
// normalize 为带 image_ 前缀的内部约定。同名带前缀的键已存在时，
// 无前缀键被丢弃（带前缀优先）。详见 docs/gpt-image-web.md。
var imageMetadataAliases = map[string]string{
	"size":               "image_size",
	"quality":            "image_quality",
	"background":         "image_background",
	"output_format":      "image_output_format",
	"output_compression": "image_output_compression",
	"moderation":         "image_moderation",
	"input_fidelity":     "image_input_fidelity",
	"partial_images":     "image_partial_images",
	"model":              "image_model",
}

// normalizeImageMetadataAliases 将无前缀别名键迁移到带 image_ 前缀的约定键。
// 带前缀键已存在时，无前缀键仅被删除（不覆盖，避免同请求中双传导致歧义）。
func normalizeImageMetadataAliases(md map[string]any) {
	for alias, canonical := range imageMetadataAliases {
		v, ok := md[alias]
		if !ok {
			continue
		}
		delete(md, alias)
		if _, exists := md[canonical]; !exists {
			md[canonical] = v
		}
	}
}

// cloneMetadata 浅拷贝一份 metadata，避免修改影响调用者。
func cloneMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// mergeImageMetadataIntoTools 将 body["metadata"] 中的 image_* 字段
// 合并到 body["tools"] 第一个 type=="image_generation" 的条目上。
//
// 规则：
//  1. 入口先 normalize：OpenAI Images 原生无前缀键名（size/quality/…）被迁到
//     带 image_ 前缀的约定名（见 imageMetadataAliases）；
//  2. 仅合并白名单内的键（imageMetadataToolFields）；
//  3. 空字符串忽略（便于客户端用 env 传空值时走 MODEL_OVERRIDES 默认）；
//  4. 若 tools 中暂无 image_generation 条目，则新增一条；
//  5. 上游 codex backend 不接受任意 metadata 字段（返回 Unsupported parameter:
//     metadata），所以函数末尾统一 drop 整份 metadata，未识别字段不透传。
func mergeImageMetadataIntoTools(body map[string]any) {
	md, ok := body["metadata"].(map[string]any)
	if !ok || len(md) == 0 {
		return
	}

	normalizeImageMetadataAliases(md)

	overrides := make(map[string]any, len(imageMetadataToolFields))
	for mdKey, toolKey := range imageMetadataToolFields {
		v, exists := md[mdKey]
		if !exists {
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			delete(md, mdKey)
			continue
		}
		overrides[toolKey] = v
		delete(md, mdKey)
	}

	// 上游不接受任意 metadata 字段（未识别的键也会触发 400），
	// 消费完 image_* 后统一 drop 整份 metadata。
	delete(body, "metadata")

	if len(overrides) == 0 {
		return
	}

	tools, _ := body["tools"].([]any)
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if ty, _ := tm["type"].(string); ty == "image_generation" {
			for k, v := range overrides {
				tm[k] = v
			}
			sanitizeImageTool(tm)
			return
		}
	}

	// 没找到现有 image_generation tool→ 新增一个。
	newTool := map[string]any{"type": "image_generation"}
	for k, v := range overrides {
		newTool[k] = v
	}
	sanitizeImageTool(newTool)
	body["tools"] = append(tools, newTool)
}

// imageModelsSupportingInputFidelity 记录上游支持 input_fidelity 参数的画图模型。
// gpt-image-2（及空值默认，对应上游默认模型 gpt-image-2）不支持该参数，
// 强行传会收到 400 "The model 'gpt-image-2' does not support the 'input_fidelity' parameter"。
// 只有 gpt-image-1 / gpt-image-1.5 / gpt-image-1-mini 保留 input_fidelity。
var imageModelsSupportingInputFidelity = map[string]bool{
	"gpt-image-1":      true,
	"gpt-image-1.5":    true,
	"gpt-image-1-mini": true,
}

// sanitizeImageTool 根据 image_generation tool 当前的 model 值，
// 丢弃上游不兼容的参数，避免上游 400 invalid_input_fidelity_model 等错误。
//
// 目前唯一的过滤规则：
//   - input_fidelity 仅在 model ∈ {gpt-image-1, gpt-image-1.5, gpt-image-1-mini} 时保留。
//     其它值（空、gpt-image-2、未知新模型）会被删除。
func sanitizeImageTool(tool map[string]any) {
	if _, has := tool["input_fidelity"]; !has {
		return
	}
	model, _ := tool["model"].(string)
	if !imageModelsSupportingInputFidelity[model] {
		delete(tool, "input_fidelity")
	}
}

// PrepareResponsesBody 将 Responses API 原始请求转换为上游可接受的格式
// 采用 Unmarshal→map 操作→Marshal 模式，替代逐字段 sjson 操作
// 返回: (处理后的 body, 展开后的 input JSON 原始文本)
func PrepareResponsesBody(rawBody []byte) ([]byte, string) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, ""
	}

	// 1. 强制设置 Codex 必需字段
	body["stream"] = true
	body["store"] = false
	if _, ok := body["include"]; !ok {
		body["include"] = []string{"reasoning.encrypted_content"}
	}

	// 2. 字符串 input → 数组包装（Codex 要求 input 为 list）
	if inputStr, ok := body["input"].(string); ok {
		body["input"] = []map[string]string{
			{"role": "user", "content": inputStr},
		}
	}

	// 3. reasoning_effort → reasoning.effort 自动转换 + 钳位
	if re, ok := body["reasoning_effort"].(string); ok && re != "" {
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		if _, hasEffort := reasoning["effort"]; !hasEffort {
			reasoning["effort"] = re
			body["reasoning"] = reasoning
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			switch strings.ToLower(effort) {
			case "low", "medium", "high", "xhigh":
				// 合法值，保留
			default:
				reasoning["effort"] = "high"
			}
		}
	}

	// 4. service tier 清理（fast 映射为上游接受的 priority）
	delete(body, "serviceTier")
	if tier, ok := body["service_tier"].(string); ok {
		tier = strings.TrimSpace(tier)
		if !isAllowedServiceTier(tier) {
			delete(body, "service_tier")
		} else {
			body["service_tier"] = upstreamServiceTier(tier)
		}
	}

	// 5. 工具描述补充 + schema 清理 + 上游数量限制
	if tools, ok := body["tools"].([]any); ok {
		if len(tools) > maxTools {
			tools = tools[:maxTools]
			body["tools"] = tools
		}
		toolDescDefaults := map[string]string{
			"tool_search": "Search through available tools to find the most relevant one for the task.",
		}
		for _, t := range tools {
			toolMap, ok := t.(map[string]any)
			if !ok {
				continue
			}
			// 补充默认描述
			if toolType, _ := toolMap["type"].(string); toolType != "" {
				if defaultDesc, ok := toolDescDefaults[toolType]; ok {
					desc, _ := toolMap["description"].(string)
					if desc == "" {
						toolMap["description"] = defaultDesc
					}
				}
			}
			// 递归清理不支持的 JSON Schema 关键字，并修正上游要求的结构
			if params, ok := toolMap["parameters"].(map[string]any); ok {
				sanitizeSchemaForUpstream(params)
			}
		}
	}

	// 6a. metadata.image_* 运行时覆盖（v1.7.10+）：将 body["metadata"] 中的
	// image_size / quality / … 合并到 tools[].image_generation 上。
	mergeImageMetadataIntoTools(body)

	// 6. 展开 previous_response_id
	prevID, _ := body["previous_response_id"].(string)
	if prevID != "" {
		if cached := getResponseCache(prevID); cached != nil {
			var cachedItems []any
			for _, item := range cached {
				var v any
				if json.Unmarshal(item, &v) == nil {
					cachedItems = append(cachedItems, v)
				}
			}
			currentInput, _ := body["input"].([]any)
			body["input"] = append(cachedItems, currentInput...)
		}
	}

	// 保存展开后的 input 原始 JSON（用于响应缓存链路）
	var expandedInputRaw string
	if inputVal, ok := body["input"]; ok {
		if b, err := json.Marshal(inputVal); err == nil {
			expandedInputRaw = string(b)
		}
	}

	// 7. 删除 Codex 不支持的字段
	for _, field := range []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "stop", "user",
		"logit_bias", "response_format", "serviceTier",
		"stream_options", "reasoning_effort", "truncation", "context_management",
		"disable_response_storage", "verbosity",
	} {
		delete(body, field)
	}

	result, err := json.Marshal(body)
	if err != nil {
		return rawBody, expandedInputRaw
	}
	return result, expandedInputRaw
}

// PrepareCompactResponsesBody 将 /responses/compact 请求转换为上游可接受的格式。
// 它复用通用 Responses 预处理，但会移除 compact 端点不接受的自动注入字段。
func PrepareCompactResponsesBody(rawBody []byte) ([]byte, string) {
	body, expandedInputRaw := PrepareResponsesBody(rawBody)
	body, _ = sjson.DeleteBytes(body, "include")
	body, _ = sjson.DeleteBytes(body, "store")
	body, _ = sjson.DeleteBytes(body, "stream")
	return body, expandedInputRaw
}

// normalizeReasoningEffort 将 reasoning_effort 钳位到上游支持的值
func normalizeReasoningEffort(effort string) string {
	if effort == "" {
		return ""
	}
	switch strings.ToLower(effort) {
	case "low", "medium", "high", "xhigh":
		return effort
	default:
		return "high"
	}
}

// isAllowedServiceTier 判断 service_tier 是否在上游允许的范围内
func isAllowedServiceTier(tier string) bool {
	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast":
		return true
	default:
		return false
	}
}

// upstreamServiceTier 将客户端 service_tier 映射为上游接受的值（fast → priority）
func upstreamServiceTier(tier string) string {
	if tier == "fast" {
		return "priority"
	}
	return tier
}

// convertMessagesToInputSlice 将 OpenAI messages 转换为 Codex input 数组（纯内存操作，零中间序列化）
func convertMessagesToInputSlice(messages []openAIMessage) []any {
	input := make([]any, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  rawMessageToString(m.Content),
			})

		case "assistant":
			if len(m.ToolCalls) > 0 {
				// 有 tool_calls 的 assistant 消息
				if text := rawMessageToString(m.Content); text != "" {
					input = append(input, map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": text},
						},
					})
				}
				for _, tc := range m.ToolCalls {
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
			} else {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": buildContentPartsSlice("assistant", m.Content),
				})
			}

		case "system":
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": buildContentPartsSlice("system", m.Content),
			})

		default:
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": buildContentPartsSlice("user", m.Content),
			})
		}
	}
	return input
}

// buildContentPartsSlice 将 content（string 或 []contentPart）转为 []any
func buildContentPartsSlice(role string, raw json.RawMessage) []any {
	parts := make([]any, 0)
	if len(raw) == 0 {
		return parts
	}

	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}

	first := firstNonSpace(raw)
	switch first {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return parts
		}
		return append(parts, map[string]any{"type": contentType, "text": s})
	case '[':
		var arr []openAIContentPart
		if json.Unmarshal(raw, &arr) != nil {
			return parts
		}
		for _, item := range arr {
			switch item.Type {
			case "text":
				parts = append(parts, map[string]any{"type": contentType, "text": item.Text})
			case "image_url":
				if item.ImageURL != nil && item.ImageURL.URL != "" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": item.ImageURL.URL})
				}
			}
		}
		return parts
	default:
		return parts
	}
}

// rawMessageToString 安全地将 json.RawMessage 转为 Go string
func rawMessageToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func firstNonSpace(raw json.RawMessage) byte {
	for _, b := range raw {
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b
		}
	}
	return 0
}

// convertToolsToCodexFormat 将 OpenAI 工具格式转为 Codex 格式（纯内存操作）
// OpenAI: {type:"function", function:{name, description, parameters}}
// Codex:  {type:"function", name, description, parameters}
// 上游限制最多 128 个工具，超出部分静默截断
func convertToolsToCodexFormat(rawTools []json.RawMessage) []any {
	cap := len(rawTools)
	if cap > maxTools {
		cap = maxTools
		rawTools = rawTools[:maxTools]
	}
	tools := make([]any, 0, cap)
	for _, raw := range rawTools {
		var parsed openAIToolParsed
		if json.Unmarshal(raw, &parsed) != nil {
			continue
		}

		if parsed.Type != "function" || parsed.Function == nil {
			// 非 function 类型 → 透传原始 JSON
			var passThrough any
			_ = json.Unmarshal(raw, &passThrough)
			tools = append(tools, passThrough)
			continue
		}

		// function 类型 → 提升 function 内字段到顶层
		item := map[string]any{
			"type": "function",
			"name": parsed.Function.Name,
		}
		if parsed.Function.Description != "" {
			item["description"] = parsed.Function.Description
		}
		if len(parsed.Function.Parameters) > 0 {
			var params map[string]any
			if json.Unmarshal(parsed.Function.Parameters, &params) == nil {
				sanitizeSchemaForUpstream(params)
				item["parameters"] = params
			}
		}
		if parsed.Function.Strict != nil {
			item["strict"] = *parsed.Function.Strict
		}
		tools = append(tools, item)
	}
	return tools
}

// ==================== 向后兼容: 辅助函数 ====================

func normalizeServiceTierField(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		tier = strings.TrimSpace(gjson.GetBytes(body, "serviceTier").String())
	}
	if tier == "" {
		return body
	}
	body, _ = sjson.SetBytes(body, "service_tier", tier)
	body, _ = sjson.DeleteBytes(body, "serviceTier")
	return body
}

func sanitizeServiceTierForUpstream(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}
	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast":
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		// fast 映射为上游接受的 priority
		if tier == "fast" {
			body, _ = sjson.SetBytes(body, "service_tier", "priority")
		}
		return body
	default:
		body, _ = sjson.DeleteBytes(body, "service_tier")
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}
}

// resolveServiceTier 从实际 tier 和请求 tier 中选择最终值
func resolveServiceTier(actualTier, requestedTier string) string {
	requestedTier = strings.TrimSpace(requestedTier)
	if requestedTier == "fast" {
		return requestedTier
	}
	actualTier = strings.TrimSpace(actualTier)
	if actualTier != "" {
		return actualTier
	}
	return requestedTier
}

// 上游不支持的 JSON Schema 验证约束关键字
var unsupportedSchemaKeys = map[string]bool{
	"uniqueItems":      true,
	"minItems":         true,
	"maxItems":         true,
	"minimum":          true,
	"maximum":          true,
	"exclusiveMinimum": true,
	"exclusiveMaximum": true,
	"multipleOf":       true,
	"pattern":          true,
	"minLength":        true,
	"maxLength":        true,
	"format":           true,
	"minProperties":    true,
	"maxProperties":    true,
}

// stripUnsupportedSchemaKeys 递归删除 schema 中上游不支持的关键字
func stripUnsupportedSchemaKeys(schema map[string]interface{}) {
	for key := range unsupportedSchemaKeys {
		delete(schema, key)
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				stripUnsupportedSchemaKeys(sub)
			}
		}
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		stripUnsupportedSchemaKeys(items)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					stripUnsupportedSchemaKeys(sub)
				}
			}
		}
	}
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		stripUnsupportedSchemaKeys(addProps)
	}
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for _, v := range defs {
			if sub, ok := v.(map[string]interface{}); ok {
				stripUnsupportedSchemaKeys(sub)
			}
		}
	}
}

func sanitizeSchemaForUpstream(schema map[string]interface{}) {
	stripUnsupportedSchemaKeys(schema)
	ensureArrayItems(schema)
}

// ensureArrayItems 递归为缺失 items 的数组 schema 补上空 schema，
// 兼容上游对 array 必须声明 items 的校验。
func ensureArrayItems(schema map[string]interface{}) {
	if schemaDeclaresArray(schema) {
		if _, ok := schema["items"]; !ok {
			schema["items"] = map[string]interface{}{}
		}
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				ensureArrayItems(sub)
			}
		}
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		ensureArrayItems(items)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					ensureArrayItems(sub)
				}
			}
		}
	}
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		ensureArrayItems(addProps)
	}
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for _, v := range defs {
			if sub, ok := v.(map[string]interface{}); ok {
				ensureArrayItems(sub)
			}
		}
	}
}

func schemaDeclaresArray(schema map[string]interface{}) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "array"
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "array" {
				return true
			}
		}
	}
	return false
}

// ==================== 响应翻译: Codex SSE → OpenAI SSE ====================

// UsageInfo token 使用统计
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
}

// newContentChunk 构建文本内容流式块
func newContentChunk(id, model string, created int64, content string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{Content: &content},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newReasoningChunk 构建推理内容流式块
func newReasoningChunk(id, model string, created int64, reasoning string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{ReasoningContent: &reasoning},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newToolCallAnnouncementChunk 构建 tool call 首块（含 id、type、function.name）
func newToolCallAnnouncementChunk(id, model string, created int64, tcIndex int, callID, funcName string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				Role: "assistant",
				ToolCalls: []toolCallDelta{{
					Index: tcIndex,
					ID:    callID,
					Type:  "function",
					Function: toolCallFuncDelta{
						Name:      funcName,
						Arguments: "",
					},
				}},
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newToolCallDeltaChunk 构建 tool call 参数增量块
func newToolCallDeltaChunk(id, model string, created int64, tcIndex int, argsDelta string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				ToolCalls: []toolCallDelta{{
					Index:    tcIndex,
					Function: toolCallFuncDelta{Arguments: argsDelta},
				}},
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newFinalChunk 构建最终流式块（含 finish_reason 和可选 usage）
func newFinalChunk(id, model string, created int64, finishReason string, usage *UsageInfo) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newErrorResponse 构建错误响应
func newErrorResponse(message string) []byte {
	resp := openAIErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "upstream_error"
	b, _ := json.Marshal(resp)
	return b
}

// TranslateStreamChunk 将 Codex SSE 数据块翻译为 OpenAI Chat Completions 流式格式（无状态）
func TranslateStreamChunk(eventData []byte, model string, chunkID string, created int64) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newContentChunk(chunkID, model, created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newReasoningChunk(chunkID, model, created, delta), false

	case "response.output_item.done":
		// image_generation_call 终稿：优先写入本地 /images 静态目录并返回 URL；
		// 本地存储未启用（IMAGE_BASE_URL 未配置）时退回 data URL。
		if gjson.GetBytes(eventData, "item.type").String() == "image_generation_call" {
			if b64 := gjson.GetBytes(eventData, "item.result").String(); b64 != "" {
				return newContentChunk(chunkID, model, created, imageMarkdownFromBase64(b64)), false
			}
		}
		return nil, false

	case "response.completed":
		usage := extractUsage(eventData)
		return newFinalChunk(chunkID, model, created, "stop", usage), true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return newErrorResponse(errMsg), true

	case "response.image_generation_call.partial_image":
		// 渐进式预览帧：客户端通过 metadata.image_partial_images = N
		// 让上游在终稿之前发 N 张中间帧。每帧 partial_image_b64 字段都是
		// 一张完整的 base64 PNG，按图像生成的 SSE 顺序到达。
		// 这里和终稿走同样的落盘路径，作为 markdown image 注入 content delta；
		// 客户端可以收到一系列 ![image](url1) ![image](url2) ![image](finalurl)。
		if b64 := gjson.GetBytes(eventData, "partial_image_b64").String(); b64 != "" {
			return newContentChunk(chunkID, model, created, imageMarkdownFromBase64(b64)), false
		}
		return nil, false

	case "response.content_part.done",
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.image_generation_call.in_progress", "response.image_generation_call.generating",
		"response.image_generation_call.completed":
		return nil, false

	default:
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(chunkID, model, created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== 有状态流式转换器（支持 Function Calling） ====================

// ToolCallResult 表示一个完整的工具调用结果
type ToolCallResult struct {
	ID        string
	Name      string
	Arguments string
}

// StreamTranslator 有状态的流式响应翻译器，跟踪 function_call 索引映射
type StreamTranslator struct {
	Model        string
	ChunkID      string
	Created      int64
	HasToolCalls bool
	toolCallMap  map[string]int // Codex item.id → OpenAI tool_calls index
	nextIdx      int
}

// NewStreamTranslator 创建流式翻译器实例
func NewStreamTranslator(chunkID, model string, created int64) *StreamTranslator {
	return &StreamTranslator{
		Model:       model,
		ChunkID:     chunkID,
		Created:     created,
		toolCallMap: make(map[string]int),
	}
}

// Translate 将单个 Codex SSE 事件翻译为 OpenAI Chat Completions 流式格式
func (st *StreamTranslator) Translate(eventData []byte) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newContentChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newReasoningChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.output_item.added":
		itemType := gjson.GetBytes(eventData, "item.type").String()
		if itemType != "function_call" {
			return nil, false
		}
		callID := gjson.GetBytes(eventData, "item.call_id").String()
		name := gjson.GetBytes(eventData, "item.name").String()
		itemID := gjson.GetBytes(eventData, "item.id").String()

		tcIdx := st.nextIdx
		st.toolCallMap[itemID] = tcIdx
		st.nextIdx++
		st.HasToolCalls = true

		return newToolCallAnnouncementChunk(st.ChunkID, st.Model, st.Created, tcIdx, callID, name), false

	case "response.function_call_arguments.delta":
		itemID := gjson.GetBytes(eventData, "item_id").String()
		tcIdx, ok := st.toolCallMap[itemID]
		if !ok {
			return nil, false
		}
		delta := gjson.GetBytes(eventData, "delta").String()
		return newToolCallDeltaChunk(st.ChunkID, st.Model, st.Created, tcIdx, delta), false

	case "response.function_call_arguments.done":
		return nil, false

	case "response.output_item.done":
		// image_generation_call 终稿：优先写本地 /images → URL，未配置则退回 data URL。
		if gjson.GetBytes(eventData, "item.type").String() == "image_generation_call" {
			if b64 := gjson.GetBytes(eventData, "item.result").String(); b64 != "" {
				return newContentChunk(st.ChunkID, st.Model, st.Created, imageMarkdownFromBase64(b64)), false
			}
		}
		return nil, false

	case "response.completed":
		usage := extractUsage(eventData)
		finishReason := "stop"
		if st.HasToolCalls {
			finishReason = "tool_calls"
		}
		return newFinalChunk(st.ChunkID, st.Model, st.Created, finishReason, usage), true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return newErrorResponse(errMsg), true

	case "response.image_generation_call.partial_image":
		// 渐进式预览帧：见 stateless Translate 路径的同名 case 说明。
		if b64 := gjson.GetBytes(eventData, "partial_image_b64").String(); b64 != "" {
			return newContentChunk(st.ChunkID, st.Model, st.Created, imageMarkdownFromBase64(b64)), false
		}
		return nil, false

	case "response.content_part.done",
		"response.created", "response.in_progress",
		"response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.image_generation_call.in_progress", "response.image_generation_call.generating",
		"response.image_generation_call.completed":
		return nil, false

	default:
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(st.ChunkID, st.Model, st.Created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== 非流式响应翻译 ====================

// TranslateCompactResponse 将 Codex 非流式响应转换为 OpenAI 格式
func TranslateCompactResponse(responseData []byte, model string, id string) []byte {
	var outputText string
	output := gjson.GetBytes(responseData, "output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "message" {
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							outputText += part.Get("text").String()
						}
						return true
					})
				}
			}
			return true
		})
	}

	usage := extractUsage(responseData)

	resp := openAICompactResponse{
		ID:     id,
		Object: "chat.completion",
		Model:  model,
		Choices: []compactChoice{{
			Index: 0,
			Message: compactMessage{
				Role:    "assistant",
				Content: &outputText,
			},
			FinishReason: "stop",
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(resp)
	return b
}

// BuildCompactResponse 构建非流式完整响应（供 handler.go 调用，替代内联 sjson）
// 当有 toolCalls 且 content 为空时，content 输出为 JSON null
func BuildCompactResponse(id, model string, created int64, content string, toolCalls []ToolCallResult, usage *UsageInfo) []byte {
	finishReason := "stop"
	msg := compactMessage{
		Role:    "assistant",
		Content: &content,
	}

	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		if content == "" {
			msg.Content = nil // JSON null
		}
		msg.ToolCalls = make([]compactToolCallOut, len(toolCalls))
		for i, tc := range toolCalls {
			msg.ToolCalls[i] = compactToolCallOut{
				ID:   tc.ID,
				Type: "function",
			}
			msg.ToolCalls[i].Function.Name = tc.Name
			msg.ToolCalls[i].Function.Arguments = tc.Arguments
		}
	}

	resp := openAICompactResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []compactChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(resp)
	return b
}

// ==================== 公共工具函数 ====================

// extractUsage 从 response.completed 事件提取 usage
func extractUsage(eventData []byte) *UsageInfo {
	return extractUsageFromResult(gjson.GetBytes(eventData, "response.usage"))
}

// extractUsageFromResult 从已解析的 gjson.Result 提取 usage（避免重复解析）
func extractUsageFromResult(usage gjson.Result) *UsageInfo {
	if !usage.Exists() {
		return nil
	}
	inputTokens := int(usage.Get("input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	reasoningTokens := int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	cachedTokens := int(usage.Get("input_tokens_details.cached_tokens").Int())
	return &UsageInfo{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		CachedTokens:     cachedTokens,
	}
}

// ExtractToolCallFromDoneEvent 从 response.output_item.done 事件提取 function_call。
// 非 function_call 项返回 nil。OpenAI Responses 规范保证 done 事件 item.arguments 已是完整字符串。
func ExtractToolCallFromDoneEvent(eventData []byte) *ToolCallResult {
	item := gjson.GetBytes(eventData, "item")
	if item.Get("type").String() != "function_call" {
		return nil
	}
	return &ToolCallResult{
		ID:        item.Get("call_id").String(),
		Name:      item.Get("name").String(),
		Arguments: item.Get("arguments").String(),
	}
}

// ExtractToolCallsFromOutput 从 response.completed 事件的 output 数组中提取 function_call 项
func ExtractToolCallsFromOutput(eventData []byte) []ToolCallResult {
	var toolCalls []ToolCallResult
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return nil
	}
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			toolCalls = append(toolCalls, ToolCallResult{
				ID:        item.Get("call_id").String(),
				Name:      item.Get("name").String(),
				Arguments: item.Get("arguments").String(),
			})
		}
		return true
	})
	return toolCalls
}
