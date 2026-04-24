package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// capacityErrorMarkers 上游 Codex 返回的"容量告急"错误特征关键词（小写比较）。
//
// 典型错误消息（由上游 Responses SSE 的 response.failed 事件携带）：
//
//	"Selected model is at capacity. Please try a different mode"
//	"The model you requested is at capacity..."
//
// 命中任一 marker 即判定为容量错误，允许 codex2api 对该请求做透明重试
// （换一个账号再试），前提是响应流还未向下游客户端写入任何字节。
//
// 参考：GitHub Issue openai/codex#17014 — 2026 年 4 月 gpt-5.4 区域性容量
// 紧张时期，上游经常在成功建立流后、实际生成内容前抛出 response.failed。
var capacityErrorMarkers = []string{
	"at capacity",
	"try a different mode",
	"try a different model",
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
