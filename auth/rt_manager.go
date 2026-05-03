package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RTManager 与外部 rt-manager (https://rt.iiusa.edu.kg) 服务的对接客户端。
//
// 背景：codex2api 现有 OAuth 流程使用 Codex CLI 的 client_id（app_EMo...rann）
// 刷新 RT；用户从 ChatGPT App 抓到的 RT 使用另一个 client_id（app_2SK...vigXD），
// 二者不能互换。AT-only 账号没有可用 RT，只能委托 rt-manager 用 ChatGPT
// App scope 的 RT 刷出新的 AT。
//
// rt-manager 的 RT 是 single-use（rotation），刷新成功后旧 RT 立即作废、
// 上游会下发新 RT。本 client 在每次刷新成功后都把新 RT PUT 回 rt-manager
// 持久化，否则下次会复用已作废的旧 RT 直接失败。
type RTManager struct {
	cfg atomic.Value // RTManagerConfig

	authMu          sync.Mutex
	bearerToken     string
	bearerExpiresAt time.Time

	indexMu      sync.RWMutex
	indexByEmail map[string]RTManagerAccount // email lower → account
	indexFetched time.Time

	httpClient *http.Client
}

// RTManagerConfig 由管理台 SystemSettings 持久化的运行时配置。
type RTManagerConfig struct {
	URL      string
	Password string
	Enabled  bool
}

// RTManagerAccount rt-manager /api/accounts 返回的账号子集。
type RTManagerAccount struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	RT    string `json:"rt"`
	AT    string `json:"at,omitempty"`
}

// RTRefreshResult /api/refresh 上游统一响应（兼容代理 + 直连 OpenAI 两种来源）。
type RTRefreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// rtManagerIndexTTL 索引缓存有效期：rt-manager 端账号变动后最多滞后 5 分钟。
const rtManagerIndexTTL = 5 * time.Minute

// rtManagerBearerLeeway 提前 30s 续 Bearer token，避免临界过期。
const rtManagerBearerLeeway = 30 * time.Second

// rtManagerBearerTTL rt-manager auth.js 默认 token 寿命 7 天，这里取保守值。
const rtManagerBearerTTL = 6 * 24 * time.Hour

// NewRTManager 构造一个未配置的 RTManager；调用 SetConfig 注入 URL+密码后才会工作。
func NewRTManager() *RTManager {
	r := &RTManager{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	r.cfg.Store(RTManagerConfig{})
	return r
}

// SetConfig 热替换配置；URL/Password 变化会强制重新登录并清空索引缓存。
func (r *RTManager) SetConfig(cfg RTManagerConfig) {
	prev, _ := r.cfg.Load().(RTManagerConfig)
	r.cfg.Store(cfg)
	if cfg.URL != prev.URL || cfg.Password != prev.Password {
		r.authMu.Lock()
		r.bearerToken = ""
		r.bearerExpiresAt = time.Time{}
		r.authMu.Unlock()

		r.indexMu.Lock()
		r.indexByEmail = nil
		r.indexFetched = time.Time{}
		r.indexMu.Unlock()
	}
}

// GetConfig 返回当前配置副本。
func (r *RTManager) GetConfig() RTManagerConfig {
	cfg, _ := r.cfg.Load().(RTManagerConfig)
	return cfg
}

// Enabled rt-manager 联动是否开启且配置完整。
func (r *RTManager) Enabled() bool {
	cfg := r.GetConfig()
	return cfg.Enabled && strings.TrimSpace(cfg.URL) != "" && strings.TrimSpace(cfg.Password) != ""
}

// Lookup 通过 email 查找 rt-manager 上对应的账号；找不到返回 ok=false。
// 索引超过 TTL 会自动刷新；登录/拉取失败返回 false。
func (r *RTManager) Lookup(ctx context.Context, email string) (RTManagerAccount, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return RTManagerAccount{}, false
	}
	if err := r.ensureIndex(ctx); err != nil {
		return RTManagerAccount{}, false
	}
	r.indexMu.RLock()
	defer r.indexMu.RUnlock()
	acc, ok := r.indexByEmail[email]
	return acc, ok
}

