package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestConfuseCodexSessionForAccountDisabled(t *testing.T) {
	// 默认未设置环境变量 → 禁用，原样返回
	acc := &auth.Account{AccountID: "acc-1"}
	if got := ConfuseCodexSessionForAccount(acc, "session-1", NewCodexIdentityConfuseState()); got != "session-1" {
		t.Fatalf("disabled confuse = %q, want passthrough %q", got, "session-1")
	}
}

func TestConfuseCodexSessionForAccountIsolatesPerAccount(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")

	raw := "session-1"
	accA := &auth.Account{AccountID: "acc-A"}
	accB := &auth.Account{AccountID: "acc-B"}

	a1 := ConfuseCodexSessionForAccount(accA, raw, NewCodexIdentityConfuseState())
	a2 := ConfuseCodexSessionForAccount(accA, raw, NewCodexIdentityConfuseState())
	b1 := ConfuseCodexSessionForAccount(accB, raw, NewCodexIdentityConfuseState())

	if a1 == raw || b1 == raw {
		t.Fatalf("expected confused values, got a1=%q b1=%q raw=%q", a1, b1, raw)
	}
	if a1 != a2 {
		t.Fatalf("same account+value must be deterministic: a1=%q a2=%q", a1, a2)
	}
	if a1 == b1 {
		t.Fatalf("different accounts must not share session id: a1=%q b1=%q", a1, b1)
	}
}

func TestConfuseCodexSessionForAccountFallbackDBID(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")

	raw := "session-1"
	acc1 := &auth.Account{DBID: 1}
	acc2 := &auth.Account{DBID: 2}

	c1 := ConfuseCodexSessionForAccount(acc1, raw, NewCodexIdentityConfuseState())
	c2 := ConfuseCodexSessionForAccount(acc2, raw, NewCodexIdentityConfuseState())
	if c1 == raw || c2 == raw || c1 == c2 {
		t.Fatalf("DBID salt should produce distinct confused ids: c1=%q c2=%q raw=%q", c1, c2, raw)
	}
}

func TestConfuseCodexSessionForAccountNoSalt(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")
	// 无 AccountID 且 DBID<=0 → 拿不到 salt，原样返回
	acc := &auth.Account{}
	if got := ConfuseCodexSessionForAccount(acc, "session-1", NewCodexIdentityConfuseState()); got != "session-1" {
		t.Fatalf("no-salt confuse = %q, want passthrough %q", got, "session-1")
	}
}

func TestApplyCodexIdentityConfuseBodyWithTurnID(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")

	acc := &auth.Account{AccountID: "acc-A"}
	state := NewCodexIdentityConfuseState()
	confusedSession := ConfuseCodexSessionForAccount(acc, "session-1", state)

	body := []byte(`{
		"prompt_cache_key": "` + confusedSession + `",
		"client_metadata": {
			"x-codex-installation-id": "install-orig",
			"x-codex-window-id": "window-orig",
			"x-codex-turn-metadata": "{\"prompt_cache_key\":\"old\",\"window_id\":\"w-old\",\"turn_id\":\"turn-orig\"}"
		}
	}`)

	out := ApplyCodexIdentityConfuseBody(acc, body, confusedSession, state)

	// installation-id 应被混淆
	if got := gjson.GetBytes(out, "client_metadata.x-codex-installation-id").String(); got == "install-orig" || got == "" {
		t.Fatalf("installation id not confused: %q", got)
	}
	// window-id 应改为 confusedSession:0
	if got := gjson.GetBytes(out, "client_metadata.x-codex-window-id").String(); got != confusedSession+":0" {
		t.Fatalf("window id = %q, want %q", got, confusedSession+":0")
	}
	// turn-metadata：prompt_cache_key / window_id 混淆，turn_id 也被混淆（完全体）
	tm := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	if got := gjson.Get(tm, "prompt_cache_key").String(); got != confusedSession {
		t.Fatalf("turn-metadata prompt_cache_key = %q, want %q", got, confusedSession)
	}
	confusedTurnID := gjson.Get(tm, "turn_id").String()
	if confusedTurnID == "turn-orig" || confusedTurnID == "" {
		t.Fatalf("turn_id should be confused, got %q", confusedTurnID)
	}

	// 响应回写：上游回显混淆值时应被还原成客户端原值
	payload := []byte(`data: {"prompt_cache_key":"` + confusedSession + `","turn_id":"` + confusedTurnID + `"}` + "\n")
	restored := RestoreCodexIdentityResponse(payload, state)
	if strings.Contains(string(restored), confusedSession) || strings.Contains(string(restored), confusedTurnID) {
		t.Fatalf("restore should remove confused values, got %s", restored)
	}
	if !strings.Contains(string(restored), "session-1") || !strings.Contains(string(restored), "turn-orig") {
		t.Fatalf("restore should expose original values, got %s", restored)
	}
}

