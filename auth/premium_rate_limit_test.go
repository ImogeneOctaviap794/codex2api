package auth

import (
	"context"
	"testing"
	"time"
)

func newPremium5hTestAccount(plan string, resetAt time.Time) *Account {
	return &Account{
		DBID:                1,
		AccessToken:         "token",
		PlanType:            plan,
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           resetAt,
		UsageUpdatedAt:      time.Now().Add(-20 * time.Minute),
	}
}

// Premium 5h 限流（5h 用量 100% 且 Reset5hAt 未到）期间完全不可调度，
// RuntimeStatus 归为 usage_exhausted（与 free 110%+ 同语义：上游真停服）。
func TestPremium5hRateLimitedAccountIsUnavailable(t *testing.T) {
	acc := newPremium5hTestAccount("plus", time.Now().Add(45*time.Minute))

	if got := acc.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted", got)
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false during premium 5h rate limit window")
	}
}

func TestPremium5hRateLimitExpiresAndUsageProbeResumes(t *testing.T) {
	acc := newPremium5hTestAccount("team", time.Now().Add(-time.Minute))

	snapshot := acc.GetSchedulerDebugSnapshot(4)
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active after reset expires", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true after reset expires")
	}
	if snapshot.HealthTier != string(HealthTierHealthy) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierHealthy)
	}
	if snapshot.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want 4", snapshot.DynamicConcurrencyLimit)
	}
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true after reset expires and snapshot becomes stale")
	}
}

func TestPremium5hRateLimitedSkipsUsageProbeBeforeReset(t *testing.T) {
	acc := newPremium5hTestAccount("pro", time.Now().Add(30*time.Minute))

	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false before premium 5h reset time")
	}
}

func TestCleanByRuntimeStatusSkipsPremium5hRateLimitedAccount(t *testing.T) {
	acc := newPremium5hTestAccount("plus", time.Now().Add(20*time.Minute))
	store := &Store{
		accounts: []*Account{acc},
	}

	if cleaned := store.CleanByRuntimeStatus(context.Background(), "rate_limited"); cleaned != 0 {
		t.Fatalf("CleanByRuntimeStatus() cleaned = %d, want 0", cleaned)
	}
	if store.AccountCount() != 1 {
		t.Fatalf("AccountCount() = %d, want 1", store.AccountCount())
	}
}
