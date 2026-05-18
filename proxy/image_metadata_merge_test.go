package proxy

import (
	"encoding/json"
	"testing"
)

// 辅助：解析 body，找到第一个 type=="image_generation" 的 tool
func firstImageTool(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	tools, ok := m["tools"].([]any)
	if !ok {
		return nil
	}
	for _, tv := range tools {
		tm, ok := tv.(map[string]any)
		if !ok {
			continue
		}
		if ty, _ := tm["type"].(string); ty == "image_generation" {
			return tm
		}
	}
	return nil
}

// =========== 直接作用于 map 的单元测试 ===========

func TestMergeImageMetadata_MergesNonEmpty(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"image_size":          "1536x1024",
			"image_quality":       "high",
			"image_background":    "transparent",
			"unrelated":           "keep",
			"image_output_format": "",
		},
		"tools": []any{
			map[string]any{"type": "image_generation", "quality": "low"},
		},
	}

	mergeImageMetadataIntoTools(body)

	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["size"] != "1536x1024" {
		t.Errorf("size = %v, want 1536x1024", tool["size"])
	}
	if tool["quality"] != "high" {
		t.Errorf("quality should be overridden to high, got %v", tool["quality"])
	}
	if tool["background"] != "transparent" {
		t.Errorf("background = %v, want transparent", tool["background"])
	}
	if _, ok := tool["output_format"]; ok {
		t.Error("empty output_format should be skipped")
	}

	// 上游不接受任意 metadata 字段，包括未识别的 `unrelated`，整份 metadata 都该被删除
	if _, ok := body["metadata"]; ok {
		t.Error("metadata should be fully dropped (upstream rejects any metadata field)")
	}
}

// alias 兼容：客户端用 OpenAI Images API 风格的无前缀键名 (size/quality/…)，
// 应被 normalize 成带 image_ 前缀的内部约定键，并合并进 image_generation tool。
func TestMergeImageMetadata_NormalizesNoPrefixAliases(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"size":          "3840x2160",
			"quality":       "high",
			"background":    "transparent",
			"output_format": "webp",
		},
		"tools": []any{
			map[string]any{"type": "image_generation"},
		},
	}

	mergeImageMetadataIntoTools(body)

	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["size"] != "3840x2160" {
		t.Errorf("size = %v, want 3840x2160 (from no-prefix alias)", tool["size"])
	}
	if tool["quality"] != "high" {
		t.Errorf("quality = %v, want high (from no-prefix alias)", tool["quality"])
	}
	if tool["background"] != "transparent" {
		t.Errorf("background = %v, want transparent", tool["background"])
	}
	if tool["output_format"] != "webp" {
		t.Errorf("output_format = %v, want webp", tool["output_format"])
	}
	if _, ok := body["metadata"]; ok {
		t.Error("metadata should be dropped after alias normalize + consume")
	}
}

// 同请求中同时传带前缀和无前缀键：带前缀优先，无前缀被丢弃。
func TestMergeImageMetadata_PrefixedKeyWinsOverAlias(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"size":       "1024x1024", // alias，应让位
			"image_size": "3840x2160", // 带前缀，胜出
			"quality":    "low",       // alias（无对应带前缀键 → 应被 normalize）
		},
		"tools": []any{map[string]any{"type": "image_generation"}},
	}

	mergeImageMetadataIntoTools(body)

	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["size"] != "3840x2160" {
		t.Errorf("size = %v, want 3840x2160 (prefixed key takes precedence)", tool["size"])
	}
	if tool["quality"] != "low" {
		t.Errorf("quality = %v, want low (alias normalized when no prefixed twin)", tool["quality"])
	}
}

// 仅传未识别字段时，metadata 也应被完全 drop（上游不接受任何 metadata）。
func TestMergeImageMetadata_UnknownKeysOnly_DropsMetadata(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"trace_id":  "abc-123",
			"user_tag":  "alpha",
			"timestamp": float64(1700000000),
		},
		"tools": []any{map[string]any{"type": "image_generation", "size": "auto"}},
	}

	mergeImageMetadataIntoTools(body)

	if _, ok := body["metadata"]; ok {
		t.Error("metadata with only unknown keys should be dropped")
	}
	// tools 不应被无关 metadata 影响
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["size"] != "auto" {
		t.Errorf("tool should be untouched when no image_* metadata, got %+v", tool)
	}
}

