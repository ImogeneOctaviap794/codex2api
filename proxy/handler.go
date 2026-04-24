package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Handler API 路由处理器
type Handler struct {
	store      *auth.Store
	configKeys map[string]bool // 配置文件中的静态 key
	db         *database.DB
	cfg        *config.Config       // 全局配置
	deviceCfg  *DeviceProfileConfig // 设备指纹配置

	// 动态 key 缓存
	dbKeysMu    sync.RWMutex
	dbKeys      map[string]*database.APIKeyRow
	dbKeysUntil time.Time
}

func (h *Handler) nextAccountForSession(sessionID string, exclude map[int64]bool) (*auth.Account, string) {
	if h == nil || h.store == nil {
		return nil, ""
	}
	return h.store.NextForSession(sessionID, exclude)
}

// nextAccountForSessionWithPreference 按 plan 偏好的两阶段选号：
//  1. 先把 preferPlan 以外的账号全部视作 exclude，只在偏好池中选；
//  2. 偏好池空/全忙时，回退到普通 exclude 选号。
// preferPlan 为空则退化为普通选号。
func (h *Handler) nextAccountForSessionWithPreference(sessionID string, exclude map[int64]bool, preferPlan string) (*auth.Account, string) {
	if h == nil || h.store == nil {
		return nil, ""
	}
	preferPlan = strings.ToLower(strings.TrimSpace(preferPlan))
	if preferPlan != "" {
		nonPref := h.store.AccountIDsExcludingPlans(preferPlan)
		if len(nonPref) > 0 {
			merged := make(map[int64]bool, len(exclude)+len(nonPref))
			for id := range exclude {
				merged[id] = true
			}
			for id := range nonPref {
				merged[id] = true
			}
			if acc, sticky := h.store.NextForSession(sessionID, merged); acc != nil {
				return acc, sticky
			}
		}
	}
	return h.store.NextForSession(sessionID, exclude)
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

type CodexUsageSyncResult struct {
	UsagePct7d           float64
	HasUsage7d           bool
	UsagePct5h           float64
	Reset5hAt            time.Time
	HasUsage5h           bool
	Used5hHeaders        bool
	Persisted5hOnly      bool
	Premium5hRateLimited bool
}

type codexRateLimitWindow string

const (
	codexRateLimitWindowUnknown codexRateLimitWindow = ""
	codexRateLimitWindowShort   codexRateLimitWindow = "short"
	codexRateLimitWindow5h      codexRateLimitWindow = "5h"
	codexRateLimitWindow7d      codexRateLimitWindow = "7d"
)

type codex429Decision struct {
	Premium5h bool
	ResetAt   time.Time
	Cooldown  time.Duration
}

const (
	contextAPIKeyID     = "apiKeyID"
	contextAPIKeyName   = "apiKeyName"
	contextAPIKeyMasked = "apiKeyMasked"
)

// NewHandler 创建处理器
func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler {
	return &Handler{
		store:      store,
		configKeys: make(map[string]bool), // 不再使用硬编码，但保留结构以向后兼容逻辑
		db:         db,
		cfg:        cfg,
		deviceCfg:  deviceCfg,
	}
}

// NewHandlerWithDeviceProfile 创建处理器（带设备指纹配置）
func NewHandlerWithDeviceProfile(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return NewHandler(store, db, nil, deviceCfg)
}

// refreshDBKeys 从数据库刷新密钥缓存（5 分钟）
func (h *Handler) refreshDBKeys() map[string]*database.APIKeyRow {
	h.dbKeysMu.RLock()
	if time.Now().Before(h.dbKeysUntil) {
		keys := h.dbKeys
		h.dbKeysMu.RUnlock()
		return keys
	}
	h.dbKeysMu.RUnlock()

	h.dbKeysMu.Lock()
	defer h.dbKeysMu.Unlock()

	// double check
	if time.Now().Before(h.dbKeysUntil) {
		return h.dbKeys
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		log.Printf("刷新 API Keys 缓存失败: %v", err)
		return h.dbKeys
	}

	newMap := make(map[string]*database.APIKeyRow, len(rows))
	for _, row := range rows {
		if row == nil || row.Key == "" {
			continue
		}
		newMap[row.Key] = row
	}
	h.dbKeys = newMap
	h.dbKeysUntil = time.Now().Add(5 * time.Minute)
	return newMap
}

func (h *Handler) resolveAPIKey(key string) (*database.APIKeyRow, bool) {
	if h.configKeys[key] {
		return &database.APIKeyRow{
			ID:   0,
			Name: "config",
			Key:  key,
		}, true
	}
	dbKeys := h.refreshDBKeys()
	row, ok := dbKeys[key]
	return row, ok
}

// isValidKey 检查 key 是否有效（配置文件 + DB）
func (h *Handler) isValidKey(key string) bool {
	_, ok := h.resolveAPIKey(key)
	return ok
}

// hasAnyKeys 检查是否配置了任何密钥
func (h *Handler) hasAnyKeys() bool {
	if len(h.configKeys) > 0 {
		return true
	}
	dbKeys := h.refreshDBKeys()
	return len(dbKeys) > 0
}

// logUsage 记录请求日志（非阻塞，写入内存缓冲由后台批量 flush）
func (h *Handler) logUsage(input *database.UsageLogInput) {
	if h.db == nil || input == nil {
		return
	}
	_ = h.db.InsertUsageLog(context.Background(), input)
}

func populateAPIKeyMetaFromContext(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	if v, exists := c.Get(contextAPIKeyID); exists && v != nil {
		switch typed := v.(type) {
		case int64:
			input.APIKeyID = typed
		case int:
			input.APIKeyID = int64(typed)
		}
	}
	if v, exists := c.Get(contextAPIKeyName); exists && v != nil {
		if name, ok := v.(string); ok {
			input.APIKeyName = name
		}
	}
	if v, exists := c.Get(contextAPIKeyMasked); exists && v != nil {
		if masked, ok := v.(string); ok {
			input.APIKeyMasked = masked
		}
	}
}

func (h *Handler) logUsageForRequest(c *gin.Context, input *database.UsageLogInput) {
	populateAPIKeyMetaFromContext(c, input)
	h.logUsage(input)
}

// extractReasoningEffort 从请求体提取推理强度
// 支持 reasoning.effort（Responses API）和 reasoning_effort（Chat Completions API）
func extractReasoningEffort(body []byte) string {
	// Responses API: reasoning.effort
	if effort := gjson.GetBytes(body, "reasoning.effort").String(); effort != "" {
		return effort
	}
	// Chat Completions API: reasoning_effort
	if effort := gjson.GetBytes(body, "reasoning_effort").String(); effort != "" {
		return effort
	}
	return ""
}

// extractServiceTier 从请求体提取服务等级
func extractServiceTier(body []byte) string {
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" {
		return tier
	}
	return gjson.GetBytes(body, "serviceTier").String()
}

