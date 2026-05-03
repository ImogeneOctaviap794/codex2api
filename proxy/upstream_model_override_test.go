package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyUpstreamModelOverride_SetsModelFromMetadata(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","messages":[],"metadata":{"upstream_model":"gpt-5"}}`)
	out := applyUpstreamModelOverride(body)

	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5" {
		t.Errorf("model should be overridden to gpt-5, got %q", got)
	}
	if gjson.GetBytes(out, "metadata.upstream_model").Exists() {
		t.Error("metadata.upstream_model should be removed after consumption")
	}
	// metadata 只剩 upstream_model 一个键 → 整个 metadata 也该被删除
	if gjson.GetBytes(out, "metadata").Exists() {
		t.Error("empty metadata should be deleted")
	}
}

func TestApplyUpstreamModelOverride_PreservesOtherMetadataKeys(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","metadata":{"upstream_model":"gpt-5.2","image_quality":"high"}}`)
	out := applyUpstreamModelOverride(body)

	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.2" {
		t.Errorf("model mismatch, got %q", got)
	}
	if got := gjson.GetBytes(out, "metadata.image_quality").String(); got != "high" {
		t.Errorf("image_quality should be preserved, got %q", got)
	}
	if gjson.GetBytes(out, "metadata.upstream_model").Exists() {
		t.Error("upstream_model should be removed")
	}
}

func TestApplyUpstreamModelOverride_EmptyMetadataIsNoOp(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","messages":[]}`)
	out := applyUpstreamModelOverride(body)
	if string(out) != string(body) {
		t.Errorf("no metadata → should be no-op")
	}
}

func TestApplyUpstreamModelOverride_EmptyStringIsNoOp(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","metadata":{"upstream_model":""}}`)
	out := applyUpstreamModelOverride(body)
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.4-mini" {
		t.Errorf("empty upstream_model should not override; got %q", got)
	}
}

func TestApplyUpstreamModelOverride_NilBodyIsNoOp(t *testing.T) {
	out := applyUpstreamModelOverride(nil)
	if out != nil {
		t.Errorf("nil body should return nil, got %q", string(out))
	}
}

func TestApplyUpstreamModelOverride_WorksAfterVirtualModel(t *testing.T) {
	overrides := ParseModelOverrides(`{
		"gpt-image-2": {
			"base_model": "gpt-5.4-mini",
			"inject": {"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}
		}
	}`)

	// 客户端发：gpt-image-2 虚拟模型 + metadata.upstream_model 想上游换成 gpt-5.2
	body := []byte(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"x"}],
		"metadata":{"upstream_model":"gpt-5.2","image_quality":"low"}
	}`)

	// 1. 虚拟模型注入：model → gpt-5.4-mini + tools/tool_choice 合并
	afterVM, hit := ApplyModelOverride(body, overrides)
	if hit == nil {
		t.Fatal("virtual model should hit")
	}
	if got := gjson.GetBytes(afterVM, "model").String(); got != "gpt-5.4-mini" {
		t.Fatalf("after virtual model, model should be gpt-5.4-mini, got %q", got)
	}

	// 2. 隐藏参数覆盖：model → gpt-5.2
	afterUM := applyUpstreamModelOverride(afterVM)
	if got := gjson.GetBytes(afterUM, "model").String(); got != "gpt-5.2" {
		t.Errorf("after upstream override, model should be gpt-5.2, got %q", got)
	}
	// tools / tool_choice 依旧在
	if !gjson.GetBytes(afterUM, "tools.0.type").Exists() {
		t.Error("tools should survive upstream override")
	}
	if got := gjson.GetBytes(afterUM, "tool_choice.type").String(); got != "image_generation" {
		t.Errorf("tool_choice should survive, got %q", got)
	}
	// image_quality 也要在
	if got := gjson.GetBytes(afterUM, "metadata.image_quality").String(); got != "low" {
		t.Errorf("image_quality should survive, got %q", got)
	}
}
