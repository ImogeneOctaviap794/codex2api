package proxy

import (
	"encoding/json"
	"testing"
)

// skip_if 为空：正常改写
func TestApplyModelOverride_SkipIf_Nil(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"response_alias": "gpt-5.5"
		}
	}`
	overrides := ParseModelOverrides(raw)
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"xhigh"}}`)
	out, hit := ApplyModelOverride(body, overrides)
	if hit == nil {
		t.Fatal("期望命中 override（skip_if 为空）")
	}
	if got := gjsonStr(out, "model"); got != "gpt-5.4" {
		t.Fatalf("model 应被改为 gpt-5.4，实际=%s", got)
	}
}

// skip_if 字符串匹配：跳过
func TestApplyModelOverride_SkipIf_StringMatch(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"response_alias": "gpt-5.5",
			"skip_if": {"reasoning.effort": "xhigh"}
		}
	}`
	overrides := ParseModelOverrides(raw)
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"xhigh"}}`)
	out, hit := ApplyModelOverride(body, overrides)
	if hit != nil {
		t.Fatal("期望跳过 override（skip_if 命中）")
	}
	if got := gjsonStr(out, "model"); got != "gpt-5.5" {
		t.Fatalf("model 应保持 gpt-5.5，实际=%s", got)
	}
}

// skip_if 字符串不匹配：正常改写
func TestApplyModelOverride_SkipIf_StringMiss(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"skip_if": {"reasoning.effort": "xhigh"}
		}
	}`
	overrides := ParseModelOverrides(raw)
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"medium"}}`)
	_, hit := ApplyModelOverride(body, overrides)
	if hit == nil {
		t.Fatal("effort=medium 不应命中 skip_if，应正常改写")
	}
}

// skip_if 数组任一匹配：跳过
func TestApplyModelOverride_SkipIf_ArrayMatch(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"skip_if": {"reasoning.effort": ["xhigh","high"]}
		}
	}`
	overrides := ParseModelOverrides(raw)

	// high 命中
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"}}`)
	if _, hit := ApplyModelOverride(body, overrides); hit != nil {
		t.Fatal("effort=high 应命中 skip_if array")
	}

	// xhigh 命中
	body = []byte(`{"model":"gpt-5.5","reasoning":{"effort":"xhigh"}}`)
	if _, hit := ApplyModelOverride(body, overrides); hit != nil {
		t.Fatal("effort=xhigh 应命中 skip_if array")
	}

	// medium 不命中
	body = []byte(`{"model":"gpt-5.5","reasoning":{"effort":"medium"}}`)
	if _, hit := ApplyModelOverride(body, overrides); hit == nil {
		t.Fatal("effort=medium 不应命中 skip_if array")
	}
}

// skip_if 多 key OR 语义：双字段兼容 Responses 和 Chat 两种 API
func TestApplyModelOverride_SkipIf_MultiKeyOR(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"skip_if": {
				"reasoning.effort": "xhigh",
				"reasoning_effort": "xhigh"
			}
		}
	}`
	overrides := ParseModelOverrides(raw)

	// Responses API 风格：reasoning.effort
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"xhigh"}}`)
	if _, hit := ApplyModelOverride(body, overrides); hit != nil {
		t.Fatal("Responses 格式 effort=xhigh 应命中")
	}

	// Chat Completions 风格：reasoning_effort
	body = []byte(`{"model":"gpt-5.5","reasoning_effort":"xhigh"}`)
	if _, hit := ApplyModelOverride(body, overrides); hit != nil {
		t.Fatal("Chat 格式 reasoning_effort=xhigh 应命中")
	}

	// 都不是 xhigh：正常改写
	body = []byte(`{"model":"gpt-5.5","reasoning_effort":"medium"}`)
	if _, hit := ApplyModelOverride(body, overrides); hit == nil {
		t.Fatal("medium 不应命中，应正常改写")
	}
}

// skip_if 路径不存在：不命中（视为 false）
func TestApplyModelOverride_SkipIf_PathMissing(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"skip_if": {"reasoning.effort": "xhigh"}
		}
	}`
	overrides := ParseModelOverrides(raw)
	body := []byte(`{"model":"gpt-5.5"}`) // 无 reasoning 字段
	_, hit := ApplyModelOverride(body, overrides)
	if hit == nil {
		t.Fatal("字段不存在时不应命中 skip_if，应正常改写")
	}
}

// skip_if 布尔匹配
func TestApplyModelOverride_SkipIf_Bool(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"skip_if": {"stream": false}
		}
	}`
	overrides := ParseModelOverrides(raw)

	// stream=false 命中
	body := []byte(`{"model":"gpt-5.5","stream":false}`)
	if _, hit := ApplyModelOverride(body, overrides); hit != nil {
		t.Fatal("stream=false 应命中 skip_if bool")
	}

	// stream=true 不命中
	body = []byte(`{"model":"gpt-5.5","stream":true}`)
	if _, hit := ApplyModelOverride(body, overrides); hit == nil {
		t.Fatal("stream=true 不应命中")
	}
}

// ParseModelOverrides 保留 skip_if 字段
func TestParseModelOverrides_PreservesSkipIf(t *testing.T) {
	raw := `{
		"gpt-5.5": {
			"base_model": "gpt-5.4",
			"skip_if": {"reasoning.effort": "xhigh"}
		}
	}`
	m := ParseModelOverrides(raw)
	got := m["gpt-5.5"]
	if len(got.SkipIf) != 1 {
		t.Fatalf("SkipIf 长度 = %d, want 1", len(got.SkipIf))
	}
	var v string
	if err := json.Unmarshal(got.SkipIf["reasoning.effort"], &v); err != nil || v != "xhigh" {
		t.Fatalf("SkipIf 值解析错误: v=%q err=%v", v, err)
	}
}

// 辅助：从 JSON 提取字符串
func gjsonStr(body []byte, path string) string {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	cur := any(m)
	for _, p := range splitDot(path) {
		mp, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mp[p]
	}
	s, _ := cur.(string)
	return s
}

func splitDot(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
