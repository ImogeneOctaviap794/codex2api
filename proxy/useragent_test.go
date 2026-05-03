package proxy

import (
	"strings"
	"testing"
)

// TestClientProfilesAreConsistent 验证 113 画像池的内部一致性：
//   - 非空
//   - 每个画像 UA 字段中的版本号必须与 Version 字段一致
//   - 所有版本必须是 0.124.x 或 0.125.x（gpt-5.5 兼容范围）
//   - 画像池至少包含 1 个 latestCodexCLIVersion 画像（确保 fallback 可用）
func TestClientProfilesAreConsistent(t *testing.T) {
	if len(clientProfiles) == 0 {
		t.Fatal("clientProfiles should not be empty")
	}
	hasLatest := false
	for _, profile := range clientProfiles {
		if !strings.HasPrefix(profile.Version, "0.124.") && !strings.HasPrefix(profile.Version, "0.125.") {
			t.Fatalf("profile version %q out of supported range (0.124.x / 0.125.x)", profile.Version)
		}
		wantPrefix := "codex_cli_rs/" + profile.Version
		if !strings.HasPrefix(profile.UserAgent, wantPrefix) {
			t.Fatalf("UA %q must start with %q", profile.UserAgent, wantPrefix)
		}
		if profile.Version == latestCodexCLIVersion {
			hasLatest = true
		}
	}
	if !hasLatest {
		t.Fatalf("clientProfiles should contain at least one profile with latestCodexCLIVersion=%s", latestCodexCLIVersion)
	}
}

// TestProfileForAccountIsDeterministic 验证 ProfileForAccount 的核心性质：
//   - 同一账号 ID 始终返回同一画像（FNV hash 稳定）
//   - 返回的画像版本在支持范围内
func TestProfileForAccountIsDeterministic(t *testing.T) {
	profile1 := ProfileForAccount(12345)
	profile2 := ProfileForAccount(12345)
	if profile1.UserAgent != profile2.UserAgent {
		t.Fatalf("ProfileForAccount must be deterministic for same id; got %q vs %q", profile1.UserAgent, profile2.UserAgent)
	}
	if !strings.HasPrefix(profile1.Version, "0.124.") && !strings.HasPrefix(profile1.Version, "0.125.") {
		t.Fatalf("profile version %q out of supported range", profile1.Version)
	}
}

func TestIsCodexOfficialClientByHeaders(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		originator string
		want       bool
	}{
		{name: "cli ua", userAgent: "codex_cli_rs/0.125.0", want: true},
		{name: "vscode ua", userAgent: "codex_vscode/1.2.3", want: true},
		{name: "desktop originator", originator: "codex_chatgpt_desktop", want: true},
		{name: "non official", userAgent: "curl/8.0", originator: "opencode", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCodexOfficialClientByHeaders(tc.userAgent, tc.originator); got != tc.want {
				t.Fatalf("IsCodexOfficialClientByHeaders() = %v, want %v", got, tc.want)
			}
		})
	}
}
