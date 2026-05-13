// Package database — dialog_logs.go
//
// 对话原始数据采集表（仅 PostgreSQL）：按月 RANGE 分区存储。
//
// 设计要点：
//   - 完全独立于现有 usage_logs / accounts 等表，不影响主链路任何已有逻辑。
//   - 主表 dialog_logs (PARTITION BY RANGE(ts)) + 子分区 dialog_logs_YYYY_MM。
//   - 所有写入走 INSERT INTO dialog_logs，由 PG 路由到对应分区。
//   - 启动时确保当月 + 下月分区存在；后台任务每天检查一次（避免月初首次写入卡 DDL）。
//   - 任何写入错误只 log 不抛回主链路（dialog_collector.go 再做兜底）。
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// DialogLogInput 单条对话采集记录。
//
// 所有 JSON 字段使用 json.RawMessage 直接落盘 JSONB，不做反序列化校验
// （PG 自身会校验合法性；非合法 JSON 由 InsertDialogLogs 兜底丢弃）。
type DialogLogInput struct {
	Timestamp        time.Time
	Endpoint         string          // /v1/chat/completions, /v1/responses, /v1/responses/compact
	Model            string          // gpt-5, gpt-5-codex, gpt-5.4, ...
	BaseModel        string          // 虚拟模型对应的 base_model（画图场景）
	AccountID        int64           // 命中的上游账号 ID
	APIKeyHash       string          // 调用方 API key 的 SHA256 前 16 字节 hex（不存原文）
	SessionID        string          // 客户端会话 ID，同一会话下多次调用共享（采购规范必填）
	RequestID        string          // 单次调用唯一 ID，UUID（采购规范必填）
	IsStream         bool            // 客户端 stream:true / false
	RequestBody      json.RawMessage // 原始请求 JSON
	ResponseBody     json.RawMessage // 完整响应 JSON（流式拼合后；非流式直接是 body）
	ReasoningContent string          // 拼接后的 reasoning_content（便于训练时直接读）
	ToolCalls        json.RawMessage // 拼接后的 tool_calls 数组（可空）
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	CachedTokens     int
	DurationMs       int
	StatusCode       int
	ServiceTier      string
	ReasoningEffort  string
}

// EnsureDialogLogsSchema 创建 dialog_logs 主表（仅 PostgreSQL，SQLite 直接跳过）。
//
// 主表使用 RANGE 分区，子分区在 EnsureDialogPartition 中按月创建。
// 函数 idempotent：CREATE TABLE IF NOT EXISTS。
func (db *DB) EnsureDialogLogsSchema(ctx context.Context) error {
	if db == nil || db.driver != "postgres" {
		return nil
	}
	const ddl = `
CREATE TABLE IF NOT EXISTS dialog_logs (
    id               BIGSERIAL,
    ts               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    endpoint         TEXT NOT NULL,
    model            TEXT,
    base_model       TEXT,
    account_id       BIGINT,
    api_key_hash     TEXT,
    is_stream        BOOLEAN,
    request_body     JSONB,
    response_body    JSONB,
    reasoning_content TEXT,
    tool_calls       JSONB,
    prompt_tokens    INTEGER,
    completion_tokens INTEGER,
    reasoning_tokens INTEGER,
    cached_tokens    INTEGER,
    duration_ms      INTEGER,
    status_code      INTEGER,
    service_tier     TEXT,
    reasoning_effort TEXT,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);

CREATE INDEX IF NOT EXISTS dialog_logs_ts_idx ON dialog_logs (ts);
CREATE INDEX IF NOT EXISTS dialog_logs_model_idx ON dialog_logs (model);
CREATE INDEX IF NOT EXISTS dialog_logs_account_idx ON dialog_logs (account_id);

-- v1.7.54 采购规范必填字段：session_id (同会话多 call 关联) + request_id (单 call 全局唯一)
ALTER TABLE dialog_logs ADD COLUMN IF NOT EXISTS session_id TEXT;
ALTER TABLE dialog_logs ADD COLUMN IF NOT EXISTS request_id TEXT;
CREATE INDEX IF NOT EXISTS dialog_logs_session_idx ON dialog_logs (session_id);
CREATE INDEX IF NOT EXISTS dialog_logs_request_idx ON dialog_logs (request_id);
`
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create dialog_logs: %w", err)
	}
	return nil
}

