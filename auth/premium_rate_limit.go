package auth

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

const premium5hFallbackWindow = 5 * time.Hour

func normalizePlanType(plan string) string {
	return strings.ToLower(strings.TrimSpace(plan))
}

func isPremium5hPlan(plan string) bool {
	switch normalizePlanType(plan) {
	case "plus", "pro", "team":
		return true
	default:
		return false
	}
}

// IsPremium5hPlan 判断当前账号是否属于 premium 5h 限流语义范围。
func (a *Account) IsPremium5hPlan() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return isPremium5hPlan(a.PlanType)
}

func (a *Account) premium5hRateLimitedLocked(now time.Time) bool {
	if !isPremium5hPlan(a.PlanType) {
		return false
	}
	if !a.UsagePercent5hValid || a.UsagePercent5h < 100 {
		return false
	}
	if a.Reset5hAt.IsZero() {
		return false
	}
	return a.Reset5hAt.After(now)
}

func (a *Account) premium5hRateLimitWindowLocked(now time.Time) (bool, time.Time) {
	if !a.premium5hRateLimitedLocked(now) {
		return false, time.Time{}
	}
	return true, a.Reset5hAt
}

func (a *Account) premium5hCooldownSuppressedLocked(now time.Time) bool {
	if a.Status != StatusCooldown || a.CooldownReason != "rate_limited" {
		return false
	}
	active, _ := a.premium5hRateLimitWindowLocked(now)
	return active
}

// IsPremium5hRateLimited 判断账号当前是否处于 premium 5h 限流态。
func (a *Account) IsPremium5hRateLimited() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.premium5hRateLimitedLocked(time.Now())
}

// GetUsageSnapshot5h 返回当前 5h 用量快照。
func (a *Account) GetUsageSnapshot5h() (pct float64, resetAt time.Time, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.UsagePercent5hValid {
		return 0, time.Time{}, false
	}
	return a.UsagePercent5h, a.Reset5hAt, true
}

// PersistUsageSnapshot5hOnly 持久化仅包含 5h 数据的用量快照。
func (s *Store) PersistUsageSnapshot5hOnly(acc *Account) {
	if acc == nil || s == nil || s.db == nil {
		return
	}

	pct5h, reset5hAt, ok := acc.GetUsageSnapshot5h()
	if !ok {
		return
	}

	updatedAt := time.Now()
	acc.mu.Lock()
	acc.UsageUpdatedAt = updatedAt
	acc.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateUsageSnapshot5h(ctx, acc.DBID, pct5h, reset5hAt, updatedAt); err != nil {
		log.Printf("[账号 %d] 持久化 5h 用量快照失败: %v", acc.DBID, err)
	}
}

// InferPremiumPlanFromHeaders 在检测到 5h 响应头但 plan_type 为空时回退推断为 "plus"。
// 仅 Plus/Pro/Team 套餐会返回 5h 窗口头，但头内不区分具体等级，故以 plus 作为最小权限的安全默认。
// 返回 true 表示确实发生了回退赋值并已持久化。
func (s *Store) InferPremiumPlanFromHeaders(acc *Account) bool {
	if acc == nil || s == nil {
		return false
	}
	acc.mu.Lock()
	if acc.PlanType != "" {
		acc.mu.Unlock()
		return false
	}
	acc.PlanType = "plus"
	dbID := acc.DBID
	acc.mu.Unlock()

	log.Printf("[账号 %d] 依据 5h 响应头回退推断 plan_type=plus", dbID)

	if s.db == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, dbID, map[string]interface{}{
		"plan_type": "plus",
	}); err != nil {
		log.Printf("[账号 %d] 持久化推断 plan_type 失败: %v", dbID, err)
	}
	return true
}

// SyncAccountPlanType 同步上游报告的 plan_type 到本地账号（双写：内存 + DB）。
// 当 OpenAI 在 429 usage_limit_reached 错误体里报告 plan_type 时调用，
// 用于实时检测被降级的账号（如 plus → free）。
//
// 与 InferPremiumPlanFromHeaders 的差异：
//   - InferPremiumPlanFromHeaders: 仅在本地 plan_type 为空时回退推断为 "plus"
//   - SyncAccountPlanType: 强制覆盖（包括 plus → free 这种降级场景）
//
// 返回 true 表示发生了实际更新（plan_type 改变了）。
func (s *Store) SyncAccountPlanType(acc *Account, planType string) bool {
	if acc == nil || s == nil {
		return false
	}
	planType = strings.ToLower(strings.TrimSpace(planType))
	if planType == "" {
		return false
	}
	acc.mu.Lock()
	oldPlan := acc.PlanType
	if oldPlan == planType {
		acc.mu.Unlock()
		return false
	}
	acc.PlanType = planType
	dbID := acc.DBID
	// 重算调度分（plan 改变会影响 score_bias）
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)

	log.Printf("[账号 %d] 同步上游 plan_type: %q → %q", dbID, oldPlan, planType)

	if s.db == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, dbID, map[string]interface{}{
		"plan_type": planType,
	}); err != nil {
		log.Printf("[账号 %d] 持久化 plan_type 失败: %v", dbID, err)
	}
	return true
}

// MarkPremium5hRateLimited 将账号标记为 premium 5h 限流态，并按 resetAt 驱动恢复。
func (s *Store) MarkPremium5hRateLimited(acc *Account, resetAt time.Time) {
	if acc == nil || s == nil {
		return
	}

	now := time.Now()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(premium5hFallbackWindow)
	}

	acc.mu.Lock()
	acc.UsagePercent5h = 100
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = resetAt
	acc.UsageUpdatedAt = now
	acc.LastRateLimitedAt = now
	if acc.Status == StatusCooldown && acc.CooldownReason == "rate_limited" {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierRisky
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearCooldown(ctx, acc.DBID); err != nil {
		log.Printf("[账号 %d] 清理 premium 5h 限流冷却状态失败: %v", acc.DBID, err)
	}
	if err := s.db.UpdateUsageSnapshot5h(ctx, acc.DBID, 100, resetAt, now); err != nil {
		log.Printf("[账号 %d] 持久化 premium 5h 限流快照失败: %v", acc.DBID, err)
	}
}
