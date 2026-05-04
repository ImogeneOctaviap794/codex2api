// Package proxy：SSE + JSON 响应保活（keep-alive）工具。
//
// 背景：Cloudflare（以及多数反向代理）对同一 HTTP 连接有 100s 左右的 idle
// timeout —— 只要连接上 100s 内没有任何字节流动，就会强制断开。
// 生图场景（image_generation tool）实际耗时 30-120s，其间上游 SSE 常长时间静默，
// 若直通客户端必断。本文件提供两种保活写入器：
//
//  1. SSEWriter：用于 stream:true 路径。主循环与独立心跳 goroutine 共享，
//     周期性发送"空 delta"的标准 OpenAI chunk（或 Codex response.in_progress），
//     客户端解析时自动忽略。
//
//  2. JSONKeepaliveWriter：用于 stream:false 路径。先在生成 body 超过 grace 期
//     时提前发出 HTTP 200 + Content-Type: application/json（chunked 编码），
//     随后周期性写入 ASCII 空格保活。JSON 标准允许任意前导 whitespace，
//     所有主流解析器（encoding/json、JSON.parse、json.loads）都会跳过，
//     最终 Commit(body) 时的完整 JSON 被无损解析。
package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 默认心跳参数
const (
	DefaultHeartbeatInterval      = 20 * time.Second
	DefaultHeartbeatIdleThreshold = 15 * time.Second
	DefaultJSONKeepaliveGrace     = 30 * time.Second
)

// =====================================================================
// SSEWriter：stream:true 路径用的线程安全 SSE 输出封装
// =====================================================================

// SSEWriter 封装一个 http.ResponseWriter，允许主循环和心跳 goroutine 并发写入。
// 所有写入操作均持锁，自动 Flush，并记录最后一次写入时间供心跳判断 idle。
type SSEWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	mu        sync.Mutex
	lastWrite time.Time
	done      bool
}

// NewSSEWriter 包装一个 ResponseWriter。若底层不支持 Flusher，返回 false。
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	return &SSEWriter{w: w, flusher: f, lastWrite: time.Now()}, true
}

// WriteEvent 写入 `data: <payload>\n\n` 格式的 SSE 事件。
func (s *SSEWriter) WriteEvent(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	s.lastWrite = time.Now()
	s.flusher.Flush()
	return nil
}

// WriteRaw 写入任意原始字节（例如 `data: [DONE]\n\n` 或上游透传整行）。
func (s *SSEWriter) WriteRaw(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	if _, err := fmt.Fprint(s.w, data); err != nil {
		return err
	}
	s.lastWrite = time.Now()
	s.flusher.Flush()
	return nil
}

// SendKeepalive 尝试发送心跳。若距上次写入 < idleThreshold 则跳过（避免刚写过正常 chunk 又补一个心跳浪费带宽）。
func (s *SSEWriter) SendKeepalive(chunkSSE string, idleThreshold time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || time.Since(s.lastWrite) < idleThreshold {
		return nil
	}
	if _, err := fmt.Fprint(s.w, chunkSSE); err != nil {
		return err
	}
	s.lastWrite = time.Now()
	s.flusher.Flush()
	return nil
}

// Close 标记 writer 结束，后续调用为 no-op。通常在 handler 退出前 defer 调用。
func (s *SSEWriter) Close() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
}

// RunSSEHeartbeat 在 ctx 取消前周期性调用 w.SendKeepalive。
// interval：两次心跳间最小间隔。
// idleThreshold：距上次任何写入 < 该值则跳过本次心跳（减少冗余）。
func RunSSEHeartbeat(ctx context.Context, w *SSEWriter, chunkSSE string, interval, idleThreshold time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.SendKeepalive(chunkSSE, idleThreshold); err != nil {
				return
			}
		}
	}
}

// =====================================================================
// JSONKeepaliveWriter：stream:false 路径用的 "JSON 前导空白" 保活
// =====================================================================

// JSONKeepaliveWriter 用于 stream:false 长耗时响应（如生图）。
// 工作原理：
//   - 第一次 SendKeepalive 或 Commit 时，先发 HTTP 200 + Content-Type: application/json
//     （Go 未设 Content-Length 时默认 chunked encoding，OK）。
//   - 后续 SendKeepalive 写入单个 ASCII 空格 ' '。JSON RFC 8259 允许 value 前的任意
//     whitespace（空格/tab/\n/\r），所有主流 JSON 解析器正确跳过。
//   - Commit(body) 写入最终完整 JSON body；客户端收到的 body 形如 "   ... {json}"，
//     解析无感。
type JSONKeepaliveWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	mu        sync.Mutex
	started   bool
	done      bool
	lastWrite time.Time
}

// NewJSONKeepaliveWriter 包装 ResponseWriter；底层不支持 Flush 则返回 false。
func NewJSONKeepaliveWriter(w http.ResponseWriter) (*JSONKeepaliveWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	return &JSONKeepaliveWriter{w: w, flusher: f}, true
}

