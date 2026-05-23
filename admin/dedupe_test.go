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
			"AT only banned (history status string)",
			mkAccount(3, "c@x.com", "openai", "banned", "free", "", "at111", false, now),
			time.Time{},
			-10000, // unhealthy 硬扣分
		},
		{
			"unauthorized + RT (死号误保留修复场景)",
			mkAccount(7, "g@x.com", "openai", "unauthorized", "free", "rt-stale", "", false, now),
			time.Time{},
			-10000 + 100, // -10000 + has_rt(+100) = -9900
		},
		{
			"error status + pro + locked + RT (仍是负分)",
			mkAccount(8, "h@x.com", "openai", "error", "pro", "rt", "", true, now),
			time.Time{},
			-10000 + 100 + 50 + 30, // -9820
		},
		{
			"cooldown_reason=unauthorized + status=active + RT (生产真实场景)",
			func() *database.AccountRow {
				a := mkAccount(9, "i@x.com", "openai", "active", "free", "rt", "", false, now)
				a.CooldownReason = "unauthorized"
				return a
			}(),
			time.Time{},
			-10000 + 100 + 10, // unhealthy + has_rt + active = -9890
		},
		{
			"cooldown_reason=deactivated_workspace + pro + RT",
			func() *database.AccountRow {
				a := mkAccount(10, "j@x.com", "openai", "active", "pro", "rt", "", false, now)
				a.CooldownReason = "deactivated_workspace"
				return a
			}(),
			time.Time{},
			-10000 + 100 + 50 + 10, // -9840
		},
		{
			"cooldown_reason=rate_limited 不算不健康（可恢复）",
			func() *database.AccountRow {
				a := mkAccount(11, "k@x.com", "openai", "active", "free", "rt", "", false, now)
				a.CooldownReason = "rate_limited"
				return a
			}(),
			time.Time{},
			100 + 10, // 无扣分：has_rt + active = 110
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

// Bug regression: 邮箱去重时，上游 401 封禁但未清 RT 的死号，
// 不应该赢过同邮箱的 active AT-only 可用账号。
// 旧逻辑下：status=unauthorized+RT score=100，active+AT-only score=10，死号赢。
// 修复后：unhealthy -10000+100=-9900，active+AT-only=10，可用号赢。
//
// 注意：这个 case 走的是 Status 列兼容分支；生产真实场景看
// TestComputeDuplicateGroups_CooldownReasonUnauthorized_NeverBeatsActive。
func TestComputeDuplicateGroups_UnauthorizedNeverBeatsActive(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		// unauthorized + RT（被 OpenAI 封但数据库还存旧 RT）
		mkAccount(100, "dup@x.com", "openai", "unauthorized", "free", "rt-stale", "", false, now),
		// active + AT-only
		mkAccount(200, "dup@x.com", "openai", "active", "free", "", "at-good", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Winner.ID != 200 {
		t.Fatalf("active AT-only must win over unauthorized+RT, got winner=%d (score=%d)", g.Winner.ID, g.Winner.Score)
	}
	if len(g.Losers) != 1 || g.Losers[0].ID != 100 {
		t.Fatalf("loser should be unauthorized id=100, got %+v", g.Losers)
	}
}

// Bug regression: 即便 unhealthy 号集齐了 RT+pro+locked（原本总加分 180），
// 也不应该赢过完全无特征的 active AT-only 号。
func TestComputeDuplicateGroups_ErrorNeverBeatsActive(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		// error + RT + pro + locked：旧逻辑 score=100+50+30=180
		mkAccount(100, "e@x.com", "openai", "error", "pro", "rt", "", true, now),
		// active + AT-only：score=10
		mkAccount(200, "e@x.com", "openai", "active", "free", "", "at", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Winner.ID != 200 {
		t.Fatalf("active must beat error even when error has RT+pro+locked, got winner=%d (score=%d)", g.Winner.ID, g.Winner.Score)
	}
}

// Bug regression: 生产真实场景 —— status='active' + cooldown_reason='unauthorized'。
// 2026-05-23 生产调研发现持久化 status 列只取 active/deleted，
// 封禁信号实际写在 cooldown_reason 上（59 万账号中 647 条处于此状态）。
func TestComputeDuplicateGroups_CooldownReasonUnauthorized_NeverBeatsActive(t *testing.T) {
	now := time.Now()
	banned := mkAccount(100, "dup@x.com", "openai", "active", "free", "rt-stale", "", false, now)
	banned.CooldownReason = "unauthorized" // 生产表达封禁的真实方式

	good := mkAccount(200, "dup@x.com", "openai", "active", "free", "", "at-good", false, now)

	groups := computeDuplicateGroups([]*database.AccountRow{banned, good}, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Winner.ID != 200 {
		t.Fatalf("active AT-only must win over cooldown=unauthorized+RT, got winner=%d (score=%d)", g.Winner.ID, g.Winner.Score)
	}
	if len(g.Losers) != 1 || g.Losers[0].ID != 100 {
		t.Fatalf("loser should be banned id=100, got %+v", g.Losers)
	}
}

// Bug regression: workspace 被 OpenAI 停用的账号（402 deactivated_workspace）同样不可恢复。
func TestComputeDuplicateGroups_DeactivatedWorkspace_NeverBeatsActive(t *testing.T) {
	now := time.Now()
	dead := mkAccount(100, "dup@x.com", "openai", "active", "pro", "rt", "", true, now) // pro + locked + RT
	dead.CooldownReason = "deactivated_workspace"

	good := mkAccount(200, "dup@x.com", "openai", "active", "free", "", "at", false, now)

	groups := computeDuplicateGroups([]*database.AccountRow{dead, good}, nil)
	if g := groups[0]; g.Winner.ID != 200 {
		t.Fatalf("active must beat deactivated_workspace even with pro+locked+RT, got winner=%d", g.Winner.ID)
	}
}

// cooldown_reason=rate_limited 是可恢复状态，不当作 unhealthy。
// 避免误伤临时限流但仍有价值的主号。
func TestComputeDuplicateGroups_RateLimited_StillUsable(t *testing.T) {
	now := time.Now()
	rateLimited := mkAccount(100, "dup@x.com", "openai", "active", "pro", "rt", "", false, now)
	rateLimited.CooldownReason = "rate_limited"

	atOnly := mkAccount(200, "dup@x.com", "openai", "active", "free", "", "at", false, now)

	groups := computeDuplicateGroups([]*database.AccountRow{rateLimited, atOnly}, nil)
	// rate_limited+pro+RT (160) 仍赢 active+AT-only (10)
	if g := groups[0]; g.Winner.ID != 100 {
		t.Fatalf("rate_limited pro+RT must still beat active AT-only (it is recoverable), got winner=%d", g.Winner.ID)
	}
}

// 两个都是 unhealthy 时，应按原有规则比（均 -10000 扣完之后看剩余分项）。
func TestComputeDuplicateGroups_BothUnhealthy_StillRanks(t *testing.T) {
	now := time.Now()
	rows := []*database.AccountRow{
		// unauthorized + RT：score=-10000+100=-9900
		mkAccount(1, "u@x.com", "openai", "unauthorized", "free", "rt", "", false, now),
		// error、无 RT：score=-10000
		mkAccount(2, "u@x.com", "openai", "error", "free", "", "at", false, now),
	}
	groups := computeDuplicateGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Winner.ID != 1 {
		t.Fatalf("when both unhealthy, higher residual score must win, got %d", g.Winner.ID)
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