// stripDisabledFastAlias 当 service_tier=fast 的别名被关闭时，从请求体里移除
// service_tier，避免下游 upstreamServiceTier()/sanitizeServiceTierForUpstream()
// 仍把 fast 映射成 priority 发给上游。
//
// 调用顺序必须在 extractServiceTier 之后，这样原始 "fast" 仍会被记录到 usage_logs
// 便于审计；上游则会走 default 队列。
func stripDisabledFastAlias(rawBody []byte, aliasEnabled bool) []byte {
	if aliasEnabled || len(rawBody) == 0 {
		return rawBody
	}
	tier := strings.TrimSpace(gjson.GetBytes(rawBody, "service_tier").String())
	if !strings.EqualFold(tier, "fast") {
		return rawBody
	}
	if stripped, err := sjson.DeleteBytes(rawBody, "service_tier"); err == nil {
		return stripped
	}
	return rawBody
}

func classifyTransportFailure(err error) string {
	if err == nil {
		return ""
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return "timeout"
	}
	return "transport"
}

func classifyHTTPFailure(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized:
		return "unauthorized"
	case statusCode == http.StatusTooManyRequests:
		return "" // 429 由 applyCooldown 单独处理
	case statusCode >= 500:
		return "server"
	case statusCode >= 400:
		return "client"
	default:
		return ""
	}
}

type streamOutcome struct {
	logStatusCode  int
	failureKind    string
	failureMessage string
	penalize       bool
}

func classifyStreamOutcome(ctxErr, readErr, writeErr error, gotTerminal bool) streamOutcome {
	if gotTerminal {
		return streamOutcome{logStatusCode: http.StatusOK}
	}

	if ctxErr != nil || writeErr != nil {
		msg := "下游客户端提前断开"
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			msg = "下游请求上下文超时"
		case writeErr != nil:
			msg = fmt.Sprintf("写回下游失败: %v", writeErr)
		case ctxErr != nil:
			msg = fmt.Sprintf("下游请求提前取消: %v", ctxErr)
		}
		return streamOutcome{
			logStatusCode:  logStatusClientClosed,
			failureMessage: msg,
		}
	}

	if readErr != nil {
		kind := classifyTransportFailure(readErr)
		if kind == "" {
			kind = "transport"
		}
		return streamOutcome{
			logStatusCode:  logStatusUpstreamStreamBreak,
			failureKind:    kind,
			failureMessage: fmt.Sprintf("上游流读取失败: %v", readErr),
			penalize:       true,
		}
	}

	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "上游流提前结束，未收到终止事件",
		penalize:       true,
	}
}

func shouldTransparentRetryStream(outcome streamOutcome, attempt int, maxRetries int, wroteAnyBody bool, ctxErr, writeErr error) bool {
	if attempt >= maxRetries {
		return false
	}
	if !outcome.penalize {
		return false
	}
	if wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	return true
}

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	auth := h.authMiddleware()

	// /v1 前缀路由（标准路径）
	v1 := r.Group("/v1")
	v1.Use(auth)
	v1.POST("/chat/completions", h.ChatCompletions)
	v1.POST("/responses", h.Responses)
	v1.POST("/responses/compact", h.ResponsesCompact)
	v1.POST("/messages", h.Messages)
	v1.POST("/images/generations", h.ImagesGenerations)
	v1.GET("/models", h.ListModels)

	// 无前缀路由（兼容 base_url 已包含 /v1 的客户端）
	r.POST("/chat/completions", auth, h.ChatCompletions)
	r.POST("/responses", auth, h.Responses)
	r.POST("/responses/compact", auth, h.ResponsesCompact)
	r.POST("/messages", auth, h.Messages)
	r.POST("/images/generations", auth, h.ImagesGenerations)
	r.GET("/models", auth, h.ListModels)
}

// authMiddleware API Key 鉴权中间件（增强版，带安全日志）
func (h *Handler) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果没有配置任何密钥，跳过鉴权
		if !h.hasAnyKeys() {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		// 兼容 Anthropic 客户端的多种认证方式:
		// - x-api-key: Anthropic SDK 默认方式
		// - ANTHROPIC_AUTH_TOKEN: Claude Code 通过此环境变量设置，
		//   实际发送为 Authorization: Bearer <token>（已被上面覆盖）
		//   或 anthropic-auth-token 自定义 header
		if authHeader == "" {
			for _, h := range []string{"x-api-key", "anthropic-auth-token"} {
				if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
					authHeader = "Bearer " + v
					break
				}
			}
		}
		if authHeader == "" {
			// Use standardized error format from api package
			api.SendError(c, api.ErrMissingAPIKey)
			c.Abort()
			return
		}

		// 清理输入
		authHeader = security.SanitizeInput(authHeader)

		key := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		apiKeyRow, ok := h.resolveAPIKey(key)
		if !ok {
			// 记录安全审计日志（脱敏）
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			// Use standardized error format from api package
			api.SendError(c, api.ErrInvalidAPIKey)
			c.Abort()
			return
		}
		c.Set(contextAPIKeyID, apiKeyRow.ID)
		c.Set(contextAPIKeyName, strings.TrimSpace(apiKeyRow.Name))
		c.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKeyRow.Key))
		c.Set("apiKey", key)
		c.Next()
	}
}

// ==================== /v1/responses ====================

// drawingResponseModelAlias 虚拟模型命中（画图请求）时响应体中对外展示的统一模型名。
// 不管客户端选的是哪个 gpt-draw-* 虚拟模型，也不管 base_model 是 gpt-5.4-mini
// 还是别的，对外统一显示为 gpt-5.4，避免泄露内部实现。
const drawingResponseModelAlias = "gpt-5.4"

// applyVirtualModel 在请求体进入校验/翻译流程之前，识别并改写虚拟模型请求。
// 如果 body 里的 model 命中虚拟模型配置，则替换为 base_model 并合并 inject 字段。
// 未命中或未配置时返回原 body。
// 第二个返回值是 hit 的虚拟模型配置（nil 表示未命中），供调用者判断是否是画图请求。
func (h *Handler) applyVirtualModel(rawBody []byte) ([]byte, *ModelOverride) {
	overrides := ParseModelOverrides(h.store.GetModelPayloadOverrides())
	if len(overrides) == 0 {
		return rawBody, nil
	}
	return ApplyModelOverride(rawBody, overrides)
}

// responseModelFor 选响应体中对外展示的 model 名。
// 命中虚拟模型（画图）→返回 drawingResponseModelAlias；否则返回原始 model。
func responseModelFor(model string, hit *ModelOverride) string {
	if hit != nil {
		return drawingResponseModelAlias
	}
	return model
}

// rewriteResponseModelIfDrawing 在画图场景下（hit != nil）把 JSON 数据中指定 path
// 的 model 字段原地改写为 drawingResponseModelAlias。其他情况原样返回。
// path 支持 sjson 语法，如 "model" 或 "response.model"。
func rewriteResponseModelIfDrawing(data []byte, hit *ModelOverride, path string) []byte {
	if hit == nil || len(data) == 0 {
		return data
	}
	if !gjson.GetBytes(data, path).Exists() {
		return data
	}
	if rewritten, err := sjson.SetBytes(data, path, drawingResponseModelAlias); err == nil {
		return rewritten
	}
	return data
}