func TestMergeImageMetadata_RemovesEmptyMetadata(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"image_size": "1024x1024",
		},
		"tools": []any{
			map[string]any{"type": "image_generation"},
		},
	}
	mergeImageMetadataIntoTools(body)
	if _, ok := body["metadata"]; ok {
		t.Fatal("metadata should be deleted when all consumed")
	}
}

func TestMergeImageMetadata_NoImageTool_Injects(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"image_size":    "1024x1024",
			"image_quality": "medium",
		},
	}
	mergeImageMetadataIntoTools(body)

	tool := firstImageTool(t, mustMarshal(t, body))
	if tool == nil {
		t.Fatal("image_generation tool should be injected when absent")
	}
	if tool["size"] != "1024x1024" || tool["quality"] != "medium" {
		t.Errorf("injected tool mismatch: %+v", tool)
	}
}

func TestMergeImageMetadata_NoMetadata_Noop(t *testing.T) {
	body := map[string]any{
		"tools": []any{map[string]any{"type": "image_generation", "size": "auto"}},
	}
	mergeImageMetadataIntoTools(body)
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["size"] != "auto" {
		t.Errorf("tool should be untouched, got %+v", tool)
	}
}

func TestMergeImageMetadata_PreservesNonString(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"image_output_compression": float64(80),
			"image_partial_images":     float64(2),
		},
		"tools": []any{map[string]any{"type": "image_generation"}},
	}
	mergeImageMetadataIntoTools(body)
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["output_compression"] != float64(80) {
		t.Errorf("output_compression = %v, want 80", tool["output_compression"])
	}
	if tool["partial_images"] != float64(2) {
		t.Errorf("partial_images = %v, want 2", tool["partial_images"])
	}
}

// =========== 端到端：TranslateRequest（Chat Completions） ===========

func TestTranslateRequest_MetadataOverridesInjectedTool(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-image",
		"messages": [{"role":"user","content":"cat"}],
		"tools": [{"type":"image_generation","quality":"low","size":"auto"}],
		"tool_choice": {"type":"image_generation"},
		"metadata": {
			"image_quality": "high",
			"image_size": "1536x1024",
			"image_background": "transparent"
		}
	}`)

	out, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest err: %v", err)
	}

	tool := firstImageTool(t, out)
	if tool == nil {
		t.Fatal("image_generation tool missing in output")
	}
	if tool["quality"] != "high" {
		t.Errorf("quality = %v, want high (metadata overrides inject)", tool["quality"])
	}
	if tool["size"] != "1536x1024" {
		t.Errorf("size = %v, want 1536x1024", tool["size"])
	}
	if tool["background"] != "transparent" {
		t.Errorf("background = %v, want transparent", tool["background"])
	}

	// metadata 里 image_* 应该被消费掉
	var outMap map[string]any
	_ = json.Unmarshal(out, &outMap)
	if md, ok := outMap["metadata"].(map[string]any); ok {
		for _, k := range []string{"image_size", "image_quality", "image_background"} {
			if _, exists := md[k]; exists {
				t.Errorf("metadata.%s should be consumed", k)
			}
		}
	}
}

func TestTranslateRequest_MetadataInjectsToolWhenAbsent(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-5",
		"messages": [{"role":"user","content":"cat"}],
		"metadata": {"image_size": "1024x1024"}
	}`)
	out, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	tool := firstImageTool(t, out)
	if tool == nil {
		t.Fatal("image_generation tool should be injected from metadata alone")
	}
	if tool["size"] != "1024x1024" {
		t.Errorf("size mismatch: %+v", tool)
	}
}

// =========== 端到端：PrepareResponsesBody（Responses API） ===========

func TestPrepareResponsesBody_MetadataMergesIntoTools(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-5",
		"input": "hi",
		"tools": [{"type":"image_generation","quality":"auto"}],
		"metadata": {
			"image_quality": "high",
			"image_output_format": "webp"
		}
	}`)
	out, _ := PrepareResponsesBody(raw)

	tool := firstImageTool(t, out)
	if tool == nil {
		t.Fatal("image_generation tool missing")
	}
	if tool["quality"] != "high" {
		t.Errorf("quality = %v, want high", tool["quality"])
	}
	if tool["output_format"] != "webp" {
		t.Errorf("output_format = %v, want webp", tool["output_format"])
	}
}

func TestPrepareResponsesBody_EmptyStringSkipped(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-5",
		"input": "hi",
		"tools": [{"type":"image_generation","quality":"high"}],
		"metadata": {
			"image_quality": "",
			"image_size": "1024x1024"
		}
	}`)
	out, _ := PrepareResponsesBody(raw)

	tool := firstImageTool(t, out)
	if tool["quality"] != "high" {
		t.Errorf("empty quality string should not override existing, got %v", tool["quality"])
	}
	if tool["size"] != "1024x1024" {
		t.Errorf("size = %v, want 1024x1024", tool["size"])
	}
}