// startUnlocked 首次写入时发送响应头（必须在持锁状态下调用）。
func (j *JSONKeepaliveWriter) startUnlocked() {
	if j.started {
		return
	}
	h := j.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	// 不显式 Set Content-Length，Go net/http 会自动用 chunked transfer encoding。
	j.w.WriteHeader(http.StatusOK)
	j.started = true
	j.lastWrite = time.Now()
}

// Started 返回是否已经发送过响应头。外层错误处理时用来判断能否还走
// c.JSON(502) 这种"完整响应"，还是只能把错误包成 200 + {"error":{}} 写入 body。
func (j *JSONKeepaliveWriter) Started() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.started
}

// SendKeepalive 触发保活。若尚未 started，顺便发出 HTTP 200 header 开始 chunked stream。
func (j *JSONKeepaliveWriter) SendKeepalive(idleThreshold time.Duration) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return nil
	}
	if j.started && time.Since(j.lastWrite) < idleThreshold {
		return nil
	}
	if !j.started {
		j.startUnlocked()
		return nil
	}
	if _, err := j.w.Write([]byte(" ")); err != nil {
		return err
	}
	j.flusher.Flush()
	j.lastWrite = time.Now()
	return nil
}

// Commit 写入最终 body 并结束 writer。
//
// 关键行为：
//   - 若心跳尚未启动（started=false）：走"零影响"路径，手动设置 Content-Length
//     + WriteHeader(200) + 一次性 Write(body)，与原 c.Data 行为完全一致，
//     生成定长响应（非 chunked）。适用于短请求（grace 30s 内完成）。
//   - 若心跳已启动（started=true，已发过 200 header + 若干空格）：连接已是
//     chunked encoding，不可再设 Content-Length，直接 Write(body) 追加。
func (j *JSONKeepaliveWriter) Commit(body []byte) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return nil
	}
	j.done = true
	if !j.started {
		// 心跳未启动：模拟原 c.Data(200, "application/json", body) 的定长响应
		h := j.w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		j.w.WriteHeader(http.StatusOK)
		j.started = true
	}
	if _, err := j.w.Write(body); err != nil {
		return err
	}
	j.flusher.Flush()
	return nil
}

// Close 标记结束（不写 body），用于 defer 兜底避免心跳 goroutine 继续写。
func (j *JSONKeepaliveWriter) Close() {
	j.mu.Lock()
	j.done = true
	j.mu.Unlock()
}

// RunJSONHeartbeat 在 graceDelay 后开始周期性心跳（写空格）。
// graceDelay：给前置 connect/auth/短耗时请求一个"不发头"的缓冲期，
// 避免短请求被强制转成 chunked 并导致 Content-Length 丢失。
// 超过 graceDelay 还没 done 的请求（典型：生图）会启动心跳。
func RunJSONHeartbeat(ctx context.Context, w *JSONKeepaliveWriter, graceDelay, interval, idleThreshold time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(graceDelay):
	}
	// 立即触发一次：发 header + 开始 chunked stream
	if err := w.SendKeepalive(idleThreshold); err != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.SendKeepalive(idleThreshold); err != nil {
				return
			}
		}
	}
}

// =====================================================================
// 心跳内容构造
// =====================================================================

// BuildOpenAIHeartbeatChunk 构造 OpenAI Chat Completions 流式协议下的"空 delta" chunk。
// 客户端（OpenAI SDK、Cherry Studio、LobeChat 等）遇到空 delta 会忽略，不会追加任何显示内容。
// 输出形如：
//
//	data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1714800000,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":null}]}\n\n
func BuildOpenAIHeartbeatChunk(chunkID, model string, created int64) string {
	return fmt.Sprintf(
		`data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":null}]}`+"\n\n",
		chunkID, created, model,
	)
}

// BuildCodexHeartbeatEvent 构造 /v1/responses 透传路径使用的 Codex SSE 心跳。
// response.in_progress 是 Codex 本身就会多次发的常规事件，客户端会幂等处理。
func BuildCodexHeartbeatEvent() string {
	return "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"
}

// =====================================================================
// Handler 集成 helper：统一初始化 keepalive + 错误响应
// =====================================================================

// KeepaliveMode 选择心跳 chunk 的格式。
type KeepaliveMode int

const (
	// KeepaliveModeOpenAI 构造 {"id":...,"choices":[{"delta":{}}]} 空 delta chunk，
	// 用于 /v1/chat/completions 流式路径（newapi 等 OpenAI 兼容网关原样转发）。
	KeepaliveModeOpenAI KeepaliveMode = iota
	// KeepaliveModeCodex 构造 response.in_progress 事件，
	// 用于 /v1/responses 原生 Codex 透传路径。
	KeepaliveModeCodex
)

