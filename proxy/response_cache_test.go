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
