// Package proxy — dialog_collector.go
//
// 对话采集器：把每次成功的请求 / 响应异步、有损地写入 dialog_logs。
//
// 核心设计原则：**永不影响主链路**。
//
//  1. Submit 永不阻塞：channel 满直接丢，记录 dropped 计数
//  2. Submit/写入全 goroutine 异步：handler 调用 → channel → flusher → PG
//  3. panic 隔离：所有公开方法包 defer recover，确保任何子组件 bug 都不会
//     传播到客户端响应
//  4. 运行时 atomic 开关：admin API 可一键关闭，无需重启容器
//  5. 启动级 ENV 开关：DIALOG_COLLECTION_ENABLED=false 直接不创建实例
//  6. 仅 PostgreSQL：SQLite 路径自动跳过（用于本地开发）
//
// 流程：
//
//	handler.go ──submit()──> ch (cap 10000)
//	                           │
//	                           ▼
//	                       flusher goroutine
//	                           │
//	                           │ batch=200 OR 2s tick
//	                           ▼
//	                       db.InsertDialogLogs (PG multi-VALUES INSERT)
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// DialogCollector 异步对话采集器。
type DialogCollector struct {
	db      *database.DB
	ch      chan *database.DialogLogInput
	enabled atomic.Bool

	// metrics
	submitted atomic.Int64
	dropped   atomic.Int64
	written   atomic.Int64
	failed    atomic.Int64

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewDialogCollector 创建并启动采集器。
//
// enabled=false 或 db 不是 PG 时返回 nil，调用方应该判空。
func NewDialogCollector(db *database.DB, enabled bool) *DialogCollector {
	if !enabled || db == nil {
		return nil
	}
	c := &DialogCollector{
		db:   db,
		ch:   make(chan *database.DialogLogInput, 10000),
		stop: make(chan struct{}),
	}
	c.enabled.Store(true)

	// 启动后台 schema 维护 + flusher
	c.wg.Add(1)
	go c.flusher()

	return c
}

// SetEnabled 运行时开关。关闭后 Submit 立即变成 no-op；channel 中已有的会被 flush。
func (c *DialogCollector) SetEnabled(v bool) {
	if c == nil {
		return
	}
	c.enabled.Store(v)
}

// IsEnabled 当前是否在采集。
func (c *DialogCollector) IsEnabled() bool {
	if c == nil {
		return false
	}
	return c.enabled.Load()
}

// Submit 永不阻塞。channel 满直接丢、记 dropped。
func (c *DialogCollector) Submit(rec *database.DialogLogInput) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("dialog_collector: submit panic recovered: %v", r)
		}
	}()
	if c == nil || rec == nil || !c.enabled.Load() {
		return
	}
	select {
	case c.ch <- rec:
		c.submitted.Add(1)
	default:
		c.dropped.Add(1)
	}
}

// Stop 优雅停止 flusher（用于 server shutdown）。
func (c *DialogCollector) Stop(timeout time.Duration) {
	if c == nil {
		return
	}
	c.enabled.Store(false)
	close(c.stop)
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("dialog_collector: stop timeout, %d items may be lost", len(c.ch))
	}
}

// Stats 监控指标快照。
type DialogCollectorStats struct {
	Enabled   bool  `json:"enabled"`
	Submitted int64 `json:"submitted"`
	Dropped   int64 `json:"dropped"`
	Written   int64 `json:"written"`
	Failed    int64 `json:"failed"`
	QueueLen  int   `json:"queue_len"`
	QueueCap  int   `json:"queue_cap"`
}

func (c *DialogCollector) Stats() DialogCollectorStats {
	if c == nil {
		return DialogCollectorStats{}
	}
	return DialogCollectorStats{
		Enabled:   c.enabled.Load(),
		Submitted: c.submitted.Load(),
		Dropped:   c.dropped.Load(),
		Written:   c.written.Load(),
		Failed:    c.failed.Load(),
		QueueLen:  len(c.ch),
		QueueCap:  cap(c.ch),
	}
}

