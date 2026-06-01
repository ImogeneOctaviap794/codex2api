package proxy

import "testing"

// 测试目标：v1.7.62 固定减法策略。
// 仅画图虚拟模型命中且上游 input_tokens > framework_tax 时，
// AdjustUsageForVirtualImage 返回深拷贝并把 prompt/input/total 减去 framework_tax；
// 其他场景一律透传原指针。

func mustImageVirtualHit(t *testing.T) *ModelOverride {
	t.Helper()
	raw := `{"x":{"base_model":"gpt-5.4-mini","inject":{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}}}`
	m := ParseModelOverrides(raw)
	v, ok := m["x"]
	if !ok {
		t.Fatal("ParseModelOverrides should yield 'x'")
	}
	if !v.InjectsImageGeneration() {
		t.Fatal("override must InjectsImageGeneration()")
	}
	return &v
}

func mustNonImageVirtualHit(t *testing.T) *ModelOverride {
	t.Helper()
	raw := `{"x":{"base_model":"gpt-5.4","inject":{"reasoning":{"effort":"high"}}}}`
	m := ParseModelOverrides(raw)
	v, ok := m["x"]
	if !ok {
		t.Fatal("ParseModelOverrides should yield 'x'")
	}
	if v.InjectsImageGeneration() {
		t.Fatal("non-image override must NOT InjectsImageGeneration()")
	}
	return &v
}

func TestAdjustUsageForVirtualImage_NilUsage(t *testing.T) {
	v := mustImageVirtualHit(t)
	if got := AdjustUsageForVirtualImage(nil, v, []byte(`{}`)); got != nil {
		t.Fatalf("nil usage must return nil, got %+v", got)
	}
}

func TestAdjustUsageForVirtualImage_NilVirtualHit_Passthrough(t *testing.T) {
	usage := &UsageInfo{InputTokens: 1628, OutputTokens: 52, TotalTokens: 1680, PromptTokens: 1628, CompletionTokens: 52}
	got := AdjustUsageForVirtualImage(usage, nil, nil)
	if got != usage {
		t.Fatal("nil virtualHit must return same pointer")
	}
}

func TestAdjustUsageForVirtualImage_NonImageVirtual_Passthrough(t *testing.T) {
	usage := &UsageInfo{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}
	v := mustNonImageVirtualHit(t)
	got := AdjustUsageForVirtualImage(usage, v, nil)
	if got != usage {
		t.Fatal("non-image virtual hit must return same pointer (no adjustment)")
	}
}

func TestAdjustUsageForVirtualImage_TextOnly_DeductsFrameworkTax(t *testing.T) {
	// v1.7.61 生产实测样本回放：upstream 1628，framework_tax 1618 → newInput 10
	usage := &UsageInfo{
		PromptTokens:     1628,
		CompletionTokens: 52,
		TotalTokens:      1680,
		InputTokens:      1628,
		OutputTokens:     52,
		ReasoningTokens:  17,
		CachedTokens:     11,
	}
	v := mustImageVirtualHit(t)
	got := AdjustUsageForVirtualImage(usage, v, nil)
	if got == usage {
		t.Fatal("image hit with input_tokens > framework_tax must return new struct, not same pointer")
	}
	wantNewInput := 1628 - ChatCompletionsImageFrameworkTax // = 10
	if got.InputTokens != wantNewInput {
		t.Fatalf("InputTokens = %d, want %d", got.InputTokens, wantNewInput)
	}
	if got.PromptTokens != wantNewInput {
		t.Fatalf("PromptTokens = %d, want %d", got.PromptTokens, wantNewInput)
	}
	if got.TotalTokens != wantNewInput+52 {
		t.Fatalf("TotalTokens = %d, want %d (newInput + OutputTokens)", got.TotalTokens, wantNewInput+52)
	}
	// 其他字段一字不动
	if got.OutputTokens != 52 {
		t.Fatalf("OutputTokens must stay 52, got %d", got.OutputTokens)
	}
	if got.CompletionTokens != 52 {
		t.Fatalf("CompletionTokens must stay 52, got %d", got.CompletionTokens)
	}
	if got.ReasoningTokens != 17 {
		t.Fatalf("ReasoningTokens must stay 17, got %d", got.ReasoningTokens)
	}
	if got.CachedTokens != 11 {
		t.Fatalf("CachedTokens must stay 11, got %d", got.CachedTokens)
	}
	// 源 usage 不被修改
	if usage.InputTokens != 1628 {
		t.Fatalf("source usage was mutated, InputTokens=%d", usage.InputTokens)
	}
	if usage.PromptTokens != 1628 {
		t.Fatalf("source usage was mutated, PromptTokens=%d", usage.PromptTokens)
	}
}

