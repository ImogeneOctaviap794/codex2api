package proxy

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// ==================== Codex 官方客户端识别（upstream 6204889） ====================
//
// 当上游请求头声明自己是 codex_cli_rs / codex_vscode 等官方客户端时，
// 直接透传其 UA 和指纹，避免"假装 Codex" 反而暴露。
// 此机制配合 CODEX_TRANSPORT_MODE=standard 使用。

// latestCodexCLIVersion 是本进程在 UA 头缺失时的 fallback 版本号。
// 画像池里包含 0.124.0 / 0.125.0 / 0.125.1，这里固定用 0.125.0 作为保守默认。
const (
	latestCodexCLIVersion         = "0.125.0"
	latestCodexCLIUserAgentPrefix = "codex_cli_rs/" + latestCodexCLIVersion
)

var codexOfficialClientUserAgentPrefixes = []string{
	"codex_cli_rs/",
	"codex_vscode/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
	"codex ",
}

var codexOfficialClientOriginatorPrefixes = []string{
	"codex_",
	"codex ",
}

// IsCodexOfficialClientByHeaders 根据 User-Agent / Originator 判断请求方是否自称 Codex 官方客户端
func IsCodexOfficialClientByHeaders(userAgent, originator string) bool {
	return matchCodexClientHeaderPrefixes(userAgent, codexOfficialClientUserAgentPrefixes) ||
		matchCodexClientHeaderPrefixes(originator, codexOfficialClientOriginatorPrefixes)
}

// LatestCodexCLIVersionForHeaders 返回本进程默认使用的 Codex CLI 版本（用于 header fallback）
func LatestCodexCLIVersionForHeaders() string {
	return latestCodexCLIVersion
}

// MinimalCodexCLIUserAgentForHeaders 返回 header fallback 时使用的最小 UA 前缀
func MinimalCodexCLIUserAgentForHeaders() string {
	return "codex_cli_rs/" + latestCodexCLIVersion
}

func matchCodexClientHeaderPrefixes(value string, prefixes []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(value, prefix) || strings.Contains(value, prefix) {
			return true
		}
	}
	return false
}

// ==================== 动态 User-Agent 生成 ====================
//
// 真实 codex_cli_rs 的 UA 格式（源码: codex-rs/login/src/auth/default_client.rs）：
//   {originator}/{version} ({OS} {OS_version}; {arch}) {terminal}
//
// 示例：
//   codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464
//   codex_cli_rs/0.124.0 (Mac OS 15.1.0; arm64) Ghostty/1.2.3
//   codex_cli_rs/0.124.0 (Windows 10.0.26120; x86_64) WindowsTerminal

// ClientProfile 表示一个模拟客户端的完整身份
type ClientProfile struct {
	UserAgent      string // 完整的 User-Agent 字符串
	Version        string // codex CLI 版本（需与 UA 中的版本一致）
	OS             string // X-Stainless-Os（MacOS / Linux / Windows）
	Arch           string // X-Stainless-Arch（arm64 / x64）
	RuntimeVersion string // X-Stainless-Runtime-Version（rustc/x.y.z）
}

