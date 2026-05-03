package auth

import (
	"testing"
	"time"
)

func newFreeTestAccount(pct float64) *Account {
	return &Account{
		DBID:                1,
		AccessToken:         "token",
		PlanType:            "free",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent7d:      pct,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(24 * time.Hour),
	}
}

// < 90% 不触发限速。
func TestFreeUsageBelowThresholdStaysActive(t *testing.T) {
	acc := newFreeTestAccount(89.9)

	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true")
	}
}

// >= 90% 且 < 100% 进入软限速：RuntimeStatus=rate_limited、HealthTier=Risky、仍可用。
func TestFreeUsageAtThresholdEntersRateLimited(t *testing.T) {
	acc := newFreeTestAccount(90)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true (仅降权，不阻断)")
	}

	snap := acc.GetSchedulerDebugSnapshot(4)
	if snap.HealthTier != string(HealthTierRisky) {
		t.Fatalf("HealthTier = %q, want %q", snap.HealthTier, HealthTierRisky)
	}
	if snap.Breakdown.UsagePenalty7d != 25 {
		t.Fatalf("UsagePenalty7d = %v, want 25", snap.Breakdown.UsagePenalty7d)
	}
}

// 95% 仍为限速，但调度惩罚加深到 30。
func TestFreeUsageAt95StillRateLimitedWithHigherPenalty(t *testing.T) {
	acc := newFreeTestAccount(95)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}

	snap := acc.GetSchedulerDebugSnapshot(4)
	if snap.Breakdown.UsagePenalty7d != 30 {
		t.Fatalf("UsagePenalty7d = %v, want 30", snap.Breakdown.UsagePenalty7d)
	}
}

// 100% 仍处于软限速区间，只有 ≥ FreeUsageHardLimitPct (110%) 才进入 usage_exhausted。
// 原因：OpenAI 100% 是软上限，实际到 110% 左右才会真返回 429。
func TestFreeUsageAt100StaysRateLimitedNotExhausted(t *testing.T) {
	acc := newFreeTestAccount(100)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true at 100%% (仅重罚分，不阻断)")
	}
	snap := acc.GetSchedulerDebugSnapshot(4)
	if snap.Breakdown.UsagePenalty7d != 40 {
		t.Fatalf("UsagePenalty7d = %v, want 40", snap.Breakdown.UsagePenalty7d)
	}
}

// 100-110% 区间 (105%) 仍可调度，只加重罚分。
func TestFreeUsageBetween100And110StillSchedulable(t *testing.T) {
	acc := newFreeTestAccount(105)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true at 105%%")
	}
}

// ≥ 110% 才进入 usage_exhausted 硬阻断。
func TestFreeUsageAt110BecomesExhausted(t *testing.T) {
	acc := newFreeTestAccount(110)

	if got := acc.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted", got)
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false at 110%%")
	}
	snap := acc.GetSchedulerDebugSnapshot(4)
	if snap.Breakdown.UsagePenalty7d != 50 {
		t.Fatalf("UsagePenalty7d = %v, want 50", snap.Breakdown.UsagePenalty7d)
	}
}

// Free 账号在软限速区间 (99%) 真收到 429 cooldown 时，
// RuntimeStatus 必须升级为 usage_exhausted，避免 UI 误显示"软限速（仍可用）"。
func TestFreeUsageInSoftLimitWithCooldownReportsExhausted(t *testing.T) {
	acc := newFreeTestAccount(99)
	// 模拟 Apply429Cooldown 的效果
	acc.Status = StatusCooldown
	acc.CooldownUtil = time.Now().Add(20 * time.Hour)
	acc.CooldownReason = "rate_limited"

	if got := acc.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted (real 429 cooldown overrides soft throttle)", got)
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false (cooldown not expired)")
	}
}

// 无 cooldown 的 99% 仍属于纯软限速。
func TestFreeUsageInSoftLimitWithoutCooldownStaysRateLimited(t *testing.T) {
	acc := newFreeTestAccount(99)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true (no cooldown, still schedulable)")
	}
}

// 软限速规则仅针对 free plan；plus/pro/team 不受影响。
func TestFreeUsageRateLimitDoesNotAffectPremiumPlans(t *testing.T) {
	for _, plan := range []string{"plus", "pro", "team"} {
		acc := newFreeTestAccount(92)
		acc.PlanType = plan

		if got := acc.RuntimeStatus(); got != "active" {
			t.Fatalf("[plan=%s] RuntimeStatus() = %q, want active", plan, got)
		}
	}
}

// 未采集到 7d 用量快照时，不触发限速。
func TestFreeUsageWithoutSnapshotNotRateLimited(t *testing.T) {
	acc := newFreeTestAccount(99)
	acc.UsagePercent7dValid = false

	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active", got)
	}
}
