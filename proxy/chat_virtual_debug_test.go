package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

// 模拟 Chat Completions 客户端选 gpt-draw-* 虚拟模型 → applyVirtualModel 注入 →
// TranslateRequest 翻译成 Codex body → 检查 tools/tool_choice 是否都到了最终 body。
func TestChatVirtualModelE2E_TranslateRequest(t *testing.T) {
	overrides := ParseModelOverrides(`{
		"gpt-draw-1024x1024": {
			"base_model": "gpt-5.4-mini",
			"inject": {
				"tools": [{"type":"image_generation","size":"1024x1024","quality":"high","background":"auto"}],
				"tool_choice": {"type":"image_generation"}
			}
		}
	}`)

	// 客户端原始请求
	raw := []byte(`{"model":"gpt-draw-1024x1024","stream":true,"messages":[{"role":"user","content":"画只猫"}]}`)

	// 1. applyVirtualModel 注入
	out, hit := ApplyModelOverride(raw, overrides)
	if hit == nil {
		t.Fatal("applyVirtualModel miss")
	}
	t.Logf("after applyVirtualModel: %s", string(out))

	// 2. TranslateRequest 翻译
	codexBody, err := TranslateRequest(out)
	if err != nil {
		t.Fatalf("TranslateRequest error: %v", err)
	}
	t.Logf("codexBody: %s", string(codexBody))

	// 3. 核心断言
	model := gjson.GetBytes(codexBody, "model").String()
	if model != "gpt-5.4-mini" {
		t.Fatalf("codexBody.model = %q, want gpt-5.4-mini", model)
	}
	tools := gjson.GetBytes(codexBody, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		t.Fatalf("codexBody.tools is empty/missing")
	}
	if got := gjson.GetBytes(codexBody, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("codexBody.tools[0].type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(codexBody, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("codexBody.tool_choice.type = %q, want image_generation", got)
	}
}