// SetupKeepalive 为 handler 初始化保活写入器并启动心跳 goroutine。
//
// 参数：
//   - isStream：true=SSE 路径；false=JSON 响应路径
//   - mode：SSE 心跳 chunk 的格式（仅 isStream=true 有效）
//   - chunkID/model/created：仅 KeepaliveModeOpenAI 用，传入占位即可
//
// 返回：
//   - sseW：SSE 写入器（仅 stream:true 非 nil）
//   - jsonW：JSON 保活写入器（仅 stream:false 非 nil）
//   - cancel：停止心跳 goroutine；必须在 handler 退出前调用（建议 defer）
//   - ok：false 表示响应不支持 Flush，handler 已发错误，调用方应直接 return
//
// 调用方还应：`defer sseW.Close()` / `defer jsonW.Close()`（如非 nil），
// 避免心跳 goroutine 在响应已完成后继续写入。
func SetupKeepalive(
	c *gin.Context,
	isStream bool,
	mode KeepaliveMode,
	chunkID, model string,
	created int64,
) (sseW *SSEWriter, jsonW *JSONKeepaliveWriter, cancel context.CancelFunc, ok bool) {
	ctx, cancel := context.WithCancel(c.Request.Context())
	if isStream {
		var okSSE bool
		sseW, okSSE = NewSSEWriter(c.Writer)
		if !okSSE {
			cancel()
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{"message": "streaming not supported", "type": "server_error"},
			})
			return nil, nil, cancel, false
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		var hbChunk string
		switch mode {
		case KeepaliveModeCodex:
			hbChunk = BuildCodexHeartbeatEvent()
		default:
			hbChunk = BuildOpenAIHeartbeatChunk(chunkID, model, created)
		}
		go RunSSEHeartbeat(ctx, sseW, hbChunk, DefaultHeartbeatInterval, DefaultHeartbeatIdleThreshold)
		return sseW, nil, cancel, true
	}

	var okJSON bool
	jsonW, okJSON = NewJSONKeepaliveWriter(c.Writer)
	if !okJSON {
		cancel()
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": "response writer not flushable", "type": "server_error"},
		})
		return nil, nil, cancel, false
	}
	go RunJSONHeartbeat(ctx, jsonW, DefaultJSONKeepaliveGrace, DefaultHeartbeatInterval, DefaultHeartbeatIdleThreshold)
	return nil, jsonW, cancel, true
}

// WriteErrorKA 在 keepalive 上下文下发送错误响应，正确处理三种情形：
//
//  1. stream:true（sseW != nil）：发 `data: {"error":...}\n\n` + `data: [DONE]\n\n`
//  2. stream:false 且 jsonW 已 Started（已向客户端发过 200 header/空格）：
//     必须复用该连接，把 errorJSON 作为 body 写入（status 仍是 200）。
//     OpenAI SDK 遇到 body 含 `error` 字段会正确抛 APIError。
//  3. stream:false 且 jsonW 未 Started：走正常 c.JSON(status, ...) 路径。
//
// 调用前必须已经 cancel 掉 heartbeat goroutine（避免与 Commit 竞争）。
func WriteErrorKA(
	c *gin.Context,
	sseW *SSEWriter,
	jsonW *JSONKeepaliveWriter,
	status int,
	message, errType string,
) {
	errJSON := fmt.Sprintf(`{"error":{"message":%q,"type":%q}}`, message, errType)
	if sseW != nil {
		_ = sseW.WriteEvent([]byte(errJSON))
		_ = sseW.WriteRaw("data: [DONE]\n\n")
		return
	}
	if jsonW != nil && jsonW.Started() {
		_ = jsonW.Commit([]byte(errJSON))
		return
	}
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": errType}})
}

// WriteUpstreamErrorKA 在 keepalive 上下文下透传上游原始错误 body。
// 用于替代 h.sendFinalUpstreamError：
//   - stream:true：上游 SSE error 事件已经传过，这里只保证写 [DONE]
//   - stream:false 且 started：Commit(upstreamBody) 保持 200
//   - 其他：走 c.Data(status, ...) 原样透传
func WriteUpstreamErrorKA(
	c *gin.Context,
	sseW *SSEWriter,
	jsonW *JSONKeepaliveWriter,
	status int,
	upstreamBody []byte,
) {
	if sseW != nil {
		// SSE 路径下上游错误应由 handler 在流中处理，这里仅兜底写 DONE
		_ = sseW.WriteRaw("data: [DONE]\n\n")
		return
	}
	if jsonW != nil && jsonW.Started() {
		if len(upstreamBody) == 0 {
			upstreamBody = []byte(fmt.Sprintf(`{"error":{"message":"upstream error status %d","type":"upstream_error"}}`, status))
		}
		_ = jsonW.Commit(upstreamBody)
		return
	}
	if len(upstreamBody) > 0 {
		c.Data(status, "application/json", upstreamBody)
		return
	}
	c.JSON(status, gin.H{"error": gin.H{"message": fmt.Sprintf("upstream error status %d", status), "type": "upstream_error"}})
}