// EnsureDialogPartition 确保指定时间所在月份的分区存在。idempotent。
//
// 分区命名：dialog_logs_YYYY_MM
// 范围：[YYYY-MM-01, YYYY-(MM+1)-01)
func (db *DB) EnsureDialogPartition(ctx context.Context, t time.Time) error {
	if db == nil || db.driver != "postgres" {
		return nil
	}
	t = t.UTC()
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := first.AddDate(0, 1, 0)
	partName := fmt.Sprintf("dialog_logs_%04d_%02d", first.Year(), int(first.Month()))
	ddl := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF dialog_logs FOR VALUES FROM ('%s') TO ('%s');`,
		partName,
		first.Format("2006-01-02"),
		next.Format("2006-01-02"),
	)
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create partition %s: %w", partName, err)
	}
	return nil
}

// EnsureDialogPartitionsAround 一次性确保 [t-1month, t+2months] 之间所有月分区都存在。
// 启动时调用一次，月底接近时也安全（已经预创建了下月）。
func (db *DB) EnsureDialogPartitionsAround(ctx context.Context, t time.Time) error {
	if db == nil || db.driver != "postgres" {
		return nil
	}
	for offset := -1; offset <= 2; offset++ {
		if err := db.EnsureDialogPartition(ctx, t.AddDate(0, offset, 0)); err != nil {
			return err
		}
	}
	return nil
}

// InsertDialogLogs 批量写入对话采集记录。
//
// 使用 multi-VALUES INSERT（一次最多 100 条，避免单 SQL 过大）。
// 任何错误只返回供 collector 记录日志，不应抛回主链路。
func (db *DB) InsertDialogLogs(ctx context.Context, batch []DialogLogInput) error {
	if db == nil || db.driver != "postgres" || len(batch) == 0 {
		return nil
	}

	const cols = `(ts, endpoint, model, base_model, account_id, api_key_hash, is_stream,
		request_body, response_body, reasoning_content, tool_calls,
		prompt_tokens, completion_tokens, reasoning_tokens, cached_tokens,
		duration_ms, status_code, service_tier, reasoning_effort,
		session_id, request_id)`
	const numCols = 21

	// 分块以避免单 SQL 参数过多（PG 上限 65535）
	const maxRowsPerStmt = 100
	for start := 0; start < len(batch); start += maxRowsPerStmt {
		end := start + maxRowsPerStmt
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[start:end]

		var placeholders []string
		args := make([]interface{}, 0, len(chunk)*numCols)
		idx := 1
		for _, r := range chunk {
			ph := make([]string, numCols)
			for i := range ph {
				ph[i] = fmt.Sprintf("$%d", idx)
				idx++
			}
			placeholders = append(placeholders, "("+strings.Join(ph, ",")+")")

			// 处理 nullable JSONB：空 raw 用 nil 让 PG 写 NULL
			var reqBody, respBody, toolCalls interface{}
			if len(r.RequestBody) > 0 && json.Valid(r.RequestBody) {
				reqBody = []byte(r.RequestBody)
			}
			if len(r.ResponseBody) > 0 && json.Valid(r.ResponseBody) {
				respBody = []byte(r.ResponseBody)
			}
			if len(r.ToolCalls) > 0 && json.Valid(r.ToolCalls) {
				toolCalls = []byte(r.ToolCalls)
			}

			ts := r.Timestamp
			if ts.IsZero() {
				ts = time.Now()
			}
			var accountID interface{}
			if r.AccountID > 0 {
				accountID = r.AccountID
			}

			args = append(args,
				ts,
				r.Endpoint,
				nullableString(r.Model),
				nullableString(r.BaseModel),
				accountID,
				nullableString(r.APIKeyHash),
				r.IsStream,
				reqBody,
				respBody,
				nullableString(r.ReasoningContent),
				toolCalls,
				r.PromptTokens,
				r.CompletionTokens,
				r.ReasoningTokens,
				r.CachedTokens,
				r.DurationMs,
				r.StatusCode,
				nullableString(r.ServiceTier),
				nullableString(r.ReasoningEffort),
				nullableString(r.SessionID),
				nullableString(r.RequestID),
			)
		}

		stmt := fmt.Sprintf(
			"INSERT INTO dialog_logs %s VALUES %s",
			cols,
			strings.Join(placeholders, ","),
		)
		if _, err := db.conn.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("insert dialog_logs: %w", err)
		}
	}
	return nil
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// DialogStats 采集统计信息（用于 admin 监控）。
type DialogStats struct {
	TotalRows     int64    `json:"total_rows"`
	Today         int64    `json:"today"`
	Yesterday     int64    `json:"yesterday"`
	Last7Days     int64    `json:"last_7d"`
	TableSize     string   `json:"table_size"`
	Partitions    []string `json:"partitions"`
	OldestTs      sql.NullTime `json:"-"`
	NewestTs      sql.NullTime `json:"-"`
	OldestTsStr   string   `json:"oldest_ts,omitempty"`
	NewestTsStr   string   `json:"newest_ts,omitempty"`
}

// =====================================================================
// 抽样浏览：列表 + 详情
// =====================================================================

// DialogLogSummary 列表行（不含 request/response body 大字段，避免响应过大）。
type DialogLogSummary struct {
	ID               int64     `json:"id"`
	Timestamp        time.Time `json:"ts"`
	Endpoint         string    `json:"endpoint"`
	Model            string    `json:"model"`
	BaseModel        string    `json:"base_model,omitempty"`
	AccountID        int64     `json:"account_id"`
	APIKeyHash       string    `json:"api_key_hash"`
	IsStream         bool      `json:"is_stream"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	ReasoningTokens  int       `json:"reasoning_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	DurationMs       int       `json:"duration_ms"`
	StatusCode       int       `json:"status_code"`
	ServiceTier      string    `json:"service_tier,omitempty"`
	ReasoningEffort  string    `json:"reasoning_effort,omitempty"`
	RequestSize      int       `json:"request_size"`
	ResponseSize     int       `json:"response_size"`
	HasReasoning     bool      `json:"has_reasoning"`
	HasToolCalls     bool      `json:"has_tool_calls"`
}

// DialogLogListParams 列表查询参数。
type DialogLogListParams struct {
	Endpoint string
	Model    string
	Limit    int
	Offset   int
}

// ListDialogLogs 分页查询对话采集记录列表，按 ts 降序。
//
// 返回 (rows, total)。total 不带筛选时是全表估算（fast path），带筛选时是精确 count。
func (db *DB) ListDialogLogs(ctx context.Context, p DialogLogListParams) ([]*DialogLogSummary, int64, error) {
	if db == nil || db.driver != "postgres" {
		return nil, 0, nil
	}
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	args := []interface{}{}
	whereParts := []string{}
	idx := 1
	if p.Endpoint != "" {
		whereParts = append(whereParts, fmt.Sprintf("endpoint = $%d", idx))
		args = append(args, p.Endpoint)
		idx++
	}
	if p.Model != "" {
		whereParts = append(whereParts, fmt.Sprintf("model = $%d", idx))
		args = append(args, p.Model)
		idx++
	}
	whereSQL := ""
	if len(whereParts) > 0 {
		whereSQL = "WHERE " + strings.Join(whereParts, " AND ")
	}

	// 1) total
	var total int64
	if len(whereParts) == 0 {
		// fast path：用 pg_class 估算（避免全表 count）
		_ = db.conn.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(reltuples)::BIGINT, 0)
			   FROM pg_class c
			   JOIN pg_inherits i ON c.oid = i.inhrelid
			  WHERE i.inhparent = 'dialog_logs'::regclass`,
		).Scan(&total)
	} else {
		// 带筛选：精确 count
		countSQL := "SELECT COUNT(*) FROM dialog_logs " + whereSQL
		if err := db.conn.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count dialog_logs: %w", err)
		}
	}

	// 2) rows
	listSQL := fmt.Sprintf(`
		SELECT id, ts, endpoint,
			COALESCE(model,'') AS model,
			COALESCE(base_model,'') AS base_model,
			COALESCE(account_id, 0) AS account_id,
			COALESCE(api_key_hash,'') AS api_key_hash,
			COALESCE(is_stream, FALSE) AS is_stream,
			COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0),
			COALESCE(reasoning_tokens,0), COALESCE(cached_tokens,0),
			COALESCE(duration_ms,0), COALESCE(status_code,0),
			COALESCE(service_tier,''), COALESCE(reasoning_effort,''),
			COALESCE(octet_length(request_body::text), 0) AS req_size,
			COALESCE(octet_length(response_body::text), 0) AS resp_size,
			(reasoning_content IS NOT NULL AND reasoning_content <> '') AS has_reasoning,
			(tool_calls IS NOT NULL) AS has_tool_calls
		FROM dialog_logs
		%s
		ORDER BY ts DESC
		LIMIT $%d OFFSET $%d
	`, whereSQL, idx, idx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := db.conn.QueryContext(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list dialog_logs: %w", err)
	}
	defer rows.Close()

	out := make([]*DialogLogSummary, 0, p.Limit)
	for rows.Next() {
		var r DialogLogSummary
		if err := rows.Scan(
			&r.ID, &r.Timestamp, &r.Endpoint,
			&r.Model, &r.BaseModel, &r.AccountID, &r.APIKeyHash, &r.IsStream,
			&r.PromptTokens, &r.CompletionTokens, &r.ReasoningTokens, &r.CachedTokens,
			&r.DurationMs, &r.StatusCode, &r.ServiceTier, &r.ReasoningEffort,
			&r.RequestSize, &r.ResponseSize, &r.HasReasoning, &r.HasToolCalls,
		); err != nil {
			return nil, 0, fmt.Errorf("scan dialog_logs: %w", err)
		}
		out = append(out, &r)
	}
	return out, total, nil
}