// applyUpstreamModelOverride 隐藏参数：如果 body 中 metadata.upstream_model 非空，
// 就把 body.model 直接覆盖为该值，并从 metadata 里删除此字段。用于客户端单次临时
// 切换上游模型名，绕过虚拟模型的 base_model 绑定（高级用户功能）。
//
// 与虚拟模型 inject 的关系：
//   - 先执行 applyVirtualModel（若命中则把 model 换成 base_model、合并 inject）
//   - 再执行本函数（若 metadata.upstream_model 存在则覆盖 model）
//
// 覆盖后的 model 仍会走 validator（ModelValidator(SupportedModels)），
// 防止注入非法模型名。
func applyUpstreamModelOverride(rawBody []byte) []byte {
	if len(rawBody) == 0 {
		return rawBody
	}
	overrideModel := gjson.GetBytes(rawBody, "metadata.upstream_model").String()
	if overrideModel == "" {
		return rawBody
	}

	out, err := sjson.SetBytes(rawBody, "model", overrideModel)
	if err != nil {
		return rawBody
	}
	if cleaned, err := sjson.DeleteBytes(out, "metadata.upstream_model"); err == nil {
		out = cleaned
	}
	// 若 metadata 变成空对象 → 连同字段一并删除，避免污染上游
	if md := gjson.GetBytes(out, "metadata"); md.IsObject() && len(md.Map()) == 0 {
		if cleaned, err := sjson.DeleteBytes(out, "metadata"); err == nil {
			out = cleaned
		}
	}
	return out
}

// getMaxRetries 从 store 读取可配置的最大重试次数
func (h *Handler) getMaxRetries() int {
	return h.store.GetMaxRetries()
}

const (
	logStatusClientClosed        = 499
	logStatusUpstreamStreamBreak = 598
)

// isRetryableStatus 检查是否可重试的上游状态码
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable || code == http.StatusUnauthorized || code == http.StatusInternalServerError
}

// isUpstreamToolNotSupported 判断上游 400 是否因"当前账号不支持请求里的工具"导致
// 典型场景：image_generation 工具只在 ChatGPT Plus / Team 订阅的 Codex 通道可用，
// free 账号会返回 "Tool choice 'image_generation' not found in 'tools' parameter."
// 这类错误不是请求问题，应该换下一个账号重试。
func isUpstreamToolNotSupported(statusCode int, errBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	return bytes.Contains(errBody, []byte("Tool choice")) &&
		bytes.Contains(errBody, []byte("not found in 'tools' parameter"))
}

func parseUsageLimitDetails(body []byte) (usageLimitDetails, bool) {
	if len(body) == 0 {
		return usageLimitDetails{}, false
	}
	if gjson.GetBytes(body, "error.type").String() != "usage_limit_reached" {
		return usageLimitDetails{}, false
	}
	return usageLimitDetails{
		message:         gjson.GetBytes(body, "error.message").String(),
		planType:        gjson.GetBytes(body, "error.plan_type").String(),
		resetsAt:        gjson.GetBytes(body, "error.resets_at").Int(),
		resetsInSeconds: gjson.GetBytes(body, "error.resets_in_seconds").Int(),
	}, true
}