// Refresh 调 rt-manager /api/refresh 用 RT 换新 AT/RT/id_token。
func (r *RTManager) Refresh(ctx context.Context, refreshToken string) (*RTRefreshResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("refresh_token 为空")
	}
	cfg := r.GetConfig()
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("rt-manager URL 未配置")
	}
	endpoint, err := url.Parse(strings.TrimRight(cfg.URL, "/") + "/api/refresh")
	if err != nil {
		return nil, fmt.Errorf("rt-manager URL 无效: %w", err)
	}
	q := endpoint.Query()
	q.Set("refresh_token", refreshToken)
	q.Set("method", "ChatGPT")
	endpoint.RawQuery = q.Encode()

	body, status, err := r.do(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("rt-manager refresh 失败 (status %d): %s", status, truncateForRTM(body, 300))
	}
	var out RTRefreshResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("rt-manager refresh 响应解析失败: %w (body=%s)", err, truncateForRTM(body, 200))
	}
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return nil, fmt.Errorf("rt-manager refresh 响应缺少 access_token/refresh_token: %s", truncateForRTM(body, 200))
	}
	return &out, nil
}

// UpdateAccount 把刷新后得到的新 RT/AT 回写 rt-manager（PUT /api/accounts/:id）。
// rt-manager 端 PUT 必须带完整 body 且 rt 字段必填，因此这里会基于内存索引拼装。
func (r *RTManager) UpdateAccount(ctx context.Context, id string, refreshed *RTRefreshResult) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("rt-manager account id 为空")
	}
	if refreshed == nil {
		return errors.New("refreshed 为空")
	}
	cfg := r.GetConfig()
	if strings.TrimSpace(cfg.URL) == "" {
		return errors.New("rt-manager URL 未配置")
	}

	r.indexMu.RLock()
	current := r.findByIDLocked(id)
	r.indexMu.RUnlock()

	now := time.Now().Unix()
	atExp := now + refreshed.ExpiresIn
	payload := map[string]any{
		"id":              id,
		"email":           current.Email,
		"rt":              refreshed.RefreshToken,
		"at":              refreshed.AccessToken,
		"idToken":         refreshed.IDToken,
		"atExp":           atExp,
		"rtIssuedAt":      now,
		"lastRefreshAt":   now,
		"lastRefreshStatus": "ok",
		"lastRefreshError":  "",
		"createdAt":       current.toCreatedAtOr(now),
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(cfg.URL, "/") + "/api/accounts/" + url.PathEscape(id)
	respBody, status, err := r.do(ctx, http.MethodPut, endpoint, bodyBytes)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("rt-manager 更新账号失败 (status %d): %s", status, truncateForRTM(respBody, 200))
	}

	// 同步本地索引，避免并发刷新时复用旧 RT
	r.indexMu.Lock()
	if r.indexByEmail != nil && current.Email != "" {
		emailKey := strings.ToLower(strings.TrimSpace(current.Email))
		if existing, ok := r.indexByEmail[emailKey]; ok && existing.ID == id {
			existing.RT = refreshed.RefreshToken
			existing.AT = refreshed.AccessToken
			r.indexByEmail[emailKey] = existing
		}
	}
	r.indexMu.Unlock()
	return nil
}