// 预定义的真实客户端画像池
// 按开发者常见环境分布：macOS（主力 ~70%）> Linux（~20%）> Windows（~10%）
// 所有 CLI 版本必须 >= 0.124.0（否则 gpt-5.5 会 model_not_found）
// RuntimeVersion 使用真实的 Rust stable 版本（1.83.0~1.86.0）
//
// 设计目标：让 (UA, OS, Arch, RuntimeVersion, Version) 五维组合达到 120+ 种唯一画像，
// 配合 ProfileForAccount 的 FNV hash → 每个 OpenAI 账号锁定一个画像。
var clientProfiles = []ClientProfile{
	// ============ macOS arm64（Apple Silicon，主力开发者） ============
	// --- macOS 15.5.x / Apple_Terminal ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.1; arm64) Apple_Terminal/464", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.1 (Mac OS 15.5.0; arm64) Apple_Terminal/464", "0.124.1", "MacOS", "arm64", "rustc/1.86.0"},
	{"codex_cli_rs/0.125.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464", "0.125.0", "MacOS", "arm64", "rustc/1.86.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) Apple_Terminal/453", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	// --- macOS 15.4.x ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; arm64) Apple_Terminal/453", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.1; arm64) Apple_Terminal/453", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.1; arm64) Ghostty/1.2.3", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; arm64) Ghostty/1.2.0", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; arm64) vscode/1.98.0", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; arm64) vscode/1.99.3", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.1 (Mac OS 15.4.1; arm64) iTerm.app/3.5.10", "0.124.1", "MacOS", "arm64", "rustc/1.85.0"},
	// --- macOS 15.3.x ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.0; arm64) iTerm.app/3.5.10", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.1; arm64) iTerm.app/3.5.10", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.2; arm64) iTerm.app/3.5.11", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.0; arm64) Ghostty/1.1.0", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.1; arm64) Ghostty/1.1.2", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	// --- macOS 15.2.x ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.2.0; arm64) WezTerm/20250101", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.2.0; arm64) WezTerm/20241114", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.2.0; arm64) Alacritty/0.15.1", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.2.0; arm64) tmux/3.5a", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	// --- macOS 15.1.x / 15.0 ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.1.0; arm64) iTerm.app/3.5.8", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.1.1; arm64) Apple_Terminal/453", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.1.0; arm64) kitty/0.39.0", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.0.1; arm64) Apple_Terminal/440", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	// --- kitty 终端用户（重度命令行） ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) kitty/0.40.0", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.1; arm64) kitty/0.39.1", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.0; arm64) kitty/0.38.1", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.125.1 (Mac OS 15.5.0; arm64) kitty/0.40.1", "0.125.1", "MacOS", "arm64", "rustc/1.86.0"},
	// --- Alacritty ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) Alacritty/0.15.1", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; arm64) Alacritty/0.14.0", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.4; arm64) Alacritty/0.15.1", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.6.1; arm64) Alacritty/0.14.0", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	// --- vscode 集成终端用户 ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) vscode/1.100.0", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) vscode/1.99.3", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.1; arm64) vscode/1.99.0", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.1 (Mac OS 15.5.0; arm64) vscode/1.100.2", "0.124.1", "MacOS", "arm64", "rustc/1.85.0"},
	// --- tmux 多窗口用户 ---
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; arm64) tmux/3.5a", "0.124.0", "MacOS", "arm64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.0; arm64) tmux/3.4", "0.124.0", "MacOS", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) tmux/3.5a", "0.124.0", "MacOS", "arm64", "rustc/1.85.0"},
	// --- macOS 14.x（仍在用 Sonoma） ---
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.4; arm64) Apple_Terminal/440", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.3; arm64) iTerm.app/3.5.6", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.0; arm64) iTerm.app/3.5.4", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.6.1; arm64) Apple_Terminal/440", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.6.0; arm64) Ghostty/1.0.0", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.5.0; arm64) Apple_Terminal/440", "0.124.0", "MacOS", "arm64", "rustc/1.83.0"},

	// ============ macOS x86_64（Intel Mac，少量） ============
	{"codex_cli_rs/0.124.0 (Mac OS 15.4.0; x86_64) Apple_Terminal/464", "0.124.0", "MacOS", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.3.0; x86_64) iTerm.app/3.5.10", "0.124.0", "MacOS", "x64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.0; x86_64) iTerm.app/3.5.8", "0.124.0", "MacOS", "x64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.4; x86_64) Apple_Terminal/440", "0.124.0", "MacOS", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.6.1; x86_64) Alacritty/0.14.0", "0.124.0", "MacOS", "x64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 13.7.4; x86_64) iTerm.app/3.5.6", "0.124.0", "MacOS", "x64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 14.7.0; x86_64) vscode/1.99.0", "0.124.0", "MacOS", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Mac OS 15.2.0; x86_64) tmux/3.5a", "0.124.0", "MacOS", "x64", "rustc/1.84.0"},

	// ============ Linux（服务器与开发者工作站） ============
	// --- Ubuntu LTS / 短期版 ---
	{"codex_cli_rs/0.124.0 (Ubuntu 24.04; x86_64) kitty/0.35.2", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Ubuntu 24.04; x86_64) Alacritty/0.14.0", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Ubuntu 24.04; x86_64) gnome-terminal/3.52.0", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Ubuntu 24.04; x86_64) vscode/1.99.3", "0.124.0", "Linux", "x64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Ubuntu 24.04; x86_64) tmux/3.4", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Ubuntu 24.10; x86_64) Alacritty/0.14.0", "0.124.0", "Linux", "x64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Ubuntu 24.10; x86_64) kitty/0.36.4", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.1 (Ubuntu 24.10; x86_64) gnome-terminal/3.54.0", "0.124.1", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Ubuntu 22.04; x86_64) gnome-terminal/3.44.0", "0.124.0", "Linux", "x64", "rustc/1.83.0"},
	{"codex_cli_rs/0.124.0 (Ubuntu 22.04; x86_64) tmux/3.2a", "0.124.0", "Linux", "x64", "rustc/1.83.0"},
	// --- Debian ---
	{"codex_cli_rs/0.124.0 (Debian 12; x86_64) tmux/3.3a", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Debian 12; x86_64) gnome-terminal/3.46.0", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Debian 13; x86_64) Alacritty/0.14.0", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	// --- Arch / Manjaro / EndeavourOS ---
	{"codex_cli_rs/0.124.0 (Arch Linux Rolling; x86_64) kitty/0.40.0", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Arch Linux Rolling; x86_64) Alacritty/0.15.1", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Arch Linux Rolling; x86_64) WezTerm/20250101", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Manjaro Linux 24.2.0; x86_64) konsole/24.12.3", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.1 (Arch Linux Rolling; x86_64) Ghostty/1.2.3", "0.124.1", "Linux", "x64", "rustc/1.86.0"},
	// --- Fedora ---
	{"codex_cli_rs/0.124.0 (Fedora Linux 41; x86_64) vscode/1.100.0", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Fedora Linux 41; x86_64) gnome-terminal/3.52.0", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Fedora Linux 42; x86_64) konsole/25.04.0", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Fedora Linux 41; x86_64) Alacritty/0.14.0", "0.124.0", "Linux", "x64", "rustc/1.84.0"},
	// --- NixOS / openSUSE ---
	{"codex_cli_rs/0.124.0 (NixOS 24.11; x86_64) kitty/0.37.0", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (openSUSE Tumbleweed; x86_64) konsole/24.12.0", "0.124.0", "Linux", "x64", "rustc/1.85.0"},
	// --- Linux ARM（树莓派/服务器 ARM） ---
	{"codex_cli_rs/0.124.0 (Ubuntu 24.04; aarch64) tmux/3.4", "0.124.0", "Linux", "arm64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Debian 12; aarch64) gnome-terminal/3.46.0", "0.124.0", "Linux", "arm64", "rustc/1.84.0"},

	// ============ Windows ============
	// --- Windows 11 23H2 / 24H2 / WindowsTerminal ---
	{"codex_cli_rs/0.124.0 (Windows 10.0.26120; x86_64) WindowsTerminal", "0.124.0", "Windows", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.26100; x86_64) WindowsTerminal", "0.124.0", "Windows", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.22631; x86_64) WindowsTerminal", "0.124.0", "Windows", "x64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.22631; x86_64) WindowsTerminal", "0.124.0", "Windows", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.1 (Windows 10.0.26120; x86_64) WindowsTerminal", "0.124.1", "Windows", "x64", "rustc/1.86.0"},
	{"codex_cli_rs/0.125.0 (Windows 10.0.26120; x86_64) WindowsTerminal", "0.125.0", "Windows", "x64", "rustc/1.86.0"},
	// --- vscode on Windows ---
	{"codex_cli_rs/0.124.0 (Windows 10.0.26100; x86_64) vscode/1.99.3", "0.124.0", "Windows", "x64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.22631; x86_64) vscode/1.99.0", "0.124.0", "Windows", "x64", "rustc/1.84.1"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.26120; x86_64) vscode/1.100.0", "0.124.0", "Windows", "x64", "rustc/1.85.0"},
	// --- Alacritty on Windows ---
	{"codex_cli_rs/0.124.0 (Windows 10.0.26100; x86_64) Alacritty/0.14.0", "0.124.0", "Windows", "x64", "rustc/1.84.0"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.22631; x86_64) Alacritty/0.14.0", "0.124.0", "Windows", "x64", "rustc/1.84.0"},
	// --- Windows ARM64（Surface Pro X / Snapdragon X 笔记本） ---
	{"codex_cli_rs/0.124.0 (Windows 10.0.26120; aarch64) WindowsTerminal", "0.124.0", "Windows", "arm64", "rustc/1.85.0"},
	{"codex_cli_rs/0.124.0 (Windows 10.0.26100; aarch64) vscode/1.99.0", "0.124.0", "Windows", "arm64", "rustc/1.84.1"},
}

// ProfileForAccount 根据账号 ID 确定性地选择一个 ClientProfile
// 同一个账号永远返回相同的 profile，不同账号大概率返回不同的 profile
func ProfileForAccount(accountID int64) ClientProfile {
	if len(clientProfiles) == 0 {
		return ClientProfile{
			UserAgent:      "codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
			Version:        "0.124.0",
			OS:             "MacOS",
			Arch:           "arm64",
			RuntimeVersion: "rustc/1.85.0",
		}
	}

	// 用 FNV hash 将 accountID 映射到 profile 池，确保分布均匀
	h := fnv.New32a()
	fmt.Fprintf(h, "codex2api:ua-profile:%d", accountID)
	idx := int(h.Sum32()) % len(clientProfiles)
	if idx < 0 {
		idx = -idx
	}

	return clientProfiles[idx]
}
