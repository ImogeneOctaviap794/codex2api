package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex 身份混淆（identity confuse）
//
// 背景：OpenAI 风控会"污染下一个账号"——会话身份标识符（prompt_cache_key /
// session_id / installation-id / window_id / turn_id 等）在 fill-first / 会话
// 粘性下会跨账号流转，一个被标记的脏会话标识带到新账号上会连累新账号一起被封。
//
// 解法：账号选定后、发往上游前，把所有会话身份标识用 SHA1(accountSalt + 原值)
// 确定性重映射成一个【绑定当前账号】的新 UUID。这样：
//   - 同一账号 + 同一原值始终得到相同结果 → 账号内会话连续性 / prompt cache 不变；
//   - 不同账号永不共享同一标识 → 脏会话无法跨账号牵连。
//
// 完全体（对齐 CLIProxyAPI）：
//   - 请求侧混淆 prompt_cache_key / Session_id / installation-id / window_id，
//     以及 turn-metadata 内嵌的 prompt_cache_key / window_id / turn_id；
//   - turn_id / prompt_cache_key 的 {原值→混淆值} 记入 CodexIdentityConfuseState；
//   - 响应侧用该 state 把上游回显的混淆值还原成客户端原值（confused→original），
//     让客户端永远只见到自己的标识，多轮对话闭环一致、不漂移。
//
// 开关：环境变量 CODEX_IDENTITY_CONFUSE（true/1/yes/on 启用）。

// codexIdentityReplacement 记录一对 {原值, 混淆值}，用于响应回写还原。
type codexIdentityReplacement struct {
	original string
	confused string
}

// CodexIdentityConfuseState 贯穿单次请求-响应，记录所有需要在响应里还原的映射。
type CodexIdentityConfuseState struct {
	pairs []codexIdentityReplacement // confused → original
}

// NewCodexIdentityConfuseState 创建一个空 state（仅在全局启用时由调用方使用）。
func NewCodexIdentityConfuseState() *CodexIdentityConfuseState {
	return &CodexIdentityConfuseState{}
}

// addPair 登记一对回写映射（按混淆值去重；空值/相等忽略）。
func (s *CodexIdentityConfuseState) addPair(original, confused string) {
	if s == nil {
		return
	}
	original = strings.TrimSpace(original)
	confused = strings.TrimSpace(confused)
	if original == "" || confused == "" || original == confused {
		return
	}
	for _, p := range s.pairs {
		if p.confused == confused {
			return
		}
	}
	s.pairs = append(s.pairs, codexIdentityReplacement{original: original, confused: confused})
}

// Empty 报告 state 是否没有任何需要回写的映射。
func (s *CodexIdentityConfuseState) Empty() bool {
	return s == nil || len(s.pairs) == 0
}

// CodexIdentityConfuseEnabled 报告是否启用 Codex 身份混淆。
func CodexIdentityConfuseEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_IDENTITY_CONFUSE"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// codexAccountIdentitySalt 返回账号的稳定标识，用作混淆 salt。
// 优先使用 ChatGPT account id（绑定 OAI 账号本身，最稳定），回退到 DBID。
func codexAccountIdentitySalt(account *auth.Account) string {
	if account == nil {
		return ""
	}
	account.Mu().RLock()
	salt := strings.TrimSpace(account.AccountID)
	if salt == "" && account.DBID > 0 {
		salt = fmt.Sprintf("db:%d", account.DBID)
	}
	account.Mu().RUnlock()
	return salt
}

// codexIdentityConfuseUUID 基于 (salt, kind, value) 生成确定性 UUID。
func codexIdentityConfuseUUID(salt, kind, value string) string {
	name := strings.Join([]string{"codex2api", "codex", "identity-confuse", kind, strings.TrimSpace(salt), strings.TrimSpace(value)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// ConfuseCodexSessionForAccount 按账号混淆会话标识。
// 用于 prompt_cache_key（请求体）和 Session_id / Conversation_id（请求头）。
// 未启用、值为空或拿不到账号 salt 时原样返回，保证向后兼容。
// state 非 nil 时登记 {原值→混淆值} 供响应回写还原。
func ConfuseCodexSessionForAccount(account *auth.Account, sessionID string, state *CodexIdentityConfuseState) string {
	sessionID = strings.TrimSpace(sessionID)
	if !CodexIdentityConfuseEnabled() || sessionID == "" {
		return sessionID
	}
	salt := codexAccountIdentitySalt(account)
	if salt == "" {
		return sessionID
	}
	confused := codexIdentityConfuseUUID(salt, "session", sessionID)
	state.addPair(sessionID, confused)
	return confused
}

// ApplyCodexIdentityConfuseBody 混淆请求体中 client_metadata 内的身份字段。
// confusedSession 为已按账号混淆后的会话标识（与 prompt_cache_key 保持一致）。
// 处理字段：
//   - client_metadata.x-codex-installation-id   → 按账号确定性混淆
//   - client_metadata.x-codex-window-id         → confusedSession:0
//   - client_metadata.x-codex-turn-metadata     → 混淆内嵌 prompt_cache_key / window_id；
//     若 state 非 nil 则一并混淆 turn_id 并登记回写映射（完全体）。
func ApplyCodexIdentityConfuseBody(account *auth.Account, body []byte, confusedSession string, state *CodexIdentityConfuseState) []byte {
	if !CodexIdentityConfuseEnabled() || len(body) == 0 {
		return body
	}
	salt := codexAccountIdentitySalt(account)
	if salt == "" {
		return body
	}

	if installationID := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String()); installationID != "" {
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-installation-id", codexIdentityConfuseUUID(salt, "installation", installationID))
	}

	confusedSession = strings.TrimSpace(confusedSession)
	if confusedSession != "" {
		if windowID := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-window-id").String()); windowID != "" {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-window-id", confusedSession+":0")
		}
	}

	if turnMetadata := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()); turnMetadata != "" {
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", confuseCodexTurnMetadata(turnMetadata, confusedSession, salt, state))
	}

	return body
}