// DialogLogDetail 完整记录（含 body）。
type DialogLogDetail struct {
	DialogLogSummary
	RequestBody      json.RawMessage `json:"request_body,omitempty"`
	ResponseBody     json.RawMessage `json:"response_body,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
}

// GetDialogLogByID 按 id 查询单条记录（含完整 body）。
//
// PRIMARY KEY 是 (id, ts)，按 id 查会跨分区扫描；分区少（< 12 个）时影响很小。
func (db *DB) GetDialogLogByID(ctx context.Context, id int64) (*DialogLogDetail, error) {
	if db == nil || db.driver != "postgres" {
		return nil, nil
	}
	row := db.conn.QueryRowContext(ctx, `
		SELECT id, ts, endpoint,
			COALESCE(model,''),
			COALESCE(base_model,''),
			COALESCE(account_id, 0),
			COALESCE(api_key_hash,''),
			COALESCE(is_stream, FALSE),
			COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0),
			COALESCE(reasoning_tokens,0), COALESCE(cached_tokens,0),
			COALESCE(duration_ms,0), COALESCE(status_code,0),
			COALESCE(service_tier,''), COALESCE(reasoning_effort,''),
			COALESCE(octet_length(request_body::text), 0),
			COALESCE(octet_length(response_body::text), 0),
			(reasoning_content IS NOT NULL AND reasoning_content <> ''),
			(tool_calls IS NOT NULL),
			request_body, response_body,
			COALESCE(reasoning_content,''), tool_calls
		FROM dialog_logs WHERE id = $1
	`, id)

	var r DialogLogDetail
	var reqBody, respBody, toolCalls sql.NullString
	if err := row.Scan(
		&r.ID, &r.Timestamp, &r.Endpoint,
		&r.Model, &r.BaseModel, &r.AccountID, &r.APIKeyHash, &r.IsStream,
		&r.PromptTokens, &r.CompletionTokens, &r.ReasoningTokens, &r.CachedTokens,
		&r.DurationMs, &r.StatusCode, &r.ServiceTier, &r.ReasoningEffort,
		&r.RequestSize, &r.ResponseSize, &r.HasReasoning, &r.HasToolCalls,
		&reqBody, &respBody, &r.ReasoningContent, &toolCalls,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get dialog_log %d: %w", id, err)
	}
	if reqBody.Valid {
		r.RequestBody = json.RawMessage(reqBody.String)
	}
	if respBody.Valid {
		r.ResponseBody = json.RawMessage(respBody.String)
	}
	if toolCalls.Valid {
		r.ToolCalls = json.RawMessage(toolCalls.String)
	}
	return &r, nil
}

// GetDialogStats 查询 dialog_logs 当前规模。仅 PG。
func (db *DB) GetDialogStats(ctx context.Context) (*DialogStats, error) {
	if db == nil || db.driver != "postgres" {
		return &DialogStats{}, nil
	}

	stats := &DialogStats{}

	// 总行数 + 时间范围
	row := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(COUNT(*),0), MIN(ts), MAX(ts) FROM dialog_logs
	`)
	if err := row.Scan(&stats.TotalRows, &stats.OldestTs, &stats.NewestTs); err != nil {
		return nil, fmt.Errorf("count dialog_logs: %w", err)
	}
	if stats.OldestTs.Valid {
		stats.OldestTsStr = stats.OldestTs.Time.Format(time.RFC3339)
	}
	if stats.NewestTs.Valid {
		stats.NewestTsStr = stats.NewestTs.Time.Format(time.RFC3339)
	}

	// 今天 / 昨天 / 7d
	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	week := today.AddDate(0, 0, -7)
	if err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dialog_logs WHERE ts >= $1`, today,
	).Scan(&stats.Today); err != nil {
		log.Printf("dialog stats today: %v", err)
	}
	if err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dialog_logs WHERE ts >= $1 AND ts < $2`, yesterday, today,
	).Scan(&stats.Yesterday); err != nil {
		log.Printf("dialog stats yesterday: %v", err)
	}
	if err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dialog_logs WHERE ts >= $1`, week,
	).Scan(&stats.Last7Days); err != nil {
		log.Printf("dialog stats 7d: %v", err)
	}

	// 表大小（含所有分区）
	if err := db.conn.QueryRowContext(ctx, `
		SELECT pg_size_pretty(pg_total_relation_size('dialog_logs'))
	`).Scan(&stats.TableSize); err != nil {
		log.Printf("dialog stats size: %v", err)
	}

	// 分区列表
	rows, err := db.conn.QueryContext(ctx, `
		SELECT inhrelid::regclass::text
		FROM pg_inherits
		WHERE inhparent = 'dialog_logs'::regclass
		ORDER BY 1
	`)
	if err == nil {
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				stats.Partitions = append(stats.Partitions, name)
			}
		}
		rows.Close()
	}

	return stats, nil
}
