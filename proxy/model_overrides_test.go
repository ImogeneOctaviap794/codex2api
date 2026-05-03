package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestParseModelOverrides_Valid(t *testing.T) {
	raw := `{
		"gpt-draw-1024x1024": {
			"base_model": "gpt-5.4-mini",
			"inject": {
				"tools": [{"type":"image_generation","size":"1024x1024"}],
				"tool_choice": {"type":"image_generation"}
			},
			"description": "1:1 画图"
		},
		"gpt-draw-1024x1536": {
			"base_model": "gpt-5.4-mini",
			"inject": {"tools": [{"type":"image_generation","size":"1024x1536"}]}
		}
	}`

	m := ParseModelOverrides(raw)
	if len(m) != 2 {
		t.Fatalf("len(m) = %d, want 2", len(m))
	}
	if m["gpt-draw-1024x1024"].BaseModel != "gpt-5.4-mini" {
		t.Fatalf("base_model mismatch")
	}
	if m["gpt-draw-1024x1024"].Description != "1:1 画图" {
		t.Fatalf("description mismatch")
	}
}

func TestParseModelOverrides_SkipsEmptyBaseModel(t *testing.T) {
	raw := `{"bad":{"base_model":""},"good":{"base_model":"gpt-5.4"}}`
	m := ParseModelOverrides(raw)
	if _, ok := m["bad"]; ok {
		t.Fatal("empty base_model should be skipped")
	}
	if _, ok := m["good"]; !ok {
		t.Fatal("valid entry missing")
	}
}

func TestParseModelOverrides_EmptyOrInvalid(t *testing.T) {
	for _, in := range []string{"", "{}", "not-json", "[1,2]"} {
		if m := ParseModelOverrides(in); m != nil {
			t.Fatalf("input %q should yield nil, got %+v", in, m)
		}
	}
}

