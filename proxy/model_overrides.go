package proxy

import (
	"encoding/json"
	"sort"

	"github.com/tidwall/gjson"
)

// ModelOverride 描述一个"虚拟模型别名"的配置。
//
// 典型用法：把 image_generation 工具固化成独立模型名，让 Chat Completions
// 这种"只能选模型名"的客户端也能直接画图。
//
// 示例：
//
//	{
//	  "base_model": "gpt-5.4-mini",
//	  "inject": {
//	    "tools": [{"type":"image_generation","size":"1024x1024","quality":"high","background":"auto"}],
//	    "tool_choice": {"type":"image_generation"}
//	  }
//	}
type ModelOverride struct {
	// BaseModel 上游真实模型名。命中虚拟模型时，请求体里的 model 字段会被
	// 替换为该值。必填。
	BaseModel string `json:"base_model"`

	// Inject 一个任意 JSON 对象；它的所有顶层键会被合并/覆盖到请求体里，
	// 然后再走正常的翻译流程。例如可以在这里注入 tools / tool_choice /
	// reasoning / service_tier 等。
	Inject map[string]json.RawMessage `json:"inject,omitempty"`

	// ResponseAlias 命中本虚拟模型时，响应体中对外展示的 model 名。
	//   - 非空：响应里的 model 字段被改写为该值（覆盖默认行为）
	//   - 空：保持旧行为——画图场景退化为 drawingResponseModelAlias
	//     ("gpt-5.4")，非画图场景由调用方决定
	//
	// 典型用法：客户端请求 gpt-5.5，本字段填 "gpt-5.5"，
	// 配合 base_model="gpt-5.4" 实现"上游用 5.4，响应保持 5.5"。
	ResponseAlias string `json:"response_alias,omitempty"`

	// Description 可选的人类可读描述，仅用于 UI 展示。
	Description string `json:"description,omitempty"`

	// SkipIf 条件路由：当请求体里的字段值命中任一条件时，跳过本 override，
	// 保持原请求的 model 和其他字段不被改写（相当于让该别名透传到真实的同名上游）。
	//
	// key 支持 gjson 点分路径（如 "reasoning.effort" / "reasoning_effort" / "stream"）。
	// value 支持：
	//   - 字符串："xhigh"             （精确相等）
	//   - 字符串/数字/布尔数组：["xhigh","high"]（任一相等即匹配）
	//   - 数字/布尔：42 / true
	// 多个 key 之间是 OR 语义：任一 key 命中就跳过。路径不存在时视为不命中。
	//
	// 典型用法：
	//   {
	//     "reasoning.effort":  "xhigh",
	//     "reasoning_effort":  "xhigh"
	//   }
	// 表示 gpt-5.5 默认 → gpt-5.4，但 effort=xhigh 时保持 gpt-5.5 原样透传
	// （同时覆盖 Responses API 和 Chat Completions 两种字段命名）。
	SkipIf map[string]json.RawMessage `json:"skip_if,omitempty"`
}

// ModelOverrideMap 虚拟模型名 → 配置的映射。
type ModelOverrideMap map[string]ModelOverride

// BuiltInModelOverrides 返回无需管理员手动配置即可使用的内置虚拟模型。
func BuiltInModelOverrides() ModelOverrideMap {
	return ModelOverrideMap{
		"gpt-5.5-fast": {
			BaseModel:     "gpt-5.5",
			ResponseAlias: "gpt-5.5-fast",
			Inject: map[string]json.RawMessage{
				"service_tier": json.RawMessage(`"fast"`),
			},
			Description: "gpt-5.5 with service_tier=fast",
		},
		"gpt-5.4-fast": {
			BaseModel:     "gpt-5.4",
			ResponseAlias: "gpt-5.4-fast",
			Inject: map[string]json.RawMessage{
				"service_tier": json.RawMessage(`"fast"`),
			},
			Description: "gpt-5.4 with service_tier=fast",
		},
	}
}