func TestApplyCodexIdentityConfuseBodyNilStateSkipsTurnID(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")
	acc := &auth.Account{AccountID: "acc-A"}
	confusedSession := ConfuseCodexSessionForAccount(acc, "session-1", nil)

	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-keep\"}"}}`)
	out := ApplyCodexIdentityConfuseBody(acc, body, confusedSession, nil)
	tm := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	if got := gjson.Get(tm, "turn_id").String(); got != "turn-keep" {
		t.Fatalf("nil state must skip turn_id, got %q", got)
	}
}

func TestApplyCodexIdentityConfuseBodyDisabled(t *testing.T) {
	acc := &auth.Account{AccountID: "acc-A"}
	body := []byte(`{"client_metadata":{"x-codex-installation-id":"install-orig"}}`)
	out := ApplyCodexIdentityConfuseBody(acc, body, "session-1", NewCodexIdentityConfuseState())
	if got := gjson.GetBytes(out, "client_metadata.x-codex-installation-id").String(); got != "install-orig" {
		t.Fatalf("disabled body confuse changed installation id: %q", got)
	}
}

func TestApplyCodexIdentityConfuseHeaders(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")

	acc := &auth.Account{AccountID: "acc-A"}
	state := NewCodexIdentityConfuseState()
	confusedSession := ConfuseCodexSessionForAccount(acc, "session-1", state)

	headers := http.Header{}
	headers.Set("X-Codex-Window-Id", "window-orig")
	headers.Set("X-Codex-Turn-Metadata", `{"prompt_cache_key":"old","turn_id":"turn-orig"}`)

	ApplyCodexIdentityConfuseHeaders(acc, headers, confusedSession, state)

	if got := headers.Get("X-Codex-Window-Id"); got != confusedSession+":0" {
		t.Fatalf("header window id = %q, want %q", got, confusedSession+":0")
	}
	tm := headers.Get("X-Codex-Turn-Metadata")
	if got := gjson.Get(tm, "prompt_cache_key").String(); got != confusedSession {
		t.Fatalf("header turn-metadata prompt_cache_key = %q, want %q", got, confusedSession)
	}
	if got := gjson.Get(tm, "turn_id").String(); got == "turn-orig" || got == "" {
		t.Fatalf("header turn_id should be confused, got %q", got)
	}
}

func TestRestoreCodexIdentityResponseEmptyState(t *testing.T) {
	payload := []byte("data: {\"x\":1}\n")
	if got := RestoreCodexIdentityResponse(payload, NewCodexIdentityConfuseState()); string(got) != string(payload) {
		t.Fatalf("empty state must passthrough, got %s", got)
	}
	if got := RestoreCodexIdentityResponse(payload, nil); string(got) != string(payload) {
		t.Fatalf("nil state must passthrough, got %s", got)
	}
}

func TestWrapCodexIdentityResponseBodyRestoresAcrossChunks(t *testing.T) {
	t.Setenv("CODEX_IDENTITY_CONFUSE", "true")
	acc := &auth.Account{AccountID: "acc-A"}
	state := NewCodexIdentityConfuseState()
	confused := ConfuseCodexSessionForAccount(acc, "orig-session", state)

	// 构造跨多个 SSE 行的响应，含混淆值
	full := "data: {\"turn\":\"" + confused + "\"}\n\ndata: {\"again\":\"" + confused + "\"}\n"
	// 用一个每次只吐 7 字节的 reader 模拟分块（含把 UUID 切断的边界）
	src := &chunkedReadCloser{data: []byte(full), chunk: 7}
	wrapped := WrapCodexIdentityResponseBody(src, state)

	got, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("read wrapped body: %v", err)
	}
	if strings.Contains(string(got), confused) {
		t.Fatalf("confused value leaked to client: %s", got)
	}
	want := strings.ReplaceAll(full, confused, "orig-session")
	if string(got) != want {
		t.Fatalf("restored mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestWrapCodexIdentityResponseBodyEmptyStatePassthrough(t *testing.T) {
	src := &chunkedReadCloser{data: []byte("data: hello\n"), chunk: 4}
	wrapped := WrapCodexIdentityResponseBody(src, NewCodexIdentityConfuseState())
	if wrapped != io.ReadCloser(src) {
		t.Fatalf("empty state should return original body unchanged")
	}
}

// chunkedReadCloser 把数据按固定大小分块返回，用于测试跨边界替换。
type chunkedReadCloser struct {
	data  []byte
	chunk int
	pos   int
}

func (c *chunkedReadCloser) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + c.chunk
	if end > len(c.data) {
		end = len(c.data)
	}
	n := copy(p, c.data[c.pos:end])
	c.pos += n
	return n, nil
}

func (c *chunkedReadCloser) Close() error { return nil }