// ensureIndex 必要时拉取 /api/accounts 重建 email 索引。
func (r *RTManager) ensureIndex(ctx context.Context) error {
	r.indexMu.RLock()
	fresh := r.indexByEmail != nil && time.Since(r.indexFetched) < rtManagerIndexTTL
	r.indexMu.RUnlock()
	if fresh {
		return nil
	}

	cfg := r.GetConfig()
	if strings.TrimSpace(cfg.URL) == "" {
		return errors.New("rt-manager URL 未配置")
	}
	endpoint := strings.TrimRight(cfg.URL, "/") + "/api/accounts"
	body, status, err := r.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("rt-manager 拉取账号列表失败 (status %d): %s", status, truncateForRTM(body, 200))
	}
	var resp struct {
		Accounts []RTManagerAccount `json:"accounts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("rt-manager 账号列表解析失败: %w", err)
	}
	idx := make(map[string]RTManagerAccount, len(resp.Accounts))
	for _, a := range resp.Accounts {
		key := strings.ToLower(strings.TrimSpace(a.Email))
		if key == "" {
			continue
		}
		idx[key] = a
	}
	r.indexMu.Lock()
	r.indexByEmail = idx
	r.indexFetched = time.Now()
	r.indexMu.Unlock()
	return nil
}

// findByIDLocked 调用方需持 indexMu 读锁。
func (r *RTManager) findByIDLocked(id string) RTManagerAccount {
	for _, a := range r.indexByEmail {
		if a.ID == id {
			return a
		}
	}
	return RTManagerAccount{ID: id}
}

// do 是带 Bearer token 的 HTTP 调用，401 时自动重登录重试一次。
func (r *RTManager) do(ctx context.Context, method, fullURL string, body []byte) ([]byte, int, error) {
	tok, err := r.bearer(ctx, false)
	if err != nil {
		return nil, 0, err
	}
	respBody, status, err := r.doWithToken(ctx, method, fullURL, body, tok)
	if err != nil {
		return nil, 0, err
	}
	if status == http.StatusUnauthorized {
		// token 过期或 rt-manager 重启清空 token store → 强制重登再试一次
		tok, err = r.bearer(ctx, true)
		if err != nil {
			return respBody, status, err
		}
		return r.doWithToken(ctx, method, fullURL, body, tok)
	}
	return respBody, status, nil
}

func (r *RTManager) doWithToken(ctx context.Context, method, fullURL string, body []byte, token string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("rt-manager 请求失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("rt-manager 响应读取失败: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// bearer 返回缓存的 Bearer token；过期或 force=true 时重新登录。
func (r *RTManager) bearer(ctx context.Context, force bool) (string, error) {
	r.authMu.Lock()
	defer r.authMu.Unlock()
	if !force && r.bearerToken != "" && time.Now().Add(rtManagerBearerLeeway).Before(r.bearerExpiresAt) {
		return r.bearerToken, nil
	}
	cfg := r.GetConfig()
	if strings.TrimSpace(cfg.URL) == "" || strings.TrimSpace(cfg.Password) == "" {
		return "", errors.New("rt-manager URL 或密码未配置")
	}
	loginPayload, _ := json.Marshal(map[string]string{"password": cfg.Password})
	endpoint := strings.TrimRight(cfg.URL, "/") + "/api/auth/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(loginPayload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("rt-manager 登录失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("rt-manager 登录失败 (status %d): %s", resp.StatusCode, truncateForRTM(respBody, 200))
	}
	var loginResp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(respBody, &loginResp); err != nil {
		return "", fmt.Errorf("rt-manager 登录响应解析失败: %w", err)
	}
	if loginResp.Token == "" {
		return "", errors.New("rt-manager 登录响应缺少 token")
	}
	r.bearerToken = loginResp.Token
	r.bearerExpiresAt = time.Now().Add(rtManagerBearerTTL)
	return r.bearerToken, nil
}

// toCreatedAtOr 用于 PUT 时保留原 createdAt 字段；缓存命中且非零则返回缓存值。
func (a RTManagerAccount) toCreatedAtOr(fallback int64) int64 {
	// rt-manager 暂未在 list 接口暴露 createdAt；保险起见直接用 fallback。
	_ = a
	return fallback
}

func truncateForRTM(body []byte, n int) string {
	if len(body) <= n {
		return strings.TrimSpace(string(body))
	}
	return strings.TrimSpace(string(body[:n])) + "...(truncated)"
}