// ApplyCodexIdentityConfuseHeaders 混淆请求头中残留的会话身份标识。
// Session_id / Conversation_id 已由混淆后的 cacheKey 设置，这里仅补充处理
// X-Codex-Window-Id 与 X-Codex-Turn-Metadata（若下游透传了它们）。
// state 非 nil 时一并混淆 X-Codex-Turn-Metadata 内的 turn_id 并登记回写映射。
func ApplyCodexIdentityConfuseHeaders(account *auth.Account, headers http.Header, confusedSession string, state *CodexIdentityConfuseState) {
	if headers == nil || !CodexIdentityConfuseEnabled() {
		return
	}
	salt := codexAccountIdentitySalt(account)
	if salt == "" {
		return
	}

	if rawTurnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); rawTurnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", confuseCodexTurnMetadata(rawTurnMetadata, confusedSession, salt, state))
	}

	confusedSession = strings.TrimSpace(confusedSession)
	if confusedSession == "" {
		return
	}
	if strings.TrimSpace(headers.Get("X-Codex-Window-Id")) != "" {
		headers.Set("X-Codex-Window-Id", confusedSession+":0")
	}
}

// confuseCodexTurnMetadata 混淆 turn-metadata（JSON 字符串）内嵌的会话标识。
// 替换 prompt_cache_key / window_id；state 非 nil 时再混淆 turn_id 并登记回写映射。
func confuseCodexTurnMetadata(rawTurnMetadata, confusedSession, salt string, state *CodexIdentityConfuseState) string {
	updated := rawTurnMetadata
	confusedSession = strings.TrimSpace(confusedSession)
	if confusedSession != "" {
		if gjson.Get(rawTurnMetadata, "prompt_cache_key").Exists() {
			updated, _ = sjson.Set(updated, "prompt_cache_key", confusedSession)
		}
		if gjson.Get(rawTurnMetadata, "window_id").Exists() {
			updated, _ = sjson.Set(updated, "window_id", confusedSession+":0")
		}
	}
	// 完全体：混淆 turn_id 并记录 {原值→混淆值}，供响应回写还原。
	if state != nil && salt != "" {
		if turnID := strings.TrimSpace(gjson.Get(updated, "turn_id").String()); turnID != "" {
			confusedTurnID := codexIdentityConfuseUUID(salt, "turn", turnID)
			updated, _ = sjson.Set(updated, "turn_id", confusedTurnID)
			state.addPair(turnID, confusedTurnID)
		}
	}
	return updated
}

// RestoreCodexIdentityResponse 把响应字节里出现的混淆值还原成客户端原值
// （confused → original）。值均为 UUID/会话标识，全文替换不会误伤。
func RestoreCodexIdentityResponse(payload []byte, state *CodexIdentityConfuseState) []byte {
	if state.Empty() || len(payload) == 0 {
		return payload
	}
	for _, p := range state.pairs {
		if p.confused == "" || p.original == "" || p.confused == p.original {
			continue
		}
		if bytes.Contains(payload, []byte(p.confused)) {
			payload = bytes.ReplaceAll(payload, []byte(p.confused), []byte(p.original))
		}
	}
	return payload
}

// WrapCodexIdentityResponseBody 包装上游响应体，流式地把混淆值还原成客户端原值。
// state 为空时原样返回 body（零开销）。
func WrapCodexIdentityResponseBody(body io.ReadCloser, state *CodexIdentityConfuseState) io.ReadCloser {
	if body == nil || state.Empty() {
		return body
	}
	return &identityRestoreReader{src: body, state: state}
}

// identityRestoreReader 在读取上游响应时做 confused→original 替换。
// 混淆值（UUID/会话标识）不含换行，且在 SSE 中始终位于单个 data: 行内，
// 因此按"最后一个换行"切分：完整行立即替换输出（零额外延迟），只缓存尾部不完整行。
type identityRestoreReader struct {
	src     io.ReadCloser
	state   *CodexIdentityConfuseState
	pending []byte // 已读、尚未凑齐整行的尾部
	out     []byte // 已替换、待输出
	eof     bool
}

func (r *identityRestoreReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 && !r.eof {
		tmp := make([]byte, 32*1024)
		n, err := r.src.Read(tmp)
		if n > 0 {
			r.pending = append(r.pending, tmp[:n]...)
			// 按最后一个换行切分：完整行替换后输出，残留尾行留待下次
			if idx := bytes.LastIndexByte(r.pending, '\n'); idx >= 0 {
				complete := r.pending[:idx+1]
				tail := append([]byte(nil), r.pending[idx+1:]...)
				r.out = append(r.out, RestoreCodexIdentityResponse(complete, r.state)...)
				r.pending = tail
			}
		}
		if err != nil {
			if err == io.EOF {
				r.eof = true
				if len(r.pending) > 0 {
					r.out = append(r.out, RestoreCodexIdentityResponse(r.pending, r.state)...)
					r.pending = nil
				}
				break
			}
			// 非 EOF 错误：先吐出已处理数据，错误下次返回
			if len(r.out) > 0 {
				n := copy(p, r.out)
				r.out = r.out[n:]
				return n, nil
			}
			return 0, err
		}
	}

	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		if len(r.out) == 0 && r.eof {
			return n, io.EOF
		}
		return n, nil
	}
	if r.eof {
		return 0, io.EOF
	}
	return 0, nil
}

func (r *identityRestoreReader) Close() error {
	return r.src.Close()
}
