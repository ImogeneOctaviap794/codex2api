package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponseModelFor_HitReturnsAlias(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4-mini"}
	if got := responseModelFor("gpt-5.4-mini", hit); got != drawingResponseModelAlias {
		t.Errorf("hit should return %q, got %q", drawingResponseModelAlias, got)
	}
	if drawingResponseModelAlias != "gpt-5.4" {
		t.Errorf("drawingResponseModelAlias = %q, want gpt-5.4", drawingResponseModelAlias)
	}
}

func TestResponseModelFor_MissReturnsOriginal(t *testing.T) {
	if got := responseModelFor("gpt-5.4", nil); got != "gpt-5.4" {
		t.Errorf("miss should return original, got %q", got)
	}
	if got := responseModelFor("gpt-5.4-mini", nil); got != "gpt-5.4-mini" {
		t.Errorf("miss should keep base_model, got %q", got)
	}
}

func TestRewriteResponseModelIfDrawing_Hit(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4-mini"}
	data := []byte(`{"model":"gpt-5.4-mini","choices":[]}`)
	out := rewriteResponseModelIfDrawing(data, hit, "model")
	if got := gjson.GetBytes(out, "model").String(); got != drawingResponseModelAlias {
		t.Errorf("model should be rewritten to %q, got %q", drawingResponseModelAlias, got)
	}
}

func TestRewriteResponseModelIfDrawing_NestedPath(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4-mini"}
	data := []byte(`{"type":"response.completed","response":{"id":"x","model":"gpt-5.4-mini"}}`)
	out := rewriteResponseModelIfDrawing(data, hit, "response.model")
	if got := gjson.GetBytes(out, "response.model").String(); got != drawingResponseModelAlias {
		t.Errorf("nested model should be rewritten, got %q", got)
	}
	// 不能误伤外层 type 字段
	if got := gjson.GetBytes(out, "type").String(); got != "response.completed" {
		t.Errorf("type should be untouched, got %q", got)
	}
}

func TestRewriteResponseModelIfDrawing_MissNoOp(t *testing.T) {
	data := []byte(`{"model":"gpt-5.4-mini"}`)
	out := rewriteResponseModelIfDrawing(data, nil, "model")
	if string(out) != string(data) {
		t.Errorf("miss should no-op, got %q", string(out))
	}
}

func TestRewriteResponseModelIfDrawing_PathNotExist(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4-mini"}
	data := []byte(`{"choices":[]}`)
	out := rewriteResponseModelIfDrawing(data, hit, "model")
	// path 不存在 → 原样返回，不创建新字段
	if gjson.GetBytes(out, "model").Exists() {
		t.Error("non-existent path should not be created")
	}
}

func TestRewriteResponseModelIfDrawing_EmptyData(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4-mini"}
	out := rewriteResponseModelIfDrawing(nil, hit, "model")
	if out != nil {
		t.Errorf("empty data should return nil, got %q", string(out))
	}
}

// ResponseAlias 非空时，覆盖 drawingResponseModelAlias 默认值。
// 典型用例：客户端发 gpt-5.5 → 上游用 gpt-5.4 → 响应里 model 改回 gpt-5.5。
func TestResponseModelFor_PreferResponseAlias(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4", ResponseAlias: "gpt-5.5"}
	if got := responseModelFor("gpt-5.5", hit); got != "gpt-5.5" {
		t.Errorf("ResponseAlias should win, want %q got %q", "gpt-5.5", got)
	}
}

func TestRewriteResponseModelIfDrawing_PreferResponseAlias(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4", ResponseAlias: "gpt-5.5"}
	data := []byte(`{"model":"gpt-5.4","choices":[]}`)
	out := rewriteResponseModelIfDrawing(data, hit, "model")
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.5" {
		t.Errorf("model should be rewritten to ResponseAlias %q, got %q", "gpt-5.5", got)
	}
}