// Responses 处理 /v1/responses 请求（原生透传，增强输入验证）
func (h *Handler) Responses(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// 识别并改写虚拟模型（命中时替换 model 为 base_model 并合并 inject 字段）
	rawBody, virtualHit := h.applyVirtualModel(rawBody)
	// 隐藏参数：metadata.upstream_model 直接覆盖上游 model 名
	rawBody = applyUpstreamModelOverride(rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(SupportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}

	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody) // 先捕获原始值用于日志审计
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}
	rawBody = stripDisabledFastAlias(rawBody, h.store.GetFastAliasEnabled())

	// 2. 准备上游请求体（Unmarshal→map→Marshal，一次序列化）
	codexBody, expandedInputRaw := PrepareResponsesBody(rawBody)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool) // 重试时排除已失败的账号
	preferPlan := h.planDispatch(model, rawBody, virtualHit, excludeAccounts)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithPreference(sessionID, excludeAccounts, preferPlan)
		if account == nil {
			// 排队等待可用账号（最多 30s）
			account, stickyProxyURL = h.store.WaitForSessionAvailable(c.Request.Context(), sessionID, 30*time.Second, excludeAccounts)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := stickyProxyURL
		if proxyURL == "" {
			proxyURL = h.store.NextProxy()
		}
		useWebsocket := h.cfg != nil && h.cfg.UseWebsocket

		// 提取 API Key 用于设备指纹稳定化
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// 使用注入的设备指纹配置
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // 默认关闭
			}
		}

		// 透传下游请求头用于指纹学习
		downstreamHeaders := c.Request.Header.Clone()

		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(sessionID, account.ID())
			excludeAccounts[account.ID()] = true

			// 不可重试的结构化错误直接返回
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(sessionID, account.ID())
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, model, account.ID(), errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/responses",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/responses",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if (isRetryableStatus(resp.StatusCode) || isUpstreamToolNotSupported(resp.StatusCode, errBody) || isDeactivatedWorkspace(errBody)) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！透传响应并跟踪 TTFT / usage
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBody := false
		var responseJSON []byte
		var capacityErrMsg string // 上游 response.failed 携带的容量错误，用于触发透明重试
		var lastFailedErrMsg string // 上游 response.failed 的 error.message（debug 用，不论是否 capacity）

		if isStream {
			// 流式透传 + TTFT 跟踪
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// 容量错误透明重试：首包前上游报 "at capacity"，吞掉该事件不转发
				if eventType == "response.failed" {
					lastFailedErrMsg = extractResponseFailedErrMsg(data)
					if !wroteAnyBody && isCapacityError(lastFailedErrMsg) {
						capacityErrMsg = lastFailedErrMsg
						gotTerminal = true
						return false
					}
				}

				// TTFT: 记录第一个 output_text.delta 事件的时间
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}

				// 提取 usage + service_tier
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				// 画图场景下将 SSE 事件里的 response.model 改为 gpt-5.4
				dataToWrite := rewriteResponseModelIfDrawing(data, virtualHit, "response.model")
				if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", dataToWrite); err != nil {
					writeErr = err
					return false
				}
				wroteAnyBody = true
				flusher.Flush()
				return eventType != "response.completed" && eventType != "response.failed"
			})
		} else {
			// 非流式收集
			var lastResponseData []byte
			// Bug 2 修复：image_generation_call 的 result 在 Codex 流式响应里
			// 仅通过 response.output_item.done 事件下发；非流式客户端需要
			// 把这些 item 合并回 response.completed.response.output[]。
			imageItemsByID := make(map[string]json.RawMessage)
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if eventType == "response.output_item.done" {
					if parsed.Get("item.type").String() == "image_generation_call" {
						if id := parsed.Get("item.id").String(); id != "" {
							imageItemsByID[id] = json.RawMessage(parsed.Get("item").Raw)
						}
					}
				}
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
					lastResponseData = data
					return false
				}
				if eventType == "response.failed" {
					lastFailedErrMsg = extractResponseFailedErrMsg(data)
					if isCapacityError(lastFailedErrMsg) {
						capacityErrMsg = lastFailedErrMsg
					}
					gotTerminal = true
					lastResponseData = data
					return false
				}
				return true
			})

			if lastResponseData != nil {
				responseObj := gjson.GetBytes(lastResponseData, "response")
				if responseObj.Exists() {
					responseJSON = []byte(responseObj.Raw)
					responseJSON = MergeImageItemsIntoResponse(responseJSON, imageItemsByID)
					// 画图场景对外统一显示 gpt-5.4
					responseJSON = rewriteResponseModelIfDrawing(responseJSON, virtualHit, "model")
				}
			}
		}

		// 容量错误透明重试：首包前上游报 "at capacity" → 换账号重试，不冷却原账号。
		// continue 后若 attempt 耗尽，会自然跳出 loop 走 "所有重试都失败" 分支返 502。
		if capacityErrMsg != "" && !wroteAnyBody {
			log.Printf("上游返回容量错误，透明重试到其他账号 (attempt %d/%d, account %d, /v1/responses): %s",
				attempt+1, maxRetries+1, account.ID(), capacityErrMsg)
			resp.Body.Close()
			h.store.Release(account)
			excludeAccounts[account.ID()] = true
			lastErr = errors.New(capacityErrMsg)
			continue
		}

		// 0-token debug 日志：gotTerminal 但完全没 delta 的异常响应（用于定位上游隐蔽错误）
		if gotTerminal && deltaCharCount == 0 && capacityErrMsg == "" {
			log.Printf("[0-token] account=%d model=%s stream=%t duration=%dms failed_errmsg=%q /v1/responses",
				account.ID(), model, isStream, int(time.Since(start).Milliseconds()), truncateForLog(lastFailedErrMsg, 200))
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/responses): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClientForAccount(account)
			SyncCodexUsageState(h.store, account, resp)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}

		h.store.BindSessionAffinity(sessionID, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/responses, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3 // 粗略估算: 约 3 字符 = 1 token
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if !isStream {
			if responseJSON != nil {
				c.Data(http.StatusOK, "application/json", responseJSON)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/responses",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/responses",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		SyncCodexUsageState(h.store, account, resp)
		if outcome.penalize {
			recyclePooledClientForAccount(account)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

// ResponsesCompact 处理 /v1/responses/compact 请求（非流式压缩接口，透传到上游 /responses/compact）
func (h *Handler) ResponsesCompact(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// 识别并改写虚拟模型（命中时替换 model 为 base_model 并合并 inject 字段）
	rawBody, virtualHit := h.applyVirtualModel(rawBody)
	// 隐藏参数：metadata.upstream_model 直接覆盖上游 model 名
	rawBody = applyUpstreamModelOverride(rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(SupportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}
	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}
	rawBody = stripDisabledFastAlias(rawBody, h.store.GetFastAliasEnabled())

	// compact 强制非流式
	rawBody, _ = sjson.SetBytes(rawBody, "stream", false)

	// 准备上游请求体
	codexBody, _ := PrepareCompactResponsesBody(rawBody)

	// 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)
	preferPlan := h.planDispatch(model, rawBody, virtualHit, excludeAccounts)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithPreference(sessionID, excludeAccounts, preferPlan)
		if account == nil {
			account, stickyProxyURL = h.store.WaitForSessionAvailable(c.Request.Context(), sessionID, 30*time.Second, excludeAccounts)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := stickyProxyURL
		if proxyURL == "" {
			proxyURL = h.store.NextProxy()
		}

		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()

		resp, reqErr := ExecuteCompactRequest(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(sessionID, account.ID())
			excludeAccounts[account.ID()] = true

			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("compact 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(sessionID, account.ID())
			excludeAccounts[account.ID()] = true

			logUpstreamError("/v1/responses/compact", resp.StatusCode, model, account.ID(), errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/responses/compact",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/responses/compact",
				UpstreamEndpoint: "/v1/responses/compact",
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if (isRetryableStatus(resp.StatusCode) || isUpstreamToolNotSupported(resp.StatusCode, errBody) || isDeactivatedWorkspace(errBody)) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功：直接透传响应体
		SyncCodexUsageState(h.store, account, resp)

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 提取 usage 用于日志
		promptTokens := int(gjson.GetBytes(respBody, "usage.input_tokens").Int())
		completionTokens := int(gjson.GetBytes(respBody, "usage.output_tokens").Int())
		totalTokens := int(gjson.GetBytes(respBody, "usage.total_tokens").Int())
		reasoningTokens := int(gjson.GetBytes(respBody, "usage.output_tokens_details.reasoning_tokens").Int())
		cachedTokens := int(gjson.GetBytes(respBody, "usage.input_tokens_details.cached_tokens").Int())

		actualServiceTier := gjson.GetBytes(respBody, "service_tier").String()
		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)

		totalDuration := int(time.Since(start).Milliseconds())
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/responses/compact",
			Model:            model,
			StatusCode:       http.StatusOK,
			DurationMs:       totalDuration,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			InputTokens:      promptTokens,
			OutputTokens:     completionTokens,
			ReasoningTokens:  reasoningTokens,
			CachedTokens:     cachedTokens,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/responses/compact",
			UpstreamEndpoint: "/v1/responses/compact",
			ServiceTier:      resolvedServiceTier,
		})

		h.store.Release(account)
		// 画图场景对外统一显示 gpt-5.4
		respBody = rewriteResponseModelIfDrawing(respBody, virtualHit, "model")
		c.Data(http.StatusOK, "application/json", respBody)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

func (h *Handler) ChatCompletions(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// 识别并改写虚拟模型（命中时替换 model 为 base_model 并合并 inject 字段）
	rawBody, virtualHit := h.applyVirtualModel(rawBody)
	// 隐藏参数：metadata.upstream_model 直接覆盖上游 model 名
	rawBody = applyUpstreamModelOverride(rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(SupportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		model = "gpt-5.4"
	}

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}
	rawBody = stripDisabledFastAlias(rawBody, h.store.GetFastAliasEnabled())

	// 2. 翻译请求：OpenAI Chat → Codex Responses
	codexBody, err := TranslateRequest(rawBody)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request translation failed: "+err.Error(), api.ErrorTypeInvalidRequest))
		return
	}

	sessionID := ResolveSessionID(c.Request.Header, codexBody)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool) // 重试时排除已失败的账号
	preferPlan := h.planDispatch(model, rawBody, virtualHit, excludeAccounts)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithPreference(sessionID, excludeAccounts, preferPlan)
		if account == nil {
			// 排队等待可用账号（最多 30s）
			account, stickyProxyURL = h.store.WaitForSessionAvailable(c.Request.Context(), sessionID, 30*time.Second, excludeAccounts)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := stickyProxyURL
		if proxyURL == "" {
			proxyURL = h.store.NextProxy()
		}
		useWebsocket := h.cfg != nil && h.cfg.UseWebsocket

		// 提取 API Key 用于设备指纹稳定化
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// 使用注入的设备指纹配置
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // 默认关闭
			}
		}

		// 透传下游请求头用于指纹学习
		downstreamHeaders := c.Request.Header.Clone()

		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(sessionID, account.ID())
			excludeAccounts[account.ID()] = true

			// 不可重试的结构化错误直接返回
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(sessionID, account.ID())
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, model, account.ID(), errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/chat/completions",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/chat/completions",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if (isRetryableStatus(resp.StatusCode) || isUpstreamToolNotSupported(resp.StatusCode, errBody) || isDeactivatedWorkspace(errBody)) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！翻译响应 + TTFT 跟踪
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBody := false
		var compactResult []byte
		var capacityErrMsg string // 上游 response.failed 携带的容量错误，用于触发透明重试
		var lastFailedErrMsg string // 上游 response.failed 的 error.message（debug 用，不论是否 capacity）

		chunkID := "chatcmpl-" + uuid.New().String()[:8]
		created := time.Now().Unix()

		// 画图请求响应体中的 model 统一显示为 gpt-5.4（不泄露 base_model）
		responseModel := responseModelFor(model, virtualHit)

		if isStream {
			streamTranslator := NewStreamTranslator(chunkID, responseModel, created)
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// 容量错误透明重试：若流尚未向下游写入任何字节，且上游报 response.failed
				// 携带 "at capacity"/"try a different mode" 类错误，则吞掉该事件不翻译、
				// 不写客户端，让流结束后触发 continue 换账号重试（不冷却原账号）。
				if eventType == "response.failed" {
					lastFailedErrMsg = extractResponseFailedErrMsg(data)
					if !wroteAnyBody && isCapacityError(lastFailedErrMsg) {
						capacityErrMsg = lastFailedErrMsg
						gotTerminal = true
						return false
					}
				}

				chunk, done := streamTranslator.Translate(data)

				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数（文本 + function call 参数）
				if eventType == "response.output_text.delta" || eventType == "response.function_call_arguments.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				if chunk != nil {
					if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", chunk); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBody = true
					flusher.Flush()
				}
				if done {
					if _, err := fmt.Fprintf(c.Writer, "data: [DONE]\n\n"); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBody = true
					flusher.Flush()
					return false
				}
				return true
			})
		} else {
			var fullContent strings.Builder
			var toolCalls []ToolCallResult

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				switch eventType {
				case "response.output_text.delta":
					delta := parsed.Get("delta").String()
					deltaCharCount += len(delta)
					fullContent.WriteString(delta)
				case "response.function_call_arguments.delta":
					deltaCharCount += len(parsed.Get("delta").String())
				case "response.output_item.done":
					// Bug 2 修复：image_generation_call 终稿出现在 output_item.done 里，
					// 非流式模式下把 base64 转成 markdown image URL 拼进 content。
					if parsed.Get("item.type").String() == "image_generation_call" {
						if b64 := parsed.Get("item.result").String(); b64 != "" {
							fullContent.WriteString(imageMarkdownFromBase64(b64))
						}
					}
				case "response.completed":
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 从 response.output 提取 function_call 项
					toolCalls = ExtractToolCallsFromOutput(data)
					gotTerminal = true
					return false
				case "response.failed":
					lastFailedErrMsg = extractResponseFailedErrMsg(data)
					if isCapacityError(lastFailedErrMsg) {
						capacityErrMsg = lastFailedErrMsg
					}
					gotTerminal = true
					return false
				}
				return true
			})

			compactResult = BuildCompactResponse(chunkID, responseModel, created, fullContent.String(), toolCalls, usage)
		}

		// 容量错误透明重试：首包前上游报 "at capacity" → 换账号重试，不冷却原账号。
		// continue 后若 attempt 耗尽，会自然跳出 loop 走 "所有重试都失败" 分支返 502。
		if capacityErrMsg != "" && !wroteAnyBody {
			log.Printf("上游返回容量错误，透明重试到其他账号 (attempt %d/%d, account %d, /v1/chat/completions): %s",
				attempt+1, maxRetries+1, account.ID(), capacityErrMsg)
			resp.Body.Close()
			h.store.Release(account)
			excludeAccounts[account.ID()] = true
			lastErr = errors.New(capacityErrMsg)
			continue
		}

		// 0-token debug 日志：gotTerminal 但完全没 delta 的异常响应（用于定位上游隐蔽错误）
		if gotTerminal && deltaCharCount == 0 && capacityErrMsg == "" {
			log.Printf("[0-token] account=%d model=%s stream=%t duration=%dms failed_errmsg=%q /v1/chat/completions",
				account.ID(), model, isStream, int(time.Since(start).Milliseconds()), truncateForLog(lastFailedErrMsg, 200))
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/chat/completions): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClientForAccount(account)
			SyncCodexUsageState(h.store, account, resp)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}

		h.store.BindSessionAffinity(sessionID, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/chat/completions, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if !isStream {
			if compactResult != nil {
				c.Data(http.StatusOK, "application/json", compactResult)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		SyncCodexUsageState(h.store, account, resp)
		if outcome.penalize {
			recyclePooledClientForAccount(account)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

// handleStreamResponse 处理流式响应（翻译 Codex → OpenAI）
func (h *Handler) handleStreamResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": "streaming not supported", "type": "server_error"},
		})
		return
	}

	err := ReadSSEStream(body, func(data []byte) bool {
		chunk, done := TranslateStreamChunk(data, model, chunkID, created)
		if chunk != nil {
			fmt.Fprintf(c.Writer, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		if done {
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			flusher.Flush()
			return false
		}
		return true
	})

	if err != nil {
		log.Printf("读取上游流失败: %v", err)
	}
}

// handleCompactResponse 处理非流式响应
func (h *Handler) handleCompactResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	var fullContent strings.Builder
	var usage *UsageInfo

	_ = ReadSSEStream(body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			fullContent.WriteString(delta)
		case "response.completed":
			usage = extractUsage(data)
			return false
		case "response.failed":
			return false
		}
		return true
	})

	result := BuildCompactResponse(chunkID, model, created, fullContent.String(), nil, usage)

	c.Data(http.StatusOK, "application/json", result)
}

