package admin

import (
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type errorResponse struct {
	Error string `json:"error"`
}

type messageResponse struct {
	Message string `json:"message"`
}

type statsResponse struct {
	Total         int   `json:"total"`
	Available     int   `json:"available"`
	Error         int   `json:"error"`
	TodayRequests int64 `json:"today_requests"`
	// 用量分布
	UsageDistribution usageDistribution `json:"usage_distribution"`
}

// usageDistribution 账号 7d 用量分布
type usageDistribution struct {
	TrackedCount int     `json:"tracked_count"` // 有用量数据的账号数
	AvgPercent   float64 `json:"avg_percent"`   // 平均 7d 用量百分比
	Low          int     `json:"low"`           // 0-25%
	Medium       int     `json:"medium"`        // 25-50%
	High         int     `json:"high"`          // 50-75%
	Critical     int     `json:"critical"`      // 75-100%
	Exhausted    int     `json:"exhausted"`     // >=100%
}

type accountsResponse struct {
	Accounts []accountResponse `json:"accounts"`
}

// accountsPagedResponse 分页版账号列表响应
type accountsPagedResponse struct {
	Accounts []accountResponse `json:"accounts"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Summary  accountSummary    `json:"summary"`
}

// accountSummary 账号汇总计数（在筛选前统计）
type accountSummary struct {
	Total          int `json:"total"`
	Normal         int `json:"normal"`
	RateLimited    int `json:"rate_limited"`    // 软限速：free 7d ∈ [90%, 110%)，仍可调度
	UsageExhausted int `json:"usage_exhausted"` // 用量耗尽：free 7d ≥ 110% 或上游 5h 限流，已硬阻断
	Banned         int `json:"banned"`
	Locked         int `json:"locked"`
	Healthy        int `json:"healthy"`
	Warm           int `json:"warm"`
	Risky          int `json:"risky"`
}

type createAccountResponse struct {
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

type healthResponse struct {
	Status    string `json:"status"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
}

type usageLogsResponse struct {
	Logs []*database.UsageLog `json:"logs"`
}

type apiKeysResponse struct {
	Keys []*MaskedAPIKeyRow `json:"keys"`
}

// MaskedAPIKeyRow API Key 响应（含脱敏和完整 key）
type MaskedAPIKeyRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	RawKey    string `json:"raw_key"`
	CreatedAt string `json:"created_at"`
}

// NewMaskedAPIKeyRow 创建 API Key 响应
func NewMaskedAPIKeyRow(row *database.APIKeyRow) *MaskedAPIKeyRow {
	return &MaskedAPIKeyRow{
		ID:        row.ID,
		Name:      row.Name,
		Key:       security.MaskAPIKey(row.Key),
		RawKey:    row.Key,
		CreatedAt: row.CreatedAt.Format(time.RFC3339),
	}
}

type createAPIKeyResponse struct {
	ID   int64  `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

type opsOverviewResponse struct {
	UpdatedAt      string              `json:"updated_at"`
	UptimeSeconds  int64               `json:"uptime_seconds"`
	DatabaseDriver string              `json:"database_driver"`
	DatabaseLabel  string              `json:"database_label"`
	CacheDriver    string              `json:"cache_driver"`
	CacheLabel     string              `json:"cache_label"`
	CPU            opsCPUResponse      `json:"cpu"`
	Memory         opsMemoryResponse   `json:"memory"`
	Runtime        opsRuntimeResponse  `json:"runtime"`
	Requests       opsRequestsResponse `json:"requests"`
	Postgres       opsDatabaseResponse `json:"postgres"`
	Redis          opsRedisResponse    `json:"redis"`
	Traffic        opsTrafficResponse  `json:"traffic"`
}

type opsCPUResponse struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
}

type opsMemoryResponse struct {
	Percent      float64 `json:"percent"`
	UsedBytes    uint64  `json:"used_bytes"`
	TotalBytes   uint64  `json:"total_bytes"`
	ProcessBytes uint64  `json:"process_bytes"`
}

type opsRuntimeResponse struct {
	Goroutines        int `json:"goroutines"`
	AvailableAccounts int `json:"available_accounts"`
	TotalAccounts     int `json:"total_accounts"`
}

type opsRequestsResponse struct {
	Active int64 `json:"active"`
	Total  int64 `json:"total"`
}

type opsDatabaseResponse struct {
	Healthy      bool    `json:"healthy"`
	Open         int     `json:"open"`
	InUse        int     `json:"in_use"`
	Idle         int     `json:"idle"`
	MaxOpen      int     `json:"max_open"`
	WaitCount    int64   `json:"wait_count"`
	UsagePercent float64 `json:"usage_percent"`
}

type opsRedisResponse struct {
	Healthy      bool    `json:"healthy"`
	TotalConns   uint32  `json:"total_conns"`
	IdleConns    uint32  `json:"idle_conns"`
	StaleConns   uint32  `json:"stale_conns"`
	PoolSize     int     `json:"pool_size"`
	UsagePercent float64 `json:"usage_percent"`
}

type opsTrafficResponse struct {
	QPS           float64 `json:"qps"`
	QPSPeak       float64 `json:"qps_peak"`
	TPS           float64 `json:"tps"`
	TPSPeak       float64 `json:"tps_peak"`
	RPM           float64 `json:"rpm"`
	TPM           float64 `json:"tpm"`
	ErrorRate     float64 `json:"error_rate"`
	TodayRequests int64   `json:"today_requests"`
	TodayTokens   int64   `json:"today_tokens"`
	RPMLimit      int     `json:"rpm_limit"`
}

func writeError(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, errorResponse{Error: message})
}

func writeMessage(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, messageResponse{Message: message})
}

func writeInternalError(c *gin.Context, err error) {
	writeError(c, http.StatusInternalServerError, err.Error())
}
