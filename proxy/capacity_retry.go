package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// capacityErrorMarkers 上游 Codex 返回的"瞬时可重试错误"特征关键词（小写比较）。
//
// 典型错误消息（由上游 Responses SSE 的 response.failed 事件携带）：
//
//	"Selected model is at capacity. Please try a different mode"
//	"The model you requested is at capacity..."
//	"Our servers are currently overloaded. Please try again later."
//	"An error occurred while processing your request. You can retry your request..."
//
// 命中任一 marker 即判定为可瞬时重试错误，允许 codex2api 对该请求做透明重试
// （换一个账号再试），前提是响应流还未向下游客户端写入任何字节。
//
// 历史：v1.7.51 之前只匹配 "at capacity" 家族，但生产 dialog_logs 实测显示
// codex CLI 渲染的 "Selected model is at capacity. Please try a different
// model." 其实是客户端兜底文案——上游真实文案 90% 是
// "Our servers are currently overloaded"，10% 是 "An error occurred while
// processing your request"，原 marker 一个也命中不了，导致透明重试机制完全
// 失效，错误全部漏给客户端。
//
// 参考：GitHub Issue openai/codex#17014 — 2026 年 4 月 gpt-5.4 区域性容量
// 紧张时期，上游经常在成功建立流后、实际生成内容前抛出 response.failed。
var capacityErrorMarkers = []string{
	// 容量类（OpenAI 早期文案）
	"at capacity",
	"try a different mode",
	"try a different model",
	// 服务器过载（v1.7.51 起新增，生产主要错误）
	"currently overloaded",
	"servers are currently",
	"try again later",
	// 通用瞬时错误（上游显式说 "you can retry"）
	"an error occurred while processing your request",
}

// isCapacityError 判断错误消息是否匹配"上游容量告急"特征。
// 大小写不敏感，空字符串直接返回 false。
func isCapacityError(errMsg string) bool {
	if errMsg == "" {
		return false
	}
	lower := strings.ToLower(errMsg)
	for _, m := range capacityErrorMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Upstream error kind 常量。用作 usage_logs.upstream_error_kind 字段值，
// 也是前端 UI / API 聚合分类的稳定 token（不要随便改名，前端有对应翻译）。
const (
	ErrKindOverloaded       = "overloaded"        // "Our servers are currently overloaded"
	ErrKindCapacity         = "capacity"          // "at capacity" / "try a different model" 家族
	ErrKindProcessingError  = "processing_error"  // "An error occurred while processing your request"
	ErrKindRateLimit        = "rate_limit"        // 429 / "rate limit exceeded"
	ErrKindAuth             = "auth"              // 401 / unauthorized【OpenAI 账号 token】
	ErrKindProxyAuth        = "proxy_auth"        // 407 Proxy Authentication Required【代理凭据失效】
	ErrKindContextLength    = "context_length"    // context length exceeded
	ErrKindContentFilter    = "content_filter"    // content policy / safety
	ErrKindTimeout          = "timeout"           // 上游超时
	ErrKindClientDisconnect = "client_disconnect" // 客户端断开
	ErrKindUpstream5xx      = "upstream_5xx"      // 其他 5xx
	ErrKindUpstream4xx      = "upstream_4xx"      // 其他 4xx
	ErrKindUnknown          = "unknown"           // 兜底
)

// ClassifyUpstreamError 把 response.failed 事件携带的 error.message 文本分类到
// 稳定的 kind token。空字符串返回 ""（表示无错误）。
//
// 用于 usage_logs.upstream_error_kind 字段，让前端 UI 可以稳定地按错误类型
// 聚合 / 着色 / 国际化，不依赖随时间漂移的英文文案。
func ClassifyUpstreamError(errMsg string) string {
	if errMsg == "" {
		return ""
	}
	lower := strings.ToLower(errMsg)

	switch {
	case strings.Contains(lower, "currently overloaded") ||
		strings.Contains(lower, "servers are currently"):
		return ErrKindOverloaded
	case strings.Contains(lower, "at capacity") ||
		strings.Contains(lower, "try a different mode") ||
		strings.Contains(lower, "try a different model"):
		return ErrKindCapacity
	case strings.Contains(lower, "an error occurred while processing your request"):
		return ErrKindProcessingError
	case strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests"):
		return ErrKindRateLimit
	case strings.Contains(lower, "context length") ||
		strings.Contains(lower, "maximum context") ||
		strings.Contains(lower, "context_length_exceeded"):
		return ErrKindContextLength
	case strings.Contains(lower, "content policy") ||
		strings.Contains(lower, "safety") ||
		strings.Contains(lower, "content_filter"):
		return ErrKindContentFilter
	case strings.Contains(lower, "proxy authentication") ||
		strings.Contains(lower, "407 "):
		// 代理认证错误优先于通用 auth，避免被 "authentication" 子串同化
		return ErrKindProxyAuth
	case strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "invalid auth") ||
		strings.Contains(lower, "authentication"):
		return ErrKindAuth
	case strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "timed out") ||
		strings.Contains(lower, "deadline"):
		return ErrKindTimeout
	case strings.Contains(lower, "try again later"):
		// 通用"稍后重试"——放在最后兜底，避免和上面更具体的分类抢
		return ErrKindOverloaded
	default:
		return ErrKindUnknown
	}
}

// TruncateErrMsg 截断错误消息到指定字节数，用于持久化到 usage_logs.upstream_error_msg。
// 默认 500 字节够保留完整 OpenAI 错误模板（包含 request ID 等关键信息）。
func TruncateErrMsg(errMsg string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 500
	}
	if len(errMsg) <= maxBytes {
		return errMsg
	}
	return errMsg[:maxBytes]
}

// extractResponseFailedErrMsg 从 Codex Responses SSE 的 `response.failed`
// 事件体里提取 error.message 字段。若字段缺失返回空字符串。
func extractResponseFailedErrMsg(eventData []byte) string {
	return gjson.GetBytes(eventData, "response.error.message").String()
}

// truncateForLog 截断字符串到指定长度，用于日志输出时避免过长。
// 超长时以 "…(total=N)" 结尾标明原始长度。
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…(total=" + intToStr(len(s)) + ")"
}

// intToStr 把 int 转字符串，避免引入 strconv 依赖（capacity_retry.go 目前不需要 strconv）。
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
