package proxy

import "testing"

func TestIsCapacityError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"empty", "", false},
		{"unrelated", "Internal server error", false},
		{"exact OpenAI Codex CLI message",
			"Selected model is at capacity. Please try a different mode", true},
		{"lowercase at capacity", "model is at capacity right now", true},
		{"uppercase AT CAPACITY", "AT CAPACITY, RETRY LATER", true},
		{"try a different model", "Please try a different model.", true},
		{"rate limit is NOT capacity", "Rate limit exceeded", false},
		{"quota is NOT capacity", "You exceeded your current quota", false},
		// v1.7.51 起新增 markers（生产 dialog_logs 实测样本）
		{"servers overloaded (90% 样本)",
			"Our servers are currently overloaded. Please try again later.", true},
		{"generic upstream error (10% 样本)",
			"An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists.", true},
		{"only 'try again later' tail", "Service unavailable. Try again later.", true},
		{"curl TLS wrong version is transient",
			"Failed to perform, curl: (35) TLS connect error: error:100000f7:SSL routines:OPENSSL_internal:WRONG_VERSION_NUMBER. See https://curl.se/libcurl/c/libcurl-errors.html first for more details.", true},
		// 反向防误伤
		{"auth error must NOT retry", "Invalid authentication credentials", false},
		{"context length must NOT retry", "This model's maximum context length is 128000 tokens", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCapacityError(c.msg); got != c.want {
				t.Errorf("isCapacityError(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"empty returns empty", "", ""},
		{"overloaded (90% production)",
			"Our servers are currently overloaded. Please try again later.", ErrKindOverloaded},
		{"capacity classic",
			"Selected model is at capacity. Please try a different mode", ErrKindCapacity},
		{"processing error (10% production)",
			"An error occurred while processing your request. You can retry your request, or contact us...", ErrKindProcessingError},
		{"rate limit", "Rate limit exceeded", ErrKindRateLimit},
		{"too many requests", "Too Many Requests", ErrKindRateLimit},
		{"context length", "This model's maximum context length is 128000 tokens", ErrKindContextLength},
		{"content filter", "Your request was blocked by content policy", ErrKindContentFilter},
		{"auth", "Invalid authentication credentials", ErrKindAuth},
		{"timeout", "Request timed out", ErrKindTimeout},
		{"curl TLS wrong version",
			"Failed to perform, curl: (35) TLS connect error: error:100000f7:SSL routines:OPENSSL_internal:WRONG_VERSION_NUMBER. See https://curl.se/libcurl/c/libcurl-errors.html first for more details.",
			ErrKindTLS},
		{"try again later tail → overloaded", "Service unavailable. Try again later.", ErrKindOverloaded},
		{"unknown garbage", "something exploded server-side", ErrKindUnknown},
		// 优先级测试：overloaded 优先于 try again later
		{"overloaded takes precedence over try again later",
			"Our servers are currently overloaded. Please try again later.", ErrKindOverloaded},
		// 优先级测试：proxy_auth 优先于通用 auth（"Proxy Authentication" 含 "authentication" 子串）
		{"proxy_auth wins over auth (生产实测)",
			`upstream_error: 请求上游失败 (caused by: Post "https://chatgpt.com/backend-api/codex/responses": Proxy Authentication Required)`,
			ErrKindProxyAuth},
		{"proxy_auth 407 prefix", "got HTTP 407 from proxy", ErrKindProxyAuth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyUpstreamError(c.msg); got != c.want {
				t.Errorf("ClassifyUpstreamError(%q) = %q, want %q", c.msg, got, c.want)
			}
		})
	}
}

func TestTruncateErrMsg(t *testing.T) {
	cases := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"short", 500, "short"},
		{"exact", 5, "exact"},
		{"hello world", 5, "hello"},
		{"", 100, ""},
		{"default", 0, "default"}, // 0 → 用默认 500
	}
	for _, c := range cases {
		if got := TruncateErrMsg(c.in, c.maxBytes); got != c.want {
			t.Errorf("TruncateErrMsg(%q,%d) = %q, want %q", c.in, c.maxBytes, got, c.want)
		}
	}
}

func TestTruncateForLog(t *testing.T) {
	cases := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"short", 100, "short"},
		{"exact", 5, "exact"},
		{"hello world", 5, "hello…(total=11)"},
		{"", 10, ""},
	}
	for _, c := range cases {
		if got := truncateForLog(c.in, c.maxLen); got != c.want {
			t.Errorf("truncateForLog(%q,%d) = %q, want %q", c.in, c.maxLen, got, c.want)
		}
	}
}

func TestExtractResponseFailedErrMsg(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "standard failed event",
			payload: `{"type":"response.failed","response":{"error":{"message":"Selected model is at capacity. Please try a different mode"}}}`,
			want:    "Selected model is at capacity. Please try a different mode",
		},
		{
			name:    "missing error field",
			payload: `{"type":"response.failed","response":{}}`,
			want:    "",
		},
		{
			name:    "non-failed event",
			payload: `{"type":"response.completed","response":{"output":[]}}`,
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractResponseFailedErrMsg([]byte(c.payload)); got != c.want {
				t.Errorf("extractResponseFailedErrMsg = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsPreContentEvent(t *testing.T) {
	cases := []struct {
		eventType string
		want      bool
	}{
		// 内容前控制事件：应缓冲
		{"response.created", true},
		{"response.in_progress", true},
		{"response.queued", true},
		// 真正的内容/终止事件：不能缓冲
		{"response.output_text.delta", false},
		{"response.function_call_arguments.delta", false},
		{"response.reasoning_text.delta", false},
		{"response.output_item.added", false},
		{"response.content_part.added", false},
		{"response.completed", false},
		{"response.failed", false},
		// 边界
		{"", false},
		{"unknown.event", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			if got := isPreContentEvent(c.eventType); got != c.want {
				t.Errorf("isPreContentEvent(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}