// ResponseAlias 为空时，仍 fallback 到 drawingResponseModelAlias（兼容老画图行为）。
func TestRewriteResponseModelIfDrawing_FallbackWhenAliasEmpty(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4-mini"} // 无 ResponseAlias
	data := []byte(`{"model":"gpt-5.4-mini"}`)
	out := rewriteResponseModelIfDrawing(data, hit, "model")
	if got := gjson.GetBytes(out, "model").String(); got != drawingResponseModelAlias {
		t.Errorf("empty ResponseAlias should fall back to %q, got %q", drawingResponseModelAlias, got)
	}
}

// ParseModelOverrides 应能识别 response_alias 字段。
func TestParseModelOverrides_ResponseAlias(t *testing.T) {
	jsonStr := `{"gpt-5.5":{"base_model":"gpt-5.4","response_alias":"gpt-5.5"}}`
	m := ParseModelOverrides(jsonStr)
	if m == nil {
		t.Fatal("ParseModelOverrides returned nil")
	}
	cfg, ok := m["gpt-5.5"]
	if !ok {
		t.Fatal("missing gpt-5.5 entry")
	}
	if cfg.BaseModel != "gpt-5.4" {
		t.Errorf("BaseModel = %q, want gpt-5.4", cfg.BaseModel)
	}
	if cfg.ResponseAlias != "gpt-5.5" {
		t.Errorf("ResponseAlias = %q, want gpt-5.5", cfg.ResponseAlias)
	}
}

// scrubUpstreamModelMentions 应在 5.5→5.4 别名命中场景下，把 response.failed 事件
// error.message 里出现的 base_model 名替换成 response_alias，避免泄露上游真实模型。
func TestScrubUpstreamModelMentions_RewritesErrorMessage(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4", ResponseAlias: "gpt-5.5"}
	data := []byte(`{"type":"response.failed","response":{"error":{"message":"The model gpt-5.4 is at capacity. Please try a different model."}}}`)
	out := scrubUpstreamModelMentions(data, hit, "response.error.message")
	got := gjson.GetBytes(out, "response.error.message").String()
	want := "The model gpt-5.5 is at capacity. Please try a different model."
	if got != want {
		t.Errorf("error.message = %q, want %q", got, want)
	}
}

// 错误文案不含 base_model 时应原样返回。
func TestScrubUpstreamModelMentions_NoOpWhenMessageDoesNotContainBaseModel(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4", ResponseAlias: "gpt-5.5"}
	data := []byte(`{"type":"response.failed","response":{"error":{"message":"Our servers are currently overloaded. Please try again later."}}}`)
	out := scrubUpstreamModelMentions(data, hit, "response.error.message")
	if string(out) != string(data) {
		t.Errorf("expected no-op, got %q", string(out))
	}
}

// hit=nil / BaseModel 空 / alias==BaseModel 时都应是 no-op。
func TestScrubUpstreamModelMentions_NoOpGuards(t *testing.T) {
	data := []byte(`{"response":{"error":{"message":"gpt-5.4 is at capacity"}}}`)

	if out := scrubUpstreamModelMentions(data, nil, "response.error.message"); string(out) != string(data) {
		t.Error("nil hit should be no-op")
	}
	if out := scrubUpstreamModelMentions(data, &ModelOverride{ResponseAlias: "gpt-5.5"}, "response.error.message"); string(out) != string(data) {
		t.Error("empty BaseModel should be no-op")
	}
	hitSame := &ModelOverride{BaseModel: "gpt-5.4", ResponseAlias: "gpt-5.4"}
	if out := scrubUpstreamModelMentions(data, hitSame, "response.error.message"); string(out) != string(data) {
		t.Error("alias == BaseModel should be no-op")
	}
}

// path 不存在或非字符串时应原样返回。
func TestScrubUpstreamModelMentions_NoOpWhenPathMissing(t *testing.T) {
	hit := &ModelOverride{BaseModel: "gpt-5.4", ResponseAlias: "gpt-5.5"}
	data := []byte(`{"response":{"output":[]}}`)
	if out := scrubUpstreamModelMentions(data, hit, "response.error.message"); string(out) != string(data) {
		t.Errorf("missing path should be no-op, got %q", string(out))
	}
}