// flusher 后台 goroutine：批量写 PG。
//
// 触发 flush 的条件：
//   - batch 达到 200 条
//   - 距上次 flush 超过 2s
//   - 收到 stop 信号（最后一次 flush）
func (c *DialogCollector) flusher() {
	defer c.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("dialog_collector: flusher panic recovered: %v", r)
		}
	}()

	const batchSize = 200
	const flushInterval = 2 * time.Second

	batch := make([]database.DialogLogInput, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// 防御：写入也包 recover
		defer func() {
			if r := recover(); r != nil {
				log.Printf("dialog_collector: flush panic recovered: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.db.InsertDialogLogs(ctx, batch); err != nil {
			c.failed.Add(int64(len(batch)))
			log.Printf("dialog_collector: insert %d records failed: %v", len(batch), err)
		} else {
			c.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case rec := <-c.ch:
			if rec != nil {
				batch = append(batch, *rec)
			}
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.stop:
			// 排空剩余
			for {
				select {
				case rec := <-c.ch:
					if rec != nil {
						batch = append(batch, *rec)
					}
				default:
					flush()
					return
				}
				if len(batch) >= batchSize {
					flush()
				}
			}
		}
	}
}

// =====================================================================
// Helper：从 handler 上下文构造 DialogLogInput
// =====================================================================

// HashAPIKey 取 SHA256 前 16 字节 hex（32 字符）。永远不存原 key。
func HashAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:16])
}

// ExtractReasoningFromCodexEvents 从 Codex 上游 SSE 事件流（按行分割的 JSON）
// 中拼接 reasoning_content。
//
// Codex 的 reasoning delta 事件类型是 response.reasoning_text.delta（推理文本）
// 或 response.reasoning.summary_text.delta（推理摘要）。这里两类都拼上。
func ExtractReasoningFromCodexEvents(rawEvents []json.RawMessage) string {
	if len(rawEvents) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, ev := range rawEvents {
		t := gjson.GetBytes(ev, "type").String()
		switch t {
		case "response.reasoning_text.delta",
			"response.reasoning.summary_text.delta":
			sb.WriteString(gjson.GetBytes(ev, "delta").String())
		}
	}
	return sb.String()
}

// IsImageGenDialogEvent 判断是否为 image_generation 系列事件 —— 这些事件含 base64 图像帧，
// 体积可达数 MB（partial_image / output_item.done 的 result 字段），既不是训练数据，
// 又会让单个 dialog 记录膨胀几百倍。2026-05-15 事故元凶：单 image_generation 请求
// 的 response_body 可达 8 MB，10+ 并发即可吃光 GB 级内存。
//
// 黑名单覆盖：
//   - response.image_generation_call.partial_image / in_progress / generating / completed
//   - response.output_item.added / done 当 item.type == "image_generation_call"
//
// 调用方应在 dialog 采集 append 前判断；主链路转发完全不受影响。
func IsImageGenDialogEvent(eventType string, eventData []byte) bool {
	switch eventType {
	case "response.image_generation_call.partial_image",
		"response.image_generation_call.in_progress",
		"response.image_generation_call.generating",
		"response.image_generation_call.completed":
		return true
	case "response.output_item.added",
		"response.output_item.done":
		return gjson.GetBytes(eventData, "item.type").String() == "image_generation_call"
	}
	return false
}

// MergeCodexEventsAsResponseJSON 把 Codex SSE 事件序列拼成单个 JSON 数组，
// 用于 ResponseBody 字段（保留所有原始事件供后期分析）。
func MergeCodexEventsAsResponseJSON(rawEvents []json.RawMessage) json.RawMessage {
	if len(rawEvents) == 0 {
		return nil
	}
	out, err := json.Marshal(rawEvents)
	if err != nil {
		return nil
	}
	return out
}
