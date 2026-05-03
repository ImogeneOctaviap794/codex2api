package admin

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func mkAccount(id int64, email, platform, status, planType, refreshToken, accessToken string, locked bool, createdAt time.Time) *database.AccountRow {
	creds := map[string]interface{}{
		"email":     email,
		"plan_type": planType,
	}
	if refreshToken != "" {
		creds["refresh_token"] = refreshToken
	}
	if accessToken != "" {
		creds["access_token"] = accessToken
	}
	return &database.AccountRow{
		ID:          id,
		Name:        email,
		Platform:    platform,
		Credentials: creds,
		Status:      status,
		Locked:      locked,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

func TestScoreAccount_RuleBreakdown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		row        *database.AccountRow
		lastUsedAt time.Time
		wantScore  int
	}{
		{
			"RT only",
			mkAccount(1, "a@x.com", "openai", "rate_limited", "free", "rt123", "", false, now),
			time.Time{},
			100,
		},
		{
			"pro locked active recent RT",
			mkAccount(2, "b@x.com", "openai", "active", "pro", "rt456", "at789", true, now),
			now.Add(-1 * time.Hour),
			100 + 50 + 30 + 10 + 5, // 195
		},
		{
			"AT only banned",
			mkAccount(3, "c@x.com", "openai", "banned", "free", "", "at111", false, now),
			time.Time{},
			0,
		},
		{
			"free but active locked",
			mkAccount(4, "d@x.com", "openai", "active", "free", "rt222", "", true, now),
			time.Time{},
			100 + 30 + 10, // 140
		},
		{
			"teamplus not locked",
			mkAccount(5, "e@x.com", "openai", "active", "teamplus", "rt333", "", false, now),
			time.Time{},
			100 + 50 + 10, // 160
		},
		{
			"old last_used > 7d doesn't score",
			mkAccount(6, "f@x.com", "openai", "active", "free", "rt444", "", false, now),
			now.Add(-8 * 24 * time.Hour),
			100 + 10, // 110
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scoreAccount(c.row, c.lastUsedAt)
			if got != c.wantScore {
				t.Fatalf("%s: score=%d want=%d", c.name, got, c.wantScore)
			}
		})
	}
}

func TestComputeDuplicateGroups_SkipsSingletons(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		mkAccount(1, "a@x.com", "openai", "active", "free", "rt1", "", false, now),
		mkAccount(2, "b@x.com", "openai", "active", "free", "rt2", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups (all singletons), got %d", len(groups))
	}
}

func TestComputeDuplicateGroups_EmptyEmailIgnored(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		mkAccount(1, "", "openai", "active", "free", "rt1", "", false, now),
		mkAccount(2, "", "openai", "active", "free", "rt2", "", false, now),
		mkAccount(3, "  ", "openai", "active", "free", "rt3", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 0 {
		t.Fatalf("empty-email accounts must never be grouped, got %d groups", len(groups))
	}
}

func TestComputeDuplicateGroups_CrossPlatformNotDedupe(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		mkAccount(1, "shared@x.com", "openai", "active", "free", "rt1", "", false, now),
		mkAccount(2, "shared@x.com", "anthropic", "active", "free", "rt2", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 0 {
		t.Fatalf("same email across different platforms must NOT be duplicates, got %d groups", len(groups))
	}
}

func TestComputeDuplicateGroups_WinnerSelection_RTBeatsATOnly(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		// AT-only，id 较小
		mkAccount(100, "dup@x.com", "openai", "active", "free", "", "at1", false, now),
		// 有 RT，id 较大
		mkAccount(200, "dup@x.com", "openai", "active", "free", "rt2", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Winner.ID != 200 {
		t.Fatalf("winner should be RT-holding id=200, got %d", g.Winner.ID)
	}
	if len(g.Losers) != 1 || g.Losers[0].ID != 100 {
		t.Fatalf("loser should be AT-only id=100, got %+v", g.Losers)
	}
}

func TestComputeDuplicateGroups_WinnerSelection_ProBeatsFree(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		// free + RT
		mkAccount(1, "x@x.com", "openai", "active", "free", "rt1", "", false, now),
		// pro + RT
		mkAccount(2, "x@x.com", "openai", "active", "pro", "rt2", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if groups[0].Winner.ID != 2 {
		t.Fatalf("pro tier must win over free, got winner id=%d", groups[0].Winner.ID)
	}
}

func TestComputeDuplicateGroups_WinnerSelection_IDAscTieBreaker(t *testing.T) {
	now := time.Now()
	// 三个完全等分账号：全部 has_rt + active + free
	rows := []*database.AccountRow{
		mkAccount(30, "same@x.com", "openai", "active", "free", "rt", "", false, now),
		mkAccount(10, "same@x.com", "openai", "active", "free", "rt", "", false, now),
		mkAccount(20, "same@x.com", "openai", "active", "free", "rt", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Winner.ID != 10 {
		t.Fatalf("tie breaker must pick smallest id, got winner=%d", g.Winner.ID)
	}
	// losers 应按 id 升序排列：20, 30
	if len(g.Losers) != 2 || g.Losers[0].ID != 20 || g.Losers[1].ID != 30 {
		t.Fatalf("losers order wrong: %+v", g.Losers)
	}
}

func TestComputeDuplicateGroups_EmailNormalization(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		mkAccount(1, "USER@Example.COM", "openai", "active", "free", "rt1", "", false, now),
		mkAccount(2, "  user@example.com  ", "openai", "active", "free", "rt2", "", false, now),
		mkAccount(3, "user@example.com", "openai", "active", "free", "rt3", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("email case/space should be normalized into one group, got %d", len(groups))
	}
	if groups[0].TotalInGroup != 3 {
		t.Fatalf("expected 3 in group, got %d", groups[0].TotalInGroup)
	}
}

func TestComputeDuplicateGroups_PlatformDefault(t *testing.T) {
	now := time.Now()
	// platform 为空的历史数据应 fallback 为 openai
	rows := []*database.AccountRow{
		mkAccount(1, "same@x.com", "", "active", "free", "rt1", "", false, now),
		mkAccount(2, "same@x.com", "openai", "active", "free", "rt2", "", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("platform='' should normalize to 'openai' and group with explicit 'openai', got %d groups", len(groups))
	}
}