// ==================== 通用辅助 ====================

// parseRetryAfter 解析上游 429 响应中的重试时间（参考 CLIProxyAPI codex_executor.go:689-708）
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 2 * time.Minute
	}

	// 解析 error.resets_at (Unix timestamp)
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			d := time.Until(resetTime)
			if d > 0 {
				return d
			}
		}
	}

	// 解析 error.resets_in_seconds
	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return time.Duration(secs) * time.Second
	}

	// 默认 2 分钟
	return 2 * time.Minute
}

// IsDeactivatedWorkspace 判断上游 402 响应体是否来自 workspace 被停用。
// 导出版本，供 admin 测试路径复用。
// Codex 对已停用的 team/enterprise workspace 返回:
//   - {"detail":{"code":"deactivated_workspace"}}
//   - {"error":{"code":"deactivated_workspace"}}
// 该账号对任何 API 请求都不可恢复，应永久禁用并换号重试。
func IsDeactivatedWorkspace(body []byte) bool {
	return isDeactivatedWorkspace(body)
}

// isDeactivatedWorkspace 内部实现。
func isDeactivatedWorkspace(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "detail.code").String()), "deactivated_workspace") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()), "deactivated_workspace") {
		return true
	}
	return false
}

func isMissingScopeUnauthorized(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	if code != "missing_scope" {
		return false
	}

	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "api.responses.write") {
		return true
	}

	return strings.Contains(msg, "scope")
}