// MergeModelOverrides 合并多个虚拟模型配置，后者覆盖前者。
func MergeModelOverrides(overrides ...ModelOverrideMap) ModelOverrideMap {
	merged := ModelOverrideMap{}
	for _, current := range overrides {
		for name, cfg := range current {
			merged[name] = cfg
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// ParseModelOverrides 解析 JSON 字符串。空字符串或非法 JSON 返回空 map（不报错），
// 符合"配置错误时静默退化为无别名"的容错策略。
func ParseModelOverrides(jsonStr string) ModelOverrideMap {
	if jsonStr == "" || jsonStr == "{}" {
		return nil
	}
	var m ModelOverrideMap
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return nil
	}
	// 过滤掉 base_model 为空的无效项
	for name, cfg := range m {
		if cfg.BaseModel == "" {
			delete(m, name)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// VirtualModelNames 返回 map 中所有虚拟模型名（按字典序排序，稳定输出）。
func (m ModelOverrideMap) VirtualModelNames() []string {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// InjectsImageGeneration 判断一个虚拟模型的 Inject 是否会向请求体注入
// image_generation 工具（或把 tool_choice 指定为 image_generation）。
// 用于前端展示派发策略：含 image_generation 的虚拟模型只能由付费账号承接。
func (o ModelOverride) InjectsImageGeneration() bool {
	if len(o.Inject) == 0 {
		return false
	}
	if raw, ok := o.Inject["tool_choice"]; ok {
		var tc struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &tc); err == nil && tc.Type == "image_generation" {
			return true
		}
	}
	raw, ok := o.Inject["tools"]
	if !ok {
		return false
	}
	var tools []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return false
	}
	for _, t := range tools {
		if t.Type == "image_generation" {
			return true
		}
	}
	return false
}

// ApplyModelOverride 在请求体进入校验/翻译流程之前，识别并改写虚拟模型请求。
//
// 如果 rawBody 里的 model 字段命中 overrides 中的某个虚拟模型，则：
//  1. 把 model 字段替换为 override.BaseModel
//  2. 把 override.Inject 的每个字段合并/覆盖到 rawBody 顶层
//
// 返回 (改写后的 body, 命中时的 override 指针)。未命中时返回原 body 和 nil。
// 不修改入参；出参是新分配的 slice（只在命中时）。
func ApplyModelOverride(rawBody []byte, overrides ModelOverrideMap) ([]byte, *ModelOverride) {
	if len(overrides) == 0 || len(rawBody) == 0 {
		return rawBody, nil
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, nil
	}

	modelRaw, ok := body["model"]
	if !ok {
		return rawBody, nil
	}
	var modelName string
	if err := json.Unmarshal(modelRaw, &modelName); err != nil {
		return rawBody, nil
	}

	override, hit := overrides[modelName]
	if !hit {
		return rawBody, nil
	}

	// 条件路由：命中 override 后检查 skip_if，若命中任一条件则跳过改写
	if shouldSkipOverride(rawBody, override.SkipIf) {
		return rawBody, nil
	}

	// 替换 model
	baseRaw, err := json.Marshal(override.BaseModel)
	if err != nil {
		return rawBody, nil
	}
	body["model"] = baseRaw

	// 合并 inject 字段（覆盖同名键）
	for k, v := range override.Inject {
		body[k] = v
	}

	out, err := json.Marshal(body)
	if err != nil {
		return rawBody, nil
	}
	// 拷贝一份 override 防止外部改动污染配置
	cpy := override
	return out, &cpy
}

// shouldSkipOverride 判断是否应跳过 override 改写。
// 多个 key 之间是 OR 语义（任一命中即跳过）。路径不存在时不视为命中。
func shouldSkipOverride(rawBody []byte, skipIf map[string]json.RawMessage) bool {
	if len(skipIf) == 0 || len(rawBody) == 0 {
		return false
	}
	for path, expectedRaw := range skipIf {
		val := gjson.GetBytes(rawBody, path)
		if !val.Exists() {
			continue
		}
		if matchJSONValue(val, expectedRaw) {
			return true
		}
	}
	return false
}

// matchJSONValue 比较 gjson 取出的值与配置的原始 JSON 值。
// 支持字符串、字符串数组、数字、布尔四种常见类型；数组内任一相等即视为匹配。
func matchJSONValue(val gjson.Result, expectedRaw json.RawMessage) bool {
	if len(expectedRaw) == 0 {
		return false
	}
	// 单个字符串
	var s string
	if err := json.Unmarshal(expectedRaw, &s); err == nil {
		return val.String() == s
	}
	// 字符串数组
	var strArr []string
	if err := json.Unmarshal(expectedRaw, &strArr); err == nil {
		for _, a := range strArr {
			if val.String() == a {
				return true
			}
		}
		return false
	}
	// 布尔
	var b bool
	if err := json.Unmarshal(expectedRaw, &b); err == nil {
		return val.Bool() == b
	}
	// 数字
	var n float64
	if err := json.Unmarshal(expectedRaw, &n); err == nil {
		return val.Num == n
	}
	// 数字数组
	var numArr []float64
	if err := json.Unmarshal(expectedRaw, &numArr); err == nil {
		for _, a := range numArr {
			if val.Num == a {
				return true
			}
		}
	}
	return false
}
