package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// =========================================================================
// 账号去重（按邮箱）
// =========================================================================
//
// 设计原则（严格遵循"一定要谨慎"）：
//   1. 预览 + 确认两段式：先 GET 列出所有重复组，用户 review 后再 POST 执行。
//   2. 使用软删除（SoftDeleteAccount）：status='deleted'，24h 内可回滚。
//   3. 跨 platform 不算重复：同 email 但 platform 不同（openai/anthropic）保留全部。
//   4. 忽略空 email：无法识别的账号不参与去重。
//   5. Winner 规则（从高到低打分）：
//        - has_rt      +100  有 refresh_token（能续命）
//        - is_pro_tier  +50  plan_type != free
//        - locked       +30  已手动锁定（珍贵账号）
//        - active       +10  status=active（非 banned/error）
//        - recent_used   +5  last_used_at 在 7 天内
//        - 最终平局按 id ASC（最早导入优先）
//
// 路由：
//   GET  /api/admin/accounts/duplicates  → ListDuplicateAccounts
//   POST /api/admin/accounts/dedupe      → DedupeAccounts
// =========================================================================

// duplicateAccountInfo 单条账号的简要信息（前端展示用）
type duplicateAccountInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Platform   string `json:"platform"`
	Status     string `json:"status"`
	PlanType   string `json:"plan_type"`
	HasRT      bool   `json:"has_rt"`
	HasAT      bool   `json:"has_at"`
	Locked     bool   `json:"locked"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	Score      int    `json:"score"` // 打分详情，方便前端展示 winner 判定依据
}

// duplicateGroup 一组重复账号
type duplicateGroup struct {
	Email        string                 `json:"email"`          // 归一化后的 email（lowercase）
	Platform     string                 `json:"platform"`       // 共享的 platform
	Winner       duplicateAccountInfo   `json:"winner"`         // 将保留的账号
	Losers       []duplicateAccountInfo `json:"losers"`         // 将被软删的账号
	TotalInGroup int                    `json:"total_in_group"` // 含 winner 的总数
}

// scoreAccount 按 Winner 规则计算账号得分，用于组内排序挑 winner。
// 得分越高越应该被保留。
func scoreAccount(row *database.AccountRow, lastUsedAt time.Time) int {
	score := 0
	if row.GetCredential("refresh_token") != "" {
		score += 100
	}
	plan := strings.ToLower(strings.TrimSpace(row.GetCredential("plan_type")))
	if plan != "" && plan != "free" {
		score += 50
	}
	if row.Locked {
		score += 30
	}
	if row.Status == "active" {
		score += 10
	}
	if !lastUsedAt.IsZero() && time.Since(lastUsedAt) < 7*24*time.Hour {
		score += 5
	}
	return score
}

// normalizeEmail 归一化 email 用于分组比较（lowercase + trim）
func normalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// computeDuplicateGroups 核心分组算法，可单独单元测试。
//
// 参数:
//   - rows: 从数据库查到的非 deleted 账号
//   - lastUsedAtFn: 返回指定 accountID 的 last_used_at；传 nil 跳过 recent_used 加分
//
// 返回: 所有 size >= 2 的重复组，按 email 升序稳定排序
func computeDuplicateGroups(rows []*database.AccountRow, lastUsedAtFn func(int64) time.Time) []duplicateGroup {
	type groupKey struct {
		email    string
		platform string
	}
	buckets := make(map[groupKey][]*database.AccountRow)

	for _, r := range rows {
		email := normalizeEmail(r.GetCredential("email"))
		if email == "" {
			continue // 谨慎原则：空 email 跳过
		}
		platform := strings.ToLower(strings.TrimSpace(r.Platform))
		if platform == "" {
			platform = "openai" // 历史数据默认值
		}
		key := groupKey{email: email, platform: platform}
		buckets[key] = append(buckets[key], r)
	}

	var groups []duplicateGroup
	for key, bucket := range buckets {
		if len(bucket) < 2 {
			continue // 只有一条不算重复
		}

		// 组内按 score 降序，平局时 id ASC
		sort.SliceStable(bucket, func(i, j int) bool {
			var tI, tJ time.Time
			if lastUsedAtFn != nil {
				tI = lastUsedAtFn(bucket[i].ID)
				tJ = lastUsedAtFn(bucket[j].ID)
			}
			sI := scoreAccount(bucket[i], tI)
			sJ := scoreAccount(bucket[j], tJ)
			if sI != sJ {
				return sI > sJ
			}
			return bucket[i].ID < bucket[j].ID
		})

		toInfo := func(r *database.AccountRow) duplicateAccountInfo {
			var lastUsedStr string
			var lastUsed time.Time
			if lastUsedAtFn != nil {
				lastUsed = lastUsedAtFn(r.ID)
				if !lastUsed.IsZero() {
					lastUsedStr = lastUsed.Format(time.RFC3339)
				}
			}
			return duplicateAccountInfo{
				ID:         r.ID,
				Name:       r.Name,
				Email:      r.GetCredential("email"),
				Platform:   r.Platform,
				Status:     r.Status,
				PlanType:   r.GetCredential("plan_type"),
				HasRT:      r.GetCredential("refresh_token") != "",
				HasAT:      r.GetCredential("access_token") != "",
				Locked:     r.Locked,
				CreatedAt:  r.CreatedAt.Format(time.RFC3339),
				LastUsedAt: lastUsedStr,
				Score:      scoreAccount(r, lastUsed),
			}
		}

		winner := toInfo(bucket[0])
		losers := make([]duplicateAccountInfo, 0, len(bucket)-1)
		for _, r := range bucket[1:] {
			losers = append(losers, toInfo(r))
		}
		groups = append(groups, duplicateGroup{
			Email:        key.email,
			Platform:     key.platform,
			Winner:       winner,
			Losers:       losers,
			TotalInGroup: len(bucket),
		})
	}

	// 稳定输出：按 email 升序
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Email != groups[j].Email {
			return groups[i].Email < groups[j].Email
		}
		return groups[i].Platform < groups[j].Platform
	})
	return groups
}

// ListDuplicateAccounts GET /api/admin/accounts/duplicates
//
// 只读操作：分析所有非 deleted 账号，按 (email, platform) 分组，返回所有重复组。
// 忽略空 email 的账号。
func (h *Handler) ListDuplicateAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.db.ListNonDeleted(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 从内存池拿 last_used_at（数据库没有这个字段，只在 runtime Account 里）
	lastUsedFn := func(id int64) time.Time {
		for _, acc := range h.store.Accounts() {
			if acc.DBID == id {
				return acc.GetLastUsedAt()
			}
		}
		return time.Time{}
	}

	groups := computeDuplicateGroups(rows, lastUsedFn)

	totalLosers := 0
	for _, g := range groups {
		totalLosers += len(g.Losers)
	}

	c.JSON(http.StatusOK, gin.H{
		"groups":            groups,
		"total_groups":      len(groups),
		"total_losers":      totalLosers,
		"scanned_accounts":  len(rows),
		"winner_rule":       "has_rt(+100) > plan!=free(+50) > locked(+30) > active(+10) > recent_used(+5) > id ASC",
	})
}

// DedupeAccounts POST /api/admin/accounts/dedupe
//
// 请求体: { "loser_ids": [1, 2, 3, ...] }
// 操作: 批量软删（status='deleted'），从内存池移除，记录 account_events。
// 谨慎措施:
//   - 只接受前端显式传过来的 loser_ids，后端不自行挑选
//   - 返回实际软删条数，便于前端校验
//   - 每个被删账号写入 account_events(type='deleted', source='dedupe')
func (h *Handler) DedupeAccounts(c *gin.Context) {
	var req struct {
		LoserIDs []int64 `json:"loser_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(req.LoserIDs) == 0 {
		writeError(c, http.StatusBadRequest, "未提供要删除的账号 ID")
		return
	}
	if len(req.LoserIDs) > 10000 {
		writeError(c, http.StatusBadRequest, "单次去重最多处理 10000 条")
		return
	}

	// 去重并过滤无效 ID
	seen := make(map[int64]struct{}, len(req.LoserIDs))
	ids := make([]int64, 0, len(req.LoserIDs))
	for _, id := range req.LoserIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "未提供有效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if err := h.db.BatchSoftDeleteAccounts(ctx, ids); err != nil {
		writeError(c, http.StatusInternalServerError, fmt.Sprintf("批量软删失败: %v", err))
		return
	}

	// 从内存池移除
	for _, id := range ids {
		h.store.RemoveAccount(id)
	}

	// 记账号事件（异步）
	h.db.BatchInsertAccountEventsAsync(ids, "deleted", "dedupe")
	h.InvalidateStatsCache()

	security.SecurityAuditLog("ACCOUNTS_DEDUPED", fmt.Sprintf("count=%d ip=%s", len(ids), c.ClientIP()))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已软删 %d 个重复账号（24h 内可从数据库恢复）", len(ids)),
		"deleted": len(ids),
	})
}