func parseRetryAfterResetAt(body []byte, now time.Time) (time.Time, bool) {
	if len(body) == 0 {
		return time.Time{}, false
	}

	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(now) {
			return resetTime, true
		}
	}

	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return now.Add(time.Duration(secs) * time.Second), true
	}

	return time.Time{}, false
}

func codexWindowType(windowMinutes float64) codexRateLimitWindow {
	switch {
	case windowMinutes >= 1440:
		return codexRateLimitWindow7d
	case windowMinutes >= 60:
		return codexRateLimitWindow5h
	case windowMinutes > 0:
		return codexRateLimitWindowShort
	default:
		return codexRateLimitWindowUnknown
	}
}

type codexWindowUsage struct {
	usedPct   float64
	resetSec  float64
	windowMin float64
	valid     bool
}

func parseCodexWindowUsage(usedStr, windowStr, resetStr string) codexWindowUsage {
	if usedStr == "" {
		return codexWindowUsage{}
	}
	return codexWindowUsage{
		usedPct:   parseFloat(usedStr),
		windowMin: parseFloat(windowStr),
		resetSec:  parseFloat(resetStr),
		valid:     true,
	}
}

func classifyCodex429Window(resp *http.Response, now time.Time) (codexRateLimitWindow, time.Time, bool) {
	if resp == nil {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		resp.Header.Get("x-codex-primary-reset-after-seconds"),
	)
	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		resp.Header.Get("x-codex-secondary-reset-after-seconds"),
	)

	var exhausted []codexWindowUsage
	if primary.valid && primary.usedPct >= 100 {
		exhausted = append(exhausted, primary)
	}
	if secondary.valid && secondary.usedPct >= 100 {
		exhausted = append(exhausted, secondary)
	}
	if len(exhausted) == 0 {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	chosen := exhausted[0]
	for _, candidate := range exhausted[1:] {
		if candidate.windowMin > chosen.windowMin {
			chosen = candidate
		}
	}

	var resetAt time.Time
	if chosen.resetSec > 0 {
		resetAt = now.Add(time.Duration(chosen.resetSec) * time.Second)
	}
	return codexWindowType(chosen.windowMin), resetAt, !resetAt.IsZero()
}

func responseHasCodex5hHeaders(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		resp.Header.Get("x-codex-primary-reset-after-seconds"),
	)
	if primary.valid && codexWindowType(primary.windowMin) == codexRateLimitWindow5h {
		return true
	}

	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		resp.Header.Get("x-codex-secondary-reset-after-seconds"),
	)
	return secondary.valid && codexWindowType(secondary.windowMin) == codexRateLimitWindow5h
}

func classify429RateLimit(account *auth.Account, body []byte, resp *http.Response, now time.Time) codex429Decision {
	if account != nil && account.IsPremium5hPlan() {
		windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
		exactResetAt, hasExactReset := parseRetryAfterResetAt(body, now)

		switch windowType {
		case codexRateLimitWindow5h:
			if hasExactReset {
				resetAt = exactResetAt
			} else if !hasWindowReset {
				resetAt = now.Add(5 * time.Hour)
			}
			return codex429Decision{
				Premium5h: true,
				ResetAt:   resetAt,
				Cooldown:  time.Until(resetAt),
			}
		case codexRateLimitWindow7d, codexRateLimitWindowShort:
			// 明确不是 5h 窗口时，保持原有 cooldown 语义。
		default:
			if hasExactReset {
				return codex429Decision{
					Premium5h: true,
					ResetAt:   exactResetAt,
					Cooldown:  time.Until(exactResetAt),
				}
			}
			resetAt = now.Add(5 * time.Hour)
			return codex429Decision{
				Premium5h: true,
				ResetAt:   resetAt,
				Cooldown:  5 * time.Hour,
			}
		}
	}

	cooldown := compute429Cooldown(account, body, resp)
	return codex429Decision{Cooldown: cooldown}
}

// Apply429Cooldown 统一处理 429 对账号状态的影响，premium 5h 场景优先写入显式限流态。
func Apply429Cooldown(store *auth.Store, account *auth.Account, body []byte, resp *http.Response) codex429Decision {
	decision := classify429RateLimit(account, body, resp, time.Now())
	if store == nil || account == nil {
		return decision
	}
	if decision.Premium5h {
		store.MarkPremium5hRateLimited(account, decision.ResetAt)
		return decision
	}
	store.MarkCooldown(account, decision.Cooldown, "rate_limited")
	return decision
}

// applyCooldown 根据上游状态码设置智能冷却
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	switch statusCode {
	case http.StatusTooManyRequests:
		decision := Apply429Cooldown(h.store, account, body, resp)
		if decision.Premium5h {
			log.Printf("账号 %d 触发 premium 5h 限流 (plan=%s)，重置时间 %s", account.ID(), account.GetPlanType(), decision.ResetAt.Format(time.RFC3339))
			return
		}
		log.Printf("账号 %d 被限速 (plan=%s)，冷却 %v", account.ID(), account.GetPlanType(), decision.Cooldown)
	case http.StatusUnauthorized:
		// 原子标志瞬间置位，阻止其他并发请求再选到该账号
		atomic.StoreInt32(&account.Disabled, 1)

		if isMissingScopeUnauthorized(body) {
			log.Printf("账号 %d 收到 missing_scope 401，保留在号池", account.ID())
			atomic.StoreInt32(&account.Disabled, 0)
			return
		}

		if h.store.GetAutoCleanUnauthorized() {
			// 开启自动清理时，401 立即从号池删除
			log.Printf("账号 %d 收到 401，立即清理", account.ID())
			if h.db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = h.db.SetError(ctx, account.ID(), "deleted")
				cancel()
				h.db.InsertAccountEventAsync(account.ID(), "deleted", "auto_clean_401")
			}
			h.store.RemoveAccount(account.ID())
		} else {
			h.store.MarkCooldown(account, 5*time.Minute, "unauthorized")
		}
	case http.StatusPaymentRequired:
		// 402 目前只处理已知的 deactivated_workspace 语义：
		// workspace 被 OpenAI 停用后所有请求都会命中，账号永久不可恢复。
		// 其它未知 402 暂不改变账号状态，仅靠本次请求的 excludeAccounts 换号。
		if !isDeactivatedWorkspace(body) {
			return
		}
		atomic.StoreInt32(&account.Disabled, 1)
		if h.store.GetAutoCleanUnauthorized() {
			log.Printf("账号 %d 收到 402 deactivated_workspace，立即清理", account.ID())
			if h.db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = h.db.SetError(ctx, account.ID(), "deleted")
				cancel()
				h.db.InsertAccountEventAsync(account.ID(), "deleted", "auto_clean_deactivated_workspace")
			}
			h.store.RemoveAccount(account.ID())
		} else {
			log.Printf("账号 %d 收到 402 deactivated_workspace，标记为 banned", account.ID())
			h.store.MarkCooldown(account, 7*24*time.Hour, "deactivated_workspace")
		}
	}
}