// ========== input_fidelity 上游兼容性过滤 ==========

// gpt-image-2（上游默认，含 model 字段空）不支持 input_fidelity，
// codex2api 必须在发给上游前吞掉该字段，避免 400 invalid_input_fidelity_model。
func TestSanitizeImageTool_DropsFidelityOnGptImage2(t *testing.T) {
	cases := []struct {
		name    string
		tool    map[string]any
		wantFid bool
	}{
		{
			name:    "empty model (upstream default is gpt-image-2)",
			tool:    map[string]any{"type": "image_generation", "input_fidelity": "high"},
			wantFid: false,
		},
		{
			name:    "explicit gpt-image-2",
			tool:    map[string]any{"type": "image_generation", "model": "gpt-image-2", "input_fidelity": "high"},
			wantFid: false,
		},
		{
			name:    "gpt-image-1 keeps fidelity",
			tool:    map[string]any{"type": "image_generation", "model": "gpt-image-1", "input_fidelity": "high"},
			wantFid: true,
		},
		{
			name:    "gpt-image-1.5 keeps fidelity",
			tool:    map[string]any{"type": "image_generation", "model": "gpt-image-1.5", "input_fidelity": "low"},
			wantFid: true,
		},
		{
			name:    "gpt-image-1-mini keeps fidelity",
			tool:    map[string]any{"type": "image_generation", "model": "gpt-image-1-mini", "input_fidelity": "high"},
			wantFid: true,
		},
		{
			name:    "unknown future model drops fidelity conservatively",
			tool:    map[string]any{"type": "image_generation", "model": "gpt-image-3", "input_fidelity": "high"},
			wantFid: false,
		},
		{
			name:    "no fidelity field is no-op",
			tool:    map[string]any{"type": "image_generation", "model": "gpt-image-2"},
			wantFid: false, // 本来就没，保持没
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sanitizeImageTool(c.tool)
			_, has := c.tool["input_fidelity"]
			if has != c.wantFid {
				t.Errorf("input_fidelity presence = %v, want %v (tool=%v)", has, c.wantFid, c.tool)
			}
		})
	}
}

// 端到端：客户端通过 metadata.image_input_fidelity 传给 gpt-image-2 上游，
// mergeImageMetadataIntoTools 最终不能让这个参数发出去。
func TestMergeImageMetadata_DropsFidelityForGptImage2(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"image_input_fidelity": "high",
			"image_size":           "1024x1024",
		},
		"tools": []any{
			// 没指定 model → 上游默认 gpt-image-2
			map[string]any{"type": "image_generation"},
		},
	}

	mergeImageMetadataIntoTools(body)

	tool := body["tools"].([]any)[0].(map[string]any)
	if _, has := tool["input_fidelity"]; has {
		t.Errorf("input_fidelity should be dropped for gpt-image-2, got tool=%v", tool)
	}
	if tool["size"] != "1024x1024" {
		t.Errorf("size should still be merged, got %v", tool["size"])
	}
}

// 客户端同时传 image_model=gpt-image-1.5 + image_input_fidelity=high 时，
// input_fidelity 必须保留（该模型支持）。
func TestMergeImageMetadata_KeepsFidelityForGptImage15(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{
			"image_model":          "gpt-image-1.5",
			"image_input_fidelity": "high",
		},
		"tools": []any{
			map[string]any{"type": "image_generation"},
		},
	}

	mergeImageMetadataIntoTools(body)

	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["model"] != "gpt-image-1.5" {
		t.Errorf("model = %v, want gpt-image-1.5", tool["model"])
	}
	if tool["input_fidelity"] != "high" {
		t.Errorf("input_fidelity should be kept for gpt-image-1.5, got %v", tool["input_fidelity"])
	}
}

// ========== 小工具 ==========

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