// 含图请求场景：图起图、上传 base64、含图片链接的多轮对话。
// v1.7.61 BPE 实现会把 vision tokens 错误地从 input_tokens 里一并剃掉；
// v1.7.62 的减法策略保留 vision tokens 在剩余的 (input_tokens - framework_tax) 里。
func TestAdjustUsageForVirtualImage_WithVisionTokens_PreservesVisionInRemainder(t *testing.T) {
	// 模拟：上游 input_tokens 包含 framework_tax(1618) + 文本(10) + vision(765) = 2393
	usage := &UsageInfo{
		PromptTokens: 2393,
		InputTokens:  2393,
		OutputTokens: 50,
		TotalTokens:  2443,
	}
	v := mustImageVirtualHit(t)
	got := AdjustUsageForVirtualImage(usage, v, nil)
	if got == usage {
		t.Fatal("must return new struct")
	}
	wantNewInput := 2393 - ChatCompletionsImageFrameworkTax // = 775 = 文本(10) + vision(765)
	if got.InputTokens != wantNewInput {
		t.Fatalf("InputTokens = %d, want %d (text + vision)", got.InputTokens, wantNewInput)
	}
	// 关键断言：vision tokens 在剩余值里完整保留，不会因为 messages 里有 image_url 而被吞
	if got.InputTokens < 765 {
		t.Fatalf("vision tokens (~765) must be preserved in remainder, got InputTokens=%d", got.InputTokens)
	}
	if got.TotalTokens != wantNewInput+50 {
		t.Fatalf("TotalTokens = %d, want %d", got.TotalTokens, wantNewInput+50)
	}
}

// 上游 input_tokens 等于 framework_tax：减完为 0，按 fail-safe 不调整透传。
func TestAdjustUsageForVirtualImage_EqualToFrameworkTax_Passthrough(t *testing.T) {
	usage := &UsageInfo{
		InputTokens:  ChatCompletionsImageFrameworkTax,
		PromptTokens: ChatCompletionsImageFrameworkTax,
		OutputTokens: 0,
		TotalTokens:  ChatCompletionsImageFrameworkTax,
	}
	v := mustImageVirtualHit(t)
	got := AdjustUsageForVirtualImage(usage, v, nil)
	if got != usage {
		t.Fatal("input_tokens == framework_tax must passthrough (no adjustment)")
	}
}

// 上游 input_tokens 小于 framework_tax（罕见但理论可能）：fail-safe 透传，避免负值。
func TestAdjustUsageForVirtualImage_BelowFrameworkTax_Passthrough(t *testing.T) {
	usage := &UsageInfo{
		InputTokens:  100,
		PromptTokens: 100,
		OutputTokens: 30,
		TotalTokens:  130,
	}
	v := mustImageVirtualHit(t)
	got := AdjustUsageForVirtualImage(usage, v, nil)
	if got != usage {
		t.Fatal("input_tokens < framework_tax must passthrough (no negative result)")
	}
}

// 上游 input_tokens 刚好比 framework_tax 多 1：边界值。
func TestAdjustUsageForVirtualImage_OneAboveFrameworkTax_AdjustsToOne(t *testing.T) {
	usage := &UsageInfo{
		InputTokens:  ChatCompletionsImageFrameworkTax + 1,
		PromptTokens: ChatCompletionsImageFrameworkTax + 1,
		OutputTokens: 0,
		TotalTokens:  ChatCompletionsImageFrameworkTax + 1,
	}
	v := mustImageVirtualHit(t)
	got := AdjustUsageForVirtualImage(usage, v, nil)
	if got == usage {
		t.Fatal("input_tokens > framework_tax must adjust")
	}
	if got.InputTokens != 1 {
		t.Fatalf("InputTokens = %d, want 1", got.InputTokens)
	}
	if got.TotalTokens != 1 {
		t.Fatalf("TotalTokens = %d, want 1", got.TotalTokens)
	}
}

// rawBody 参数当前实现不依赖（保留是为了未来扩展），传 nil / 空数组 / 任意值都不影响行为。
func TestAdjustUsageForVirtualImage_RawBodyIsIgnored(t *testing.T) {
	v := mustImageVirtualHit(t)
	mkUsage := func() *UsageInfo {
		return &UsageInfo{InputTokens: 1628, PromptTokens: 1628, OutputTokens: 52, TotalTokens: 1680}
	}

	gotNil := AdjustUsageForVirtualImage(mkUsage(), v, nil)
	gotEmpty := AdjustUsageForVirtualImage(mkUsage(), v, []byte{})
	gotJunk := AdjustUsageForVirtualImage(mkUsage(), v, []byte(`not even json`))
	gotJSON := AdjustUsageForVirtualImage(mkUsage(), v, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))

	for _, got := range []*UsageInfo{gotNil, gotEmpty, gotJunk, gotJSON} {
		if got.InputTokens != 1628-ChatCompletionsImageFrameworkTax {
			t.Fatalf("rawBody must not affect result, got InputTokens=%d", got.InputTokens)
		}
	}
}