// compute429Cooldown 根据计划类型和 Codex 响应精确计算 429 冷却时间
func (h *Handler) compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	return compute429Cooldown(account, body, resp)
}

func compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	// 1. 优先使用 Codex 响应体中的精确重置时间
	if resetDuration := parseRetryAfter(body); resetDuration > 2*time.Minute {
		// parseRetryAfter 默认返回 2min（无数据），超过 2min 说明解析到了真实的 resets_at/resets_in_seconds
		if resetDuration > 7*24*time.Hour {
			resetDuration = 7 * 24 * time.Hour // 最多 7 天
		}
		return resetDuration
	}

	// 2. 没有精确重置时间，根据套餐类型 + 用量窗口推断
	planType := strings.ToLower(account.GetPlanType())

	switch planType {
	case "free":
		// Free 只有 7d 窗口，429 = 额度耗尽，冷却 7 天
		return 7 * 24 * time.Hour

	case "team", "teamplus", "pro", "plus", "enterprise":
		// Team/Pro/Plus 有 5h + 7d 双窗口，需要判断是哪个窗口触发了限制
		return detectTeamCooldownWindow(resp)

	default:
		// 未知套餐，保守默认 5 小时
		return 5 * time.Hour
	}
}

// detectTeamCooldownWindow 通过响应头判断 Team/Pro/Plus 账号是哪个窗口触发的限制
func (h *Handler) detectTeamCooldownWindow(resp *http.Response) time.Duration {
	return detectTeamCooldownWindow(resp)
}

func detectTeamCooldownWindow(resp *http.Response) time.Duration {
	if resp == nil {
		return 5 * time.Hour // 保守默认
	}

	// Codex 返回两组窗口头：primary 和 secondary
	// x-codex-primary-window-minutes / x-codex-primary-used-percent
	// x-codex-secondary-window-minutes / x-codex-secondary-used-percent
	// 用量 >= 100% 的窗口就是触发限制的窗口

	primaryUsed := parseFloat(resp.Header.Get("x-codex-primary-used-percent"))
	primaryWindowMin := parseFloat(resp.Header.Get("x-codex-primary-window-minutes"))
	secondaryUsed := parseFloat(resp.Header.Get("x-codex-secondary-used-percent"))
	secondaryWindowMin := parseFloat(resp.Header.Get("x-codex-secondary-window-minutes"))

	// 找到 used >= 100% 的窗口
	primaryExhausted := primaryUsed >= 100
	secondaryExhausted := secondaryUsed >= 100

	switch {
	case primaryExhausted && secondaryExhausted:
		// 两个窗口都满了，取较大窗口的冷却时间
		return windowMinutesToCooldown(max(primaryWindowMin, secondaryWindowMin))
	case primaryExhausted:
		return windowMinutesToCooldown(primaryWindowMin)
	case secondaryExhausted:
		return windowMinutesToCooldown(secondaryWindowMin)
	default:
		// 都没满但还是 429，可能是短时 burst 限制
		return 5 * time.Hour
	}
}

// windowMinutesToCooldown 根据窗口分钟数决定冷却时长
func windowMinutesToCooldown(windowMinutes float64) time.Duration {
	switch {
	case windowMinutes >= 1440: // >= 1 天 → 7d 窗口
		return 7 * 24 * time.Hour
	case windowMinutes >= 60: // >= 1 小时 → 5h 窗口
		return 5 * time.Hour
	default:
		return 30 * time.Minute // 短窗口
	}
}

// SyncCodexUsageState 解析 Codex 响应头并完成 7d / 5h 快照持久化与 premium 5h 提前限流。
func SyncCodexUsageState(store *auth.Store, account *auth.Account, resp *http.Response) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || resp == nil {
		return result
	}

	result.Used5hHeaders = responseHasCodex5hHeaders(resp)
	result.UsagePct7d, result.HasUsage7d = parseCodexUsageHeaders(resp, account)
	// 5h 响应头仅会出现在 Plus/Pro/Team 账号上。若 plan_type 为空（如 AT 来自非 Codex CLI 客户端，
	// JWT 中不携带 chatgpt_plan_type），回退推断为 plus，避免 UI 显示 "-" 且调度器无法识别 premium。
	if store != nil && result.Used5hHeaders {
		store.InferPremiumPlanFromHeaders(account)
	}
	if store != nil {
		if result.HasUsage7d {
			store.PersistUsageSnapshot(account, result.UsagePct7d)
		} else if result.Used5hHeaders {
			store.PersistUsageSnapshot5hOnly(account)
			result.Persisted5hOnly = true
		}
	}

	result.UsagePct5h, result.Reset5hAt, result.HasUsage5h = account.GetUsageSnapshot5h()
	if result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 {
		if store != nil {
			store.MarkPremium5hRateLimited(account, result.Reset5hAt)
		}
		result.Premium5hRateLimited = true
	}

	return result
}