func TestVirtualModelNames_Sorted(t *testing.T) {
	m := ModelOverrideMap{
		"z": {BaseModel: "x"},
		"a": {BaseModel: "x"},
		"m": {BaseModel: "x"},
	}
	got := m.VirtualModelNames()
	want := []string{"a", "m", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestApplyModelOverride_HitInjectsToolsAndReplacesModel(t *testing.T) {
	overrides := ParseModelOverrides(`{
		"gpt-draw-1024x1024": {
			"base_model": "gpt-5.4-mini",
			"inject": {
				"tools": [{"type":"image_generation","size":"1024x1024","quality":"high"}],
				"tool_choice": {"type":"image_generation"}
			}
		}
	}`)
	body := []byte(`{"model":"gpt-draw-1024x1024","input":"画只猫"}`)

	out, hit := ApplyModelOverride(body, overrides)
	if hit == nil {
		t.Fatal("expected hit, got nil")
	}
	if hit.BaseModel != "gpt-5.4-mini" {
		t.Fatalf("hit.BaseModel = %q", hit.BaseModel)
	}

	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.4-mini" {
		t.Fatalf("model in out = %q, want gpt-5.4-mini", got)
	}
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools[0].type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(out, "tools.0.size").String(); got != "1024x1024" {
		t.Fatalf("tools[0].size = %q, want 1024x1024", got)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation", got)
	}
	// input 未被动过
	if got := gjson.GetBytes(out, "input").String(); got != "画只猫" {
		t.Fatalf("input field lost: %q", got)
	}
}

func TestApplyModelOverride_InjectOverwritesExistingFields(t *testing.T) {
	overrides := ParseModelOverrides(`{
		"gpt-draw-1024x1024": {
			"base_model": "gpt-5.4-mini",
			"inject": {"tool_choice": {"type":"image_generation"}}
		}
	}`)
	// 客户端传了一个 tool_choice=auto，应被覆盖
	body := []byte(`{"model":"gpt-draw-1024x1024","input":"画","tool_choice":"auto"}`)
	out, hit := ApplyModelOverride(body, overrides)
	if hit == nil {
		t.Fatal("expected hit")
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation (override should win)", got)
	}
}

func TestApplyModelOverride_MissWhenModelNotVirtual(t *testing.T) {
	overrides := ParseModelOverrides(`{
		"gpt-draw-1024x1024": {"base_model":"gpt-5.4-mini","inject":{}}
	}`)
	body := []byte(`{"model":"gpt-5.4","input":"hi"}`)
	out, hit := ApplyModelOverride(body, overrides)
	if hit != nil {
		t.Fatal("expected miss")
	}
	// body 字节级别可能未被修改
	var want, got map[string]any
	_ = json.Unmarshal(body, &want)
	_ = json.Unmarshal(out, &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("body should be unchanged on miss")
	}
}

func TestApplyModelOverride_NilOverridesReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"whatever"}`)
	out, hit := ApplyModelOverride(body, nil)
	if hit != nil || string(out) != string(body) {
		t.Fatal("nil overrides should passthrough")
	}
}

func TestApplyModelOverride_InvalidJSONReturnsOriginal(t *testing.T) {
	overrides := ParseModelOverrides(`{"x":{"base_model":"gpt-5.4"}}`)
	body := []byte(`not-a-json`)
	out, hit := ApplyModelOverride(body, overrides)
	if hit != nil {
		t.Fatal("invalid json should not hit")
	}
	if string(out) != string(body) {
		t.Fatal("invalid json should passthrough")
	}
}

// 验证 Chat Completions 流式翻译层收到 image_generation_call 终稿事件时
// 会把 base64 PNG 包成 markdown image 注入到 assistant content 中。
// 这样 OpenAI Chat Completions 格式的客户端也能直接显示生成的图像。
func TestTranslateStreamChunk_ImageGenerationCallEmitsMarkdown(t *testing.T) {
	// 模拟上游 SSE 事件：response.output_item.done with image_generation_call
	b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB" // 任意有效 PNG 头的 base64 片段
	event := []byte(`{"type":"response.output_item.done","item":{"id":"ig_x","type":"image_generation_call","status":"completed","result":"` + b64 + `"}}`)

	chunk, done := TranslateStreamChunk(event, "gpt-draw-1024x1024", "chatcmpl-x", 0)
	if done {
		t.Fatal("image_generation_call.done should not finish the stream")
	}
	if chunk == nil {
		t.Fatal("expected content chunk, got nil")
	}
	s := string(chunk)
	if !strings.Contains(s, "data:image/png;base64,"+b64) {
		t.Fatalf("chunk missing markdown image; got %s", s)
	}
	if !strings.Contains(s, `"chat.completion.chunk"`) {
		t.Fatalf("not a chat.completion.chunk; got %s", s)
	}
}

// 有状态翻译器路径同样应支持 image_generation_call。
func TestStreamTranslator_ImageGenerationCallEmitsMarkdown(t *testing.T) {
	b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"
	event := []byte(`{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"` + b64 + `"}}`)

	st := NewStreamTranslator("chatcmpl-x", "gpt-draw-1024x1024", 0)
	chunk, done := st.Translate(event)
	if done {
		t.Fatal("should not finish on image_generation_call.done")
	}
	if chunk == nil {
		t.Fatal("expected content chunk")
	}
	if !strings.Contains(string(chunk), "data:image/png;base64,"+b64) {
		t.Fatalf("chunk missing markdown image; got %s", string(chunk))
	}
	if st.HasToolCalls {
		t.Fatal("image_generation_call should NOT set HasToolCalls (finish_reason stays 'stop')")
	}
}

// 确保非 image_generation_call 的 output_item.done 仍被忽略（向前兼容）。
func TestTranslateStreamChunk_NonImageOutputItemDoneIgnored(t *testing.T) {
	event := []byte(`{"type":"response.output_item.done","item":{"type":"message"}}`)
	chunk, done := TranslateStreamChunk(event, "gpt-5.4", "id", 0)
	if done || chunk != nil {
		t.Fatalf("non-image output_item.done should return (nil, false), got (%v, %v)", chunk, done)
	}
}
