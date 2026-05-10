package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

// resetResponseCacheForTest 清空响应上下文缓存，仅供测试使用。
func resetResponseCacheForTest() {
	respCache.mu.Lock()
	defer respCache.mu.Unlock()
	for k := range respCache.store {
		delete(respCache.store, k)
	}
}

func TestExpandPreviousResponseSkipsInjectionWhenInputHasFunctionCall(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse(
		[]byte(`[{"type":"message","role":"user","content":"call tool"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_dup","output":[{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{}"}]}}`),
	)

	// 客户端续链时同时自带 function_call 和 function_call_output，再注入缓存里的 function_call 会让 call_abc 重复。
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_dup","input":[` +
		`{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_abc","output":"sunny"}` +
		`]}`)
	got, prevID := expandPreviousResponse(body)

	if prevID != "resp_dup" {
		t.Fatalf("prevID = %q, want resp_dup", prevID)
	}
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input count = %d, want 2 (no injection); body=%s", len(input), got)
	}
	if typ := input[0].Get("type").String(); typ != "function_call" {
		t.Fatalf("input[0].type = %q, want function_call", typ)
	}
	if callID := input[0].Get("call_id").String(); callID != "call_abc" {
		t.Fatalf("input[0].call_id = %q, want call_abc", callID)
	}
}

func TestExpandPreviousResponseLeavesBodyUntouchedOnCacheMiss(t *testing.T) {
	resetResponseCacheForTest()

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[` +
		`{"type":"function_call_output","call_id":"call_missing","output":"x"}` +
		`]}`)
	got, prevID := expandPreviousResponse(body)

	if prevID != "resp_missing" {
		t.Fatalf("prevID = %q, want resp_missing (returned for downstream cache linkage)", prevID)
	}
	if string(got) != string(body) {
		t.Fatalf("body mutated on cache miss; got=%s want=%s", got, body)
	}
}

func TestCacheCompletedResponseStripsItemIDsAndSkipsReasoning(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse(
		[]byte(`[{"type":"message","id":"msg_input","role":"user","content":"call a tool"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_strip","output":[`+
			`{"type":"reasoning","id":"rs_0609","encrypted_content":"opaque"},`+
			`{"type":"message","id":"msg_output","role":"assistant","content":[{"type":"output_text","text":"thinking"}]},`+
			`{"type":"function_call","id":"fc_123","call_id":"call_abc","name":"lookup","arguments":"{}"}`+
			`]}}`),
	)

	cached := getResponseCache("resp_strip")
	if len(cached) != 2 {
		t.Fatalf("cached items = %d, want input message + function_call only (reasoning/message output should be skipped)", len(cached))
	}
	if typ := gjson.GetBytes(cached[0], "type").String(); typ != "message" {
		t.Fatalf("cached[0].type = %q, want message", typ)
	}
	if id := gjson.GetBytes(cached[0], "id"); id.Exists() {
		t.Fatalf("cached input id should be stripped, got %s", id.Raw)
	}
	if typ := gjson.GetBytes(cached[1], "type").String(); typ != "function_call" {
		t.Fatalf("cached[1].type = %q, want function_call", typ)
	}
	if id := gjson.GetBytes(cached[1], "id"); id.Exists() {
		t.Fatalf("cached function_call id should be stripped, got %s", id.Raw)
	}
	// call_id 必须保留——它是续链所依赖的关键字段
	if callID := gjson.GetBytes(cached[1], "call_id").String(); callID != "call_abc" {
		t.Fatalf("cached function_call call_id = %q, want call_abc (must be preserved)", callID)
	}
}