// parseCodexUsageHeaders 从 Codex 响应头解析 5h/7d 用量百分比
func parseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	if resp == nil {
		return 0, false
	}

	// 解析 primary 和 secondary 窗口
	primaryUsedStr := resp.Header.Get("x-codex-primary-used-percent")
	primaryWindowStr := resp.Header.Get("x-codex-primary-window-minutes")
	primaryResetStr := resp.Header.Get("x-codex-primary-reset-after-seconds")
	secondaryUsedStr := resp.Header.Get("x-codex-secondary-used-percent")
	secondaryWindowStr := resp.Header.Get("x-codex-secondary-window-minutes")
	secondaryResetStr := resp.Header.Get("x-codex-secondary-reset-after-seconds")

	primary := parseCodexWindowUsage(primaryUsedStr, primaryWindowStr, primaryResetStr)
	secondary := parseCodexWindowUsage(secondaryUsedStr, secondaryWindowStr, secondaryResetStr)

	// 归一化：小窗口 (≤360min) → 5h，大窗口 (>360min) → 7d
	var w5h, w7d codexWindowUsage
	now := time.Now()

	if primary.valid && secondary.valid {
		if primary.windowMin >= secondary.windowMin {
			w7d, w5h = primary, secondary
		} else {
			w7d, w5h = secondary, primary
		}
	} else if primary.valid {
		if primary.windowMin <= 360 && primary.windowMin > 0 {
			w5h = primary
		} else {
			w7d = primary
		}
	} else if secondary.valid {
		if secondary.windowMin <= 360 && secondary.windowMin > 0 {
			w5h = secondary
		} else {
			w7d = secondary
		}
	}

	// 写入 5h
	if w5h.valid {
		resetAt := now.Add(time.Duration(w5h.resetSec) * time.Second)
		account.SetUsageSnapshot5h(w5h.usedPct, resetAt)
	}

	// 写入 7d
	if w7d.valid {
		resetAt := now.Add(time.Duration(w7d.resetSec) * time.Second)
		account.SetReset7dAt(resetAt)
		account.SetUsagePercent7d(w7d.usedPct)
		return w7d.usedPct, true
	}

	return 0, false
}

// ParseCodexUsageHeaders 从响应头提取并更新账号用量信息
func ParseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	return parseCodexUsageHeaders(resp, account)
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

// sendUpstreamError 发送上游错误响应给客户端
func (h *Handler) sendUpstreamError(c *gin.Context, statusCode int, body []byte) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("上游返回错误 (status %d): %s", statusCode, string(body)),
			"type":    "upstream_error",
			"code":    fmt.Sprintf("upstream_%d", statusCode),
		},
	})
}

// sendFinalUpstreamError 重试用尽后的最终错误响应：识别 usage_limit_reached 改写为 503，其余透传
func (h *Handler) sendFinalUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if statusCode == http.StatusTooManyRequests {
		if details, ok := parseUsageLimitDetails(body); ok {
			if details.resetsInSeconds > 0 {
				c.Header("Retry-After", fmt.Sprintf("%d", details.resetsInSeconds))
			}

			message := "账号池额度已耗尽，请稍后重试"
			if details.message != "" {
				message = fmt.Sprintf("%s：%s", message, details.message)
			}

			errInfo := gin.H{
				"message": message,
				"type":    "server_error",
				"code":    "account_pool_usage_limit_reached",
			}
			if details.planType != "" {
				errInfo["plan_type"] = details.planType
			}
			if details.resetsAt != 0 {
				errInfo["resets_at"] = details.resetsAt
			}
			if details.resetsInSeconds != 0 {
				errInfo["resets_in_seconds"] = details.resetsInSeconds
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errInfo})
			return
		}
	}

	h.sendUpstreamError(c, statusCode, body)
}

// handleUpstreamError 统一处理上游错误（兼容旧调用）
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body, nil)
	h.sendUpstreamError(c, statusCode, body)
}

// SupportedModels 支持的模型列表（全局共享）
var SupportedModels = []string{
	"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
	"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
	"gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex",
}

// premiumOnlyModels 仅对 Plus/Pro/Team 账号开放的模型。
// 上游 Codex 后端对 Free 账号直接 HTTP 400 "model is not supported"。
// 为避免浪费重试与健康分惩罚，调度时从候选池中排除 free 账号。
var premiumOnlyModels = map[string]struct{}{
	"gpt-5.5": {},
}

// IsPremiumOnlyModel 判断模型是否仅对付费账号开放。
func IsPremiumOnlyModel(model string) bool {
	_, ok := premiumOnlyModels[strings.ToLower(strings.TrimSpace(model))]
	return ok
}

// bodyRequiresPaidAccount 判断请求体是否必须由付费账号承接。
// 当前仅 image_generation 工具需要：ChatGPT Free 订阅下调用会返回
// "Tool choice 'image_generation' not found in 'tools' parameter." 400。
func bodyRequiresPaidAccount(rawBody []byte) bool {
	if len(rawBody) == 0 {
		return false
	}
	if gjson.GetBytes(rawBody, "tool_choice.type").String() == "image_generation" {
		return true
	}
	tools := gjson.GetBytes(rawBody, "tools")
	if !tools.IsArray() {
		return false
	}
	found := false
	tools.ForEach(func(_, t gjson.Result) bool {
		if t.Get("type").String() == "image_generation" {
			found = true
			return false
		}
		return true
	})
	return found
}

// planDispatch 计算一次请求的调度策略，就地写入 exclude 集并返回 preferPlan。
// 规则：
//   - 命中 premium-only 模型，或请求体（含虚拟模型 inject 之后）带 image_generation 工具
//     → 把所有 Free 账号加入 exclude，preferPlan = ""（只调度付费）
//   - 其他请求（含只 inject reasoning effort 的虚拟模型如 gpt-5.4-high）
//     → preferPlan = "free"（优先 Free，池空回退付费）
//
// 注意：这里**不**再用 `virtualHit != nil` 作为 premium 判据——虚拟模型也可能只
// inject reasoning/service_tier，Free 完全能承接。统一以 rawBody 是否含
// image_generation 作为唯一付费判据更准确。
func (h *Handler) planDispatch(model string, rawBody []byte, _ *ModelOverride, exclude map[int64]bool) string {
	needPaid := IsPremiumOnlyModel(model) || bodyRequiresPaidAccount(rawBody)
	if needPaid {
		if exclude != nil && h != nil && h.store != nil {
			for id := range h.store.AccountIDsByPlan("free") {
				exclude[id] = true
			}
		}
		return ""
	}
	return "free"
}

// ModelPlanPolicy 描述某个模型的派发策略（供前端展示）。
type ModelPlanPolicy struct {
	Plan         string   `json:"plan_policy"`   // "premium_only" / "prefer_free"
	AllowedPlans []string `json:"allowed_plans"` // 可承接该模型的 plan 列表
	PreferPlan   string   `json:"prefer_plan"`   // 优先派发的 plan；premium_only 时为空
}

// PolicyForModel 返回模型的派发策略。
func PolicyForModel(model string) ModelPlanPolicy {
	if IsPremiumOnlyModel(model) {
		return ModelPlanPolicy{
			Plan:         "premium_only",
			AllowedPlans: []string{"plus", "pro", "team"},
			PreferPlan:   "",
		}
	}
	return ModelPlanPolicy{
		Plan:         "prefer_free",
		AllowedPlans: []string{"free", "plus", "pro", "team"},
		PreferPlan:   "free",
	}
}

// ListModels 列出可用模型（含虚拟模型别名）
func (h *Handler) ListModels(c *gin.Context) {
	virtualNames := ParseModelOverrides(h.store.GetModelPayloadOverrides()).VirtualModelNames()
	models := make([]api.Model, 0, len(SupportedModels)+len(virtualNames))
	now := time.Now().Unix()
	for _, id := range SupportedModels {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "openai",
		})
	}
	for _, id := range virtualNames {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "codex2api-virtual",
		})
	}
	api.SendList(c, "list", models)
}
