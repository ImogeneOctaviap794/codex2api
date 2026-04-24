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
