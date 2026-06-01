package proxy

// usage_adjust.go
//
// 画图虚拟模型场景下的对外响应 usage 调整（v1.7.62 重构：固定减法）。
//
// 背景：codex2api 把 ChatCompletions 当作画图通道（虚拟模型 gpt-image-2 等
// 命中后，inject image_generation tool + codex backend 注入 system instructions
// + image_generation tool schema），实测上游回报的 prompt_tokens 中约 1618 是
// 与用户输入无关的"框架税"（codex backend 协议要求的 system prompt + tool
// schema），其余才是用户实际输入对应的部分。
//
// v1.7.61 旧实现走本地 BPE（o200k_base）重算“用户 messages 输入”，问题是它只
// 能覆盖文本 part，image_url / input_image / file 等多模态部分被索性跳过，
// 导致含图请求（图起图 / 含 base64 / 含图片链接多轮对话）的 vision tokens 也被
// 错误地从上游 prompt_tokens 里一并剔掉，客户端看到的成本异常偏低。
//
// v1.7.62 改为“减法”策略：不再估算用户输入，直接从上游 input_tokens 里减掉
// 固定的框架税。这样：
//   - vision tokens 自动保留（在 input_tokens - framework_tax 里）
//   - 历史对话 / tools / 其他多模态 part 也都保留
//   - 零 BPE 依赖、零 image 解析、零词表 drift 风险
//
// 框架税取值依据：v1.7.61 实测样本“画一个女孩跳舞”：
//   upstream prompt_tokens = 1628，BPE 重算 user input = 10 (perMessageOverhead 4 +
//     role 1 + 5 个中文字符)，差值 = 1618 → framework_tax = 1618。
//
// 关键约束（同 v1.7.61）：
//   - 仅影响对外响应（BuildCompactResponse / newFinalChunk 序列化前）。
//   - **不影响上游实际请求**：发给 OpenAI 的 body 一字不动。
//   - **不影响 usage_logs 入库**：handler 入库走原始 *UsageInfo 不变。
//   - **不影响 account_billed**：billing.go 是按入库的 input_tokens 算的，所以
//     成本面板和 OpenAI 后台账单仍然一致。
//   - 上游 input_tokens ≤ framework_tax → 不调整，原 usage 透传（fail-safe）。

// ChatCompletionsImageFrameworkTax 画图请求走 ChatCompletions 通道时，codex backend
// 注入 system prompt + image_generation tool schema 带来的固定框架税。
//
// 数值来源：v1.7.61 生产实测（cx.wyzai.top）：
//   - 请求：{"model":"gpt-image-2","messages":[{"role":"user","content":"画一个女孩跳舞"}]}
//   - 上游返：prompt_tokens=1628，output_tokens=52
//   - BPE 重算 messages：10 tokens (perMessageOverhead 4 + "user" 1 + 5 个中文字符 ~5)
//   - framework_tax = 1628 - 10 = 1618
const ChatCompletionsImageFrameworkTax = 1618

// AdjustUsageForVirtualImage 在画图虚拟模型命中时，从上游 input_tokens 里减掉
// 固定框架税，返回一个新的 *UsageInfo。
//
// 行为：
//  1. usage 为 nil → 返回 nil
//  2. virtualHit 为 nil / 不是 image_generation 注入 → 原指针透传
//  3. 上游 input_tokens ≤ framework_tax → 原指针透传（上游报了个不可能的偏小值，
//     减了会负，保守不动）
//  4. 否则：返回深拷贝，prompt/input/total 减掉 framework_tax，output/cached/
//     reasoning 等其他字段一字不动。减后的 input_tokens 仍包含：
//       - 用户文本 messages
//       - vision tokens（image_url / input_image / base64 多模态部分）
//       - 历史对话
//       - 用户传的 tools schema
//
// 保留 v1.7.61 接口 (usage, virtualHit, rawBody) 不变使调用方零修改。rawBody
// 参数仅供未来扩展使用（如动态检测 user-supplied tools 增量抵决），当前实现
// 不依赖事。
func AdjustUsageForVirtualImage(usage *UsageInfo, virtualHit *ModelOverride, rawBody []byte) *UsageInfo {
	if usage == nil {
		return nil
	}
	if virtualHit == nil || !virtualHit.InjectsImageGeneration() {
		return usage
	}
	if usage.InputTokens <= ChatCompletionsImageFrameworkTax {
		return usage
	}

	newInput := usage.InputTokens - ChatCompletionsImageFrameworkTax

	// 深拷贝（UsageInfo 全部是值类型字段，直接复制即可）。
	cp := *usage
	cp.PromptTokens = newInput
	cp.InputTokens = newInput
	cp.TotalTokens = newInput + cp.OutputTokens
	return &cp
}

