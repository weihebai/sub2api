package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"golang.org/x/sync/errgroup"
)

// Upstream balance probe snapshots live in accounts.extra under this key. The
// probe answers "how much credit does this upstream key still have". It runs
// manually (one account or a batch from the account table) and, when the
// global runner is enabled, on a per-account schedule for accounts that opted
// in via extra.upstream_balance_probe_enabled.
//
// Three upstream dialects are supported, in probe order:
//  1. sub2api upstreams: GET /v1/usage returns the key-scoped remaining quota
//     as a top-level "remaining" in USD.
//  2. new-api token quota: GET /api/usage/token (also served at
//     /v1/usage/token) returns the relay token's own remaining quota, and the
//     site-wide user quota when the token allows it. This is the only balance
//     endpoint a bare API key can reach on new-api.
//  3. new-api / one-api OpenAI-compatible billing: hard_limit_usd minus
//     total_usage. Kept as a fallback because some relays only implement this
//     pair, but it is unreliable on new-api (see the sentinel note below).
const UpstreamBalanceProbeExtraKey = "upstream_balance_probe"

// UpstreamBalanceProbeEnabledExtraKey is the per-account opt-in for the
// background balance refresh runner. Manual probes ignore it.
const UpstreamBalanceProbeEnabledExtraKey = "upstream_balance_probe_enabled"

const (
	// NewAPIAccessTokenCredentialKey holds a new-api "access token"
	// (个人资料 → 安全 → 访问令牌). Unlike the API key, this token can read the
	// site account's own balance from /api/user/self when paired with the
	// New-API-User header.
	NewAPIAccessTokenCredentialKey = "newapi_access_token"
	// NewAPIAccessTokenUserIDExtraKey stores the numeric site user ID used for
	// the New-API-User header. It is not secret, so it is safe to echo back so
	// the edit form can prefill it.
	NewAPIAccessTokenUserIDExtraKey = "newapi_access_token_user_id"
)

const (
	UpstreamBalanceKindSub2API     = "sub2api"
	UpstreamBalanceKindOpenAIBill  = "openai_billing"
	UpstreamBalanceKindNewAPIToken = "newapi_token"
	UpstreamBalanceKindNewAPIUser  = "newapi_user"
	upstreamBalanceProbeMaxBodyBty = upstreamBillingProbeMaxBodyBytes

	upstreamBalanceProbeDefaultIntervalMinutes = 30
	upstreamBalanceProbeMinIntervalMinutes     = 5
	upstreamBalanceProbeMaxIntervalMinutes     = 24 * 60
	upstreamBalanceProbeLeaderLockKey          = "upstream:billing:balance_probe:leader"
)

// UpstreamBalanceProbeSettings is the global runner switch shared with the
// admin settings page.
type UpstreamBalanceProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// GetUpstreamBalanceProbeSettings returns defaults when the setting is absent.
func (s *SettingService) GetUpstreamBalanceProbeSettings(ctx context.Context) (*UpstreamBalanceProbeSettings, error) {
	defaults := defaultUpstreamBalanceProbeSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyUpstreamBalanceProbeSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get upstream balance probe settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse upstream balance probe settings: %w", err)
	}
	if settings.IntervalMinutes == 0 {
		settings.IntervalMinutes = defaults.IntervalMinutes
	}
	normalizeUpstreamBalanceProbeSettings(&settings)
	return &settings, nil
}

// SetUpstreamBalanceProbeSettings validates and persists the runner settings.
func (s *SettingService) SetUpstreamBalanceProbeSettings(ctx context.Context, settings *UpstreamBalanceProbeSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is unavailable")
	}
	if settings == nil {
		return infraerrors.BadRequest("INVALID_UPSTREAM_BALANCE_PROBE_SETTINGS", "settings cannot be nil")
	}
	if settings.IntervalMinutes < upstreamBalanceProbeMinIntervalMinutes || settings.IntervalMinutes > upstreamBalanceProbeMaxIntervalMinutes {
		return infraerrors.BadRequest(
			"INVALID_UPSTREAM_BALANCE_PROBE_INTERVAL",
			fmt.Sprintf("interval_minutes must be between %d and %d", upstreamBalanceProbeMinIntervalMinutes, upstreamBalanceProbeMaxIntervalMinutes),
		)
	}
	normalizeUpstreamBalanceProbeSettings(settings)
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal upstream balance probe settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyUpstreamBalanceProbeSettings, string(data))
}

func defaultUpstreamBalanceProbeSettings() *UpstreamBalanceProbeSettings {
	return &UpstreamBalanceProbeSettings{Enabled: false, IntervalMinutes: upstreamBalanceProbeDefaultIntervalMinutes}
}

func normalizeUpstreamBalanceProbeSettings(settings *UpstreamBalanceProbeSettings) {
	if settings.IntervalMinutes < upstreamBalanceProbeMinIntervalMinutes {
		settings.IntervalMinutes = upstreamBalanceProbeMinIntervalMinutes
	}
	if settings.IntervalMinutes > upstreamBalanceProbeMaxIntervalMinutes {
		settings.IntervalMinutes = upstreamBalanceProbeMaxIntervalMinutes
	}
}

func upstreamBalanceProbeEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	enabled, ok := account.Extra[UpstreamBalanceProbeEnabledExtraKey].(bool)
	return ok && enabled
}

// applyNewAPIAccessToken stores the optional new-api access token in credentials
// and the (non-secret) site user ID in extra. Empty token means "leave as is";
// an explicit "-" clears it, mirroring how other managed credentials are reset.
func applyNewAPIAccessToken(account *Account, token, userID string) {
	if account == nil {
		return
	}
	token = strings.TrimSpace(token)
	if token == "-" {
		delete(account.Credentials, NewAPIAccessTokenCredentialKey)
		if account.Extra != nil {
			delete(account.Extra, NewAPIAccessTokenUserIDExtraKey)
		}
		return
	}
	if token != "" {
		if account.Credentials == nil {
			account.Credentials = make(map[string]any)
		}
		account.Credentials[NewAPIAccessTokenCredentialKey] = token
	}
	userID = strings.TrimSpace(userID)
	if userID != "" {
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[NewAPIAccessTokenUserIDExtraKey] = userID
	}
}

// newapiAccessToken reads the optional access token + site user ID.
func newapiAccessToken(account *Account) (token string, userID string) {
	if account == nil {
		return "", ""
	}
	if raw, ok := account.Credentials[NewAPIAccessTokenCredentialKey]; ok {
		if value, ok := raw.(string); ok {
			token = strings.TrimSpace(value)
		}
	}
	if account.Extra != nil {
		switch value := account.Extra[NewAPIAccessTokenUserIDExtraKey].(type) {
		case string:
			userID = strings.TrimSpace(value)
		case float64:
			userID = strconv.FormatInt(int64(value), 10)
		case int64:
			userID = strconv.FormatInt(value, 10)
		}
	}
	return token, userID
}

// UpstreamBalanceProbeSnapshot mirrors the billing probe snapshot shape so the
// account table can render both columns with the same staleness logic.
type UpstreamBalanceProbeSnapshot struct {
	Status        string         `json:"status"`
	Data          map[string]any `json:"data,omitempty"`
	ReceivedAt    *time.Time     `json:"received_at,omitempty"`
	FreshUntil    *time.Time     `json:"fresh_until,omitempty"`
	LastAttemptAt time.Time      `json:"last_attempt_at"`
	NextProbeAt   time.Time      `json:"next_probe_at"`
	FailureCount  int            `json:"failure_count,omitempty"`
	HTTPStatus    int            `json:"http_status,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
}

// UpstreamBalanceProbeResult is returned by the manual probe endpoints.
type UpstreamBalanceProbeResult struct {
	AccountID int64                        `json:"account_id"`
	Snapshot  *UpstreamBalanceProbeSnapshot `json:"snapshot,omitempty"`
	Error     string                       `json:"error,omitempty"`
}

type upstreamBalanceProbeSnapshotWriter interface {
	UpdateUpstreamBalanceProbeSnapshot(context.Context, *Account, *UpstreamBalanceProbeSnapshot) error
}

func decodeUpstreamBalanceProbeSnapshot(extra map[string]any) *UpstreamBalanceProbeSnapshot {
	if extra == nil {
		return nil
	}
	value, ok := extra[UpstreamBalanceProbeExtraKey]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshot UpstreamBalanceProbeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	if snapshot.Status != UpstreamBillingProbeStatusOK &&
		snapshot.Status != UpstreamBillingProbeStatusUnsupported &&
		snapshot.Status != UpstreamBillingProbeStatusFailed {
		return nil
	}
	return &snapshot
}

// BuildUpstreamBalanceSnapshotItems projects account rows into the payload used
// by the account table refresh. Malformed or legacy extra data decodes to nil
// consistently with probe logic.
func BuildUpstreamBalanceSnapshotItems(accounts []Account) []UpstreamBalanceProbeResult {
	items := make([]UpstreamBalanceProbeResult, 0, len(accounts))
	for _, account := range accounts {
		var snapshot *UpstreamBalanceProbeSnapshot
		if account.Type == AccountTypeAPIKey {
			snapshot = decodeUpstreamBalanceProbeSnapshot(account.Extra)
		}
		items = append(items, UpstreamBalanceProbeResult{
			AccountID: account.ID,
			Snapshot:  snapshot,
		})
	}
	return items
}

// ProbeUpstreamBalanceAccount performs one manual balance probe. It ignores the
// per-account probe switches on purpose: balance display is read-only and
// never writes rates, so there is nothing to opt into or out of.
func (s *UpstreamBillingProbeService) ProbeUpstreamBalanceAccount(ctx context.Context, accountID int64) (*UpstreamBalanceProbeSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	settings, err := s.getUpstreamBalanceSettings(ctx)
	if err != nil {
		return nil, err
	}
	return s.probeUpstreamBalanceWithMode(ctx, accountID, settings.IntervalMinutes, false)
}

// GetUpstreamBalanceProbeSettings exposes the runner settings to the admin API.
func (s *UpstreamBillingProbeService) GetUpstreamBalanceProbeSettings(ctx context.Context) (*UpstreamBalanceProbeSettings, error) {
	return s.getUpstreamBalanceSettings(ctx)
}

// UpdateUpstreamBalanceProbeSettings persists the runner settings.
func (s *UpstreamBillingProbeService) UpdateUpstreamBalanceProbeSettings(ctx context.Context, settings *UpstreamBalanceProbeSettings) error {
	if s == nil || s.settingService == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	return s.settingService.SetUpstreamBalanceProbeSettings(ctx, settings)
}

// SetUpstreamBalanceProbeEnabled flips the per-account runner opt-in.
func (s *UpstreamBillingProbeService) SetUpstreamBalanceProbeEnabled(ctx context.Context, accountID int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return ErrUpstreamBillingProbeAccountInvalid
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{UpstreamBalanceProbeEnabledExtraKey: enabled})
}

func (s *UpstreamBillingProbeService) getUpstreamBalanceSettings(ctx context.Context) (*UpstreamBalanceProbeSettings, error) {
	if s.settingService == nil {
		return defaultUpstreamBalanceProbeSettings(), nil
	}
	return s.settingService.GetUpstreamBalanceProbeSettings(ctx)
}

// RunUpstreamBalanceDue executes at most one bounded batch of due accounts.
// It shares the leader lock, singleflight group and concurrency slots with the
// rate probe but uses its own cadence lock so the two runners do not starve
// each other.
func (s *UpstreamBillingProbeService) RunUpstreamBalanceDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	settings, err := s.getUpstreamBalanceSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	lockNow := time.Now()
	cadenceKey := upstreamBalanceProbeLeaderLockKey + ":" + strconv.FormatInt(lockNow.Truncate(upstreamBillingProbeCycleInterval).Unix(), 10)
	cadenceRelease, acquired, lockErr := s.tryAcquireLeaderLock(ctx, cadenceKey)
	if lockErr != nil {
		return fmt.Errorf("acquire upstream balance probe cadence lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer releaseUpstreamBillingProbeLeaderLock(cadenceRelease, lockNow.Truncate(upstreamBillingProbeCycleInterval).Add(upstreamBillingProbeCycleInterval))

	now := s.currentTime()
	accounts, err := s.accountRepo.FindByExtraField(ctx, UpstreamBalanceProbeEnabledExtraKey, true)
	if err != nil {
		return fmt.Errorf("list enabled upstream balance probes: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !isUpstreamBillingProbeAccount(&account) || !account.IsActive() || !upstreamBalanceProbeEnabled(&account) {
			continue
		}
		snapshot := decodeUpstreamBalanceProbeSnapshot(account.Extra)
		if snapshot != nil && !snapshot.NextProbeAt.IsZero() && now.Before(snapshot.NextProbeAt) {
			continue
		}
		due = append(due, account)
	}
	sort.SliceStable(due, func(i, j int) bool {
		left := decodeUpstreamBalanceProbeSnapshot(due[i].Extra)
		right := decodeUpstreamBalanceProbeSnapshot(due[j].Extra)
		leftUnset := left == nil || left.NextProbeAt.IsZero()
		rightUnset := right == nil || right.NextProbeAt.IsZero()
		if leftUnset && rightUnset {
			return due[i].ID < due[j].ID
		}
		if leftUnset {
			return true
		}
		if rightUnset {
			return false
		}
		return left.NextProbeAt.Before(right.NextProbeAt)
	})
	if len(due) > upstreamBillingProbeMaxPerCycle {
		due = due[:upstreamBillingProbeMaxPerCycle]
	}

	var group errgroup.Group
	for i := range due {
		accountID := due[i].ID
		group.Go(func() error {
			if _, probeErr := s.probeUpstreamBalanceWithMode(ctx, accountID, settings.IntervalMinutes, true); probeErr != nil {
				logger.LegacyPrintf("service.upstream_balance_probe", "balance_probe_due_failed: account_id=%d err=%v", accountID, probeErr)
			}
			return nil
		})
	}
	return group.Wait()
}

// startUpstreamBalanceLoop runs the balance runner on the same ticker cadence
// as the rate probe loop.
func (s *UpstreamBillingProbeService) startUpstreamBalanceLoop(ctx context.Context) {
	_ = s.RunUpstreamBalanceDue(ctx)
	ticker := time.NewTicker(upstreamBillingProbeCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunUpstreamBalanceDue(ctx); err != nil {
				logger.LegacyPrintf("service.upstream_balance_probe", "run_due_failed: err=%v", err)
			}
		}
	}
}

func (s *UpstreamBillingProbeService) probeUpstreamBalanceWithMode(ctx context.Context, accountID int64, intervalMinutes int, requireEnabled bool) (*UpstreamBalanceProbeSnapshot, error) {
	key := "balance:" + strconv.FormatInt(accountID, 10)
	value, err, _ := s.probeGroup.Do(key, func() (any, error) {
		select {
		case s.probeSlots <- struct{}{}:
			defer func() { <-s.probeSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		account, loadErr := s.accountRepo.GetByID(ctx, accountID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		if requireEnabled {
			if !account.IsActive() || !upstreamBalanceProbeEnabled(account) {
				return nil, nil
			}
			if snapshot := decodeUpstreamBalanceProbeSnapshot(account.Extra); snapshot != nil &&
				!snapshot.NextProbeAt.IsZero() && s.currentTime().Before(snapshot.NextProbeAt) {
				return nil, nil
			}
		}
		return s.probeUpstreamBalanceLoaded(ctx, account, intervalMinutes)
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	snapshot, ok := value.(*UpstreamBalanceProbeSnapshot)
	if !ok {
		return nil, fmt.Errorf("invalid upstream balance probe result")
	}
	return snapshot, nil
}


// ProbeUpstreamBalanceAccounts performs a bounded manual batch with the same
// service-wide concurrency limit as the rate probe.
func (s *UpstreamBillingProbeService) ProbeUpstreamBalanceAccounts(ctx context.Context, accountIDs []int64) []UpstreamBalanceProbeResult {
	if len(accountIDs) > UpstreamBillingProbeMaxBatchSize {
		accountIDs = accountIDs[:UpstreamBillingProbeMaxBatchSize]
	}
	results := make([]UpstreamBalanceProbeResult, len(accountIDs))
	if s == nil || s.accountRepo == nil {
		for i, accountID := range accountIDs {
			results[i] = UpstreamBalanceProbeResult{AccountID: accountID, Error: ErrUpstreamBillingProbeUnavailable.Error()}
		}
		return results
	}
	var group errgroup.Group
	for i, accountID := range accountIDs {
		i, accountID := i, accountID
		results[i].AccountID = accountID
		group.Go(func() error {
			snapshot, err := s.ProbeUpstreamBalanceAccount(ctx, accountID)
			if err != nil {
				results[i].Error = safeProbeError(err)
				return nil
			}
			results[i].Snapshot = snapshot
			return nil
		})
	}
	_ = group.Wait()
	return results
}

func (s *UpstreamBillingProbeService) probeUpstreamBalanceLoaded(ctx context.Context, account *Account, intervalMinutes int) (*UpstreamBalanceProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, "transport_unavailable", 0)
	}
	apiKey := account.GetCredential("api_key")
	if apiKey == "" {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, "missing_api_key", 0)
	}
	baseURL := account.GetCredential("base_url")
	if account.RoutesProtocolByInbound() && account.IsAdaptiveAPIProtocol() {
		baseURL = account.GetCNProtocolBaseURL(APIProtocolChatCompletions)
	}
	// 官方 API 一律不发请求：官方 billing 端点需要组织级管理权限，拿账号
	// key 周期性去敲只会得到 401/403 噪音。OpenAI 平台也不像倍率探测那样
	// 回退到官方域——余额探测没有"官方语义"可保持。
	if upstreamBillingProbeTargetIsOfficialAPI(baseURL) {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, "unsupported", 0)
	}
	normalizedBaseURL, err := s.accountTestService.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, "invalid_base_url", 0)
	}
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, "proxy_unavailable", 0)
		}
		if account.Proxy.ID != *account.ProxyID {
			return nil, ErrUpstreamBillingProbeIdentityChanged
		}
		proxyURL = account.Proxy.URL()
	}

	profile := HTTPUpstreamProfileDefault
	if account.Platform == PlatformOpenAI {
		profile = HTTPUpstreamProfileOpenAI
	}

	doGetWith := func(endpoint string, query url.Values, bearer string, extraHeaders map[string]string) (int, []byte, http.Header, error) {
		probeURL := buildOpenAIEndpointURL(normalizedBaseURL, endpoint)
		if len(query) > 0 {
			// buildOpenAIEndpointURL 把入参当纯路径拼接（会把 "?" 转义成
			// %3F），查询串必须在拿到最终 URL 之后再挂。
			probeURL += "?" + query.Encode()
		}
		probeCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, bytes.NewReader(nil))
		if err != nil {
			return 0, nil, nil, fmt.Errorf("request_build_failed")
		}
		reqCtx := WithHTTPUpstreamProfile(req.Context(), profile)
		req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		for name, value := range extraHeaders {
			req.Header.Set(name, value)
		}
		account.ApplyHeaderOverrides(req.Header)
		var tlsProfile *tlsfingerprint.Profile
		if s.accountTestService.tlsFPProfileService != nil {
			tlsProfile = s.accountTestService.tlsFPProfileService.ResolveTLSProfile(account)
		}
		resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("request_failed")
		}
		if resp == nil || resp.Body == nil {
			return 0, nil, nil, fmt.Errorf("empty_response")
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamBalanceProbeMaxBodyBty+1))
		if readErr != nil {
			return resp.StatusCode, nil, resp.Header, fmt.Errorf("response_read_failed")
		}
		if len(body) > upstreamBalanceProbeMaxBodyBty {
			return resp.StatusCode, nil, resp.Header, fmt.Errorf("response_too_large")
		}
		return resp.StatusCode, body, resp.Header, nil
	}
	doGet := func(endpoint string, query url.Values) (int, []byte, http.Header, error) {
		return doGetWith(endpoint, query, apiKey, nil)
	}

	// 尝试 0：账号配了 new-api 访问令牌时，直接读站点账户真实余额。
	// 这是唯一能拿到与钱包页一致数字的路径——裸 API Key 访问
	// /api/user/self 会 401。
	if accessToken, accessUserID := newapiAccessToken(account); accessToken != "" {
		headers := map[string]string{"New-API-User": accessUserID}
		if accessUserID == "" {
			headers = nil
		}
		status, body, header, tokenErr := doGetWith("/api/user/self", nil, accessToken, headers)
		if tokenErr == nil && status >= 200 && status < 300 {
			if data, ok := parseUpstreamBalanceNewAPIUserSelf(body, now); ok {
				return s.persistUpstreamBalanceSuccess(ctx, account, intervalMinutes, now, status, data)
			}
		} else if tokenErr == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
			// 令牌失效：保留旧值并提示管理员更新，不再回退到注定失败的公开端点。
			return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, status, "access_token_rejected", 0)
		}
		_ = header
	}

	// 尝试 1：sub2api 上游（/v1/usage 顶层 remaining，USD）。
	status, body, header, reqErr := doGet("/v1/usage", nil)
	if reqErr == nil {
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			// 端点不存在 → 继续尝试 new-api / one-api 方言。
		} else if status < 200 || status >= 300 {
			return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, status, "http_error", retryAfter(header, now))
		} else if data, ok := parseUpstreamBalanceSub2APIUsage(body, now); ok {
			return s.persistUpstreamBalanceSuccess(ctx, account, intervalMinutes, now, status, data)
		}
		// 200 但没有可识别的 remaining 字段 → 不是 sub2api 方言，继续尝试。
	} else if reqErr.Error() != "request_build_failed" && reqErr.Error() != "response_read_failed" && reqErr.Error() != "response_too_large" {
		// 网络层失败（request_failed / empty_response）没有再试第二个端点的
		// 意义——同一个上游，第二个端点大概率同样失败；直接记失败。
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, reqErr.Error(), 0)
	}

	// 尝试 2：new-api 令牌配额端点。这是唯一能用裸 API Key 拿到「令牌真实
	// 剩余」的端点；/api/usage/token 与 /v1/usage/token 是同一 handler 的
	// 两个前缀，按顺序都试一下，兼容只开放其中一个的站点。
	for _, tokenPath := range []string{"/api/usage/token", "/v1/usage/token"} {
		tokenStatus, tokenBody, tokenHeader, tokenErr := doGet(tokenPath, nil)
		if tokenErr != nil {
			if tokenPath == "/api/usage/token" {
				continue
			}
			break
		}
		if tokenStatus == http.StatusNotFound || tokenStatus == http.StatusMethodNotAllowed {
			continue
		}
		if tokenStatus < 200 || tokenStatus >= 300 {
			// 401/403：令牌不允许查询，落到 billing 方言兜底。
			if tokenPath == "/api/usage/token" {
				continue
			}
			break
		}
		if data, ok := parseUpstreamBalanceTokenUsage(tokenBody, upstreamBalanceDefaultQuotaPerUnit, now); ok {
			if _, hasRemaining := data["remaining_usd"]; hasRemaining {
				return s.persistUpstreamBalanceSuccess(ctx, account, intervalMinutes, now, tokenStatus, data)
			}
			// 令牌不限额度但站点未开放用户额度 → 记录这一事实后继续兜底。
			_ = tokenHeader
		}
	}

	// 尝试 3：new-api / one-api 的 OpenAI 兼容 billing 方言（兜底）。
	subStatus, subBody, subHeader, subErr := doGet("/v1/dashboard/billing/subscription", nil)
	if subErr != nil {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, subErr.Error(), 0)
	}
	if subStatus == http.StatusNotFound || subStatus == http.StatusMethodNotAllowed {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, subStatus, "unsupported", retryAfter(subHeader, now))
	}
	if subStatus < 200 || subStatus >= 300 {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, subStatus, "http_error", retryAfter(subHeader, now))
	}
	usageEnd := now.AddDate(0, 0, 1)
	usageStart := usageEnd.AddDate(-1, 0, 0)
	usageQuery := url.Values{}
	usageQuery.Set("start_date", usageStart.Format("2006-01-02"))
	usageQuery.Set("end_date", usageEnd.Format("2006-01-02"))
	usageStatus, usageBody, usageHeader, usageErr := doGet("/v1/dashboard/billing/usage", usageQuery)
	if usageErr != nil {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, 0, usageErr.Error(), 0)
	}
	if usageStatus < 200 || usageStatus >= 300 {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, usageStatus, "http_error", retryAfter(usageHeader, now))
	}
	data, err := parseUpstreamBalanceOpenAIBilling(subBody, usageBody, now)
	if err != nil {
		return s.persistUpstreamBalanceFailure(ctx, account, intervalMinutes, now, subStatus, "invalid_response", retryAfter(subHeader, now))
	}
	// new-api 对不限额度的令牌把 hard_limit_usd 写成 100000000 哨兵值，此时
	// 拿不到真实余额，只剩上游自己的"已用量"数字。
	if unlimited, _ := data["unlimited"].(bool); unlimited {
		snapshot := &UpstreamBalanceProbeSnapshot{
			Status:        UpstreamBillingProbeStatusOK,
			Data:          data,
			ReceivedAt:    probeTimePtr(now),
			FreshUntil:    probeTimePtr(now.Add(2 * time.Duration(intervalMinutes) * time.Minute)),
			LastAttemptAt: now,
			NextProbeAt:   now.Add(nextProbeDelay(intervalMinutes, 0)),
			HTTPStatus:    subStatus,
		}
		if err := s.updateUpstreamBalanceSnapshot(ctx, account, snapshot); err != nil {
			return nil, err
		}
		return snapshot, nil
	}
	return s.persistUpstreamBalanceSuccess(ctx, account, intervalMinutes, now, subStatus, data)
}

func (s *UpstreamBillingProbeService) persistUpstreamBalanceSuccess(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	data map[string]any,
) (*UpstreamBalanceProbeSnapshot, error) {
	snapshot := &UpstreamBalanceProbeSnapshot{
		Status:        UpstreamBillingProbeStatusOK,
		Data:          data,
		ReceivedAt:    probeTimePtr(now),
		FreshUntil:    probeTimePtr(now.Add(2 * time.Duration(intervalMinutes) * time.Minute)),
		LastAttemptAt: now,
		NextProbeAt:   now.Add(nextProbeDelay(intervalMinutes, 0)),
		HTTPStatus:    statusCode,
	}
	if err := s.updateUpstreamBalanceSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) persistUpstreamBalanceFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
	retryAfterDuration time.Duration,
) (*UpstreamBalanceProbeSnapshot, error) {
	previous := decodeUpstreamBalanceProbeSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	status := UpstreamBillingProbeStatusFailed
	delay := nextProbeDelay(intervalMinutes, retryAfterDuration)
	if reason == "unsupported" {
		status = UpstreamBillingProbeStatusUnsupported
		delay = unsupportedProbeDelay(intervalMinutes, retryAfterDuration)
	}
	snapshot := &UpstreamBalanceProbeSnapshot{
		Status:        status,
		LastAttemptAt: now,
		NextProbeAt:   now.Add(delay),
		FailureCount:  failureCount,
		HTTPStatus:    statusCode,
		LastError:     reason,
	}
	if previous != nil {
		snapshot.Data = previous.Data
		snapshot.ReceivedAt = previous.ReceivedAt
		snapshot.FreshUntil = previous.FreshUntil
		if snapshot.FreshUntil == nil && previous.Status == UpstreamBillingProbeStatusOK && previous.ReceivedAt != nil {
			snapshot.FreshUntil = probeTimePtr(previous.ReceivedAt.Add(2 * time.Duration(intervalMinutes) * time.Minute))
		}
	}
	if err := s.updateUpstreamBalanceSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) updateUpstreamBalanceSnapshot(ctx context.Context, account *Account, snapshot *UpstreamBalanceProbeSnapshot) error {
	writer, ok := s.accountRepo.(upstreamBalanceProbeSnapshotWriter)
	if !ok {
		return ErrUpstreamBillingProbeUnavailable
	}
	return writer.UpdateUpstreamBalanceProbeSnapshot(ctx, account, snapshot)
}

// parseUpstreamBalanceSub2APIUsage reads a sub2api /v1/usage payload. Both the
// quota_limited and unrestricted (subscription / wallet) modes expose a
// top-level "remaining" in USD; wallet mode also exposes "balance".
func parseUpstreamBalanceSub2APIUsage(body []byte, now time.Time) (map[string]any, bool) {
	var payload struct {
		Mode      string          `json:"mode"`
		Remaining *float64        `json:"remaining"`
		Balance   *float64        `json:"balance"`
		IsValid   *bool           `json:"isValid"`
		Quota     *struct {
			Limit     *float64 `json:"limit"`
			Used      *float64 `json:"used"`
			Remaining *float64 `json:"remaining"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	remaining := payload.Remaining
	if remaining == nil {
		remaining = payload.Balance
	}
	if remaining == nil || !finiteNonNegative(*remaining) {
		return nil, false
	}
	data := map[string]any{
		"kind":         UpstreamBalanceKindSub2API,
		"remaining_usd": *remaining,
		"observed_at":  now.UTC().Format(time.RFC3339Nano),
	}
	if payload.Mode != "" {
		data["mode"] = payload.Mode
	}
	if payload.Quota != nil && payload.Quota.Limit != nil && *payload.Quota.Limit > 0 {
		if finiteNonNegative(*payload.Quota.Limit) {
			data["quota_limit_usd"] = *payload.Quota.Limit
		}
		if payload.Quota.Used != nil && finiteNonNegative(*payload.Quota.Used) {
			data["quota_used_usd"] = *payload.Quota.Used
		}
	}
	return data, true
}

// parseUpstreamBalanceNewAPIUserSelf reads new-api's /api/user/self payload,
// which is the only endpoint that reports the site account's real wallet
// balance. It requires an access token plus the New-API-User header, so it is
// only attempted when the admin supplied one.
//
// The payload nests the numbers under "data":
//
//	{"success":true,"data":{"id":1,"quota":107550,"used_quota":20976980}}
//
// where quota is the remaining credit and used_quota the consumed credit, both
// in new-api quota units convertible via quota_per_unit (default 500000/USD).
func parseUpstreamBalanceNewAPIUserSelf(body []byte, now time.Time) (map[string]any, bool) {
	var payload struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Data    *struct {
			ID             *float64 `json:"id"`
			Quota          *float64 `json:"quota"`
			UsedQuota      *float64 `json:"used_quota"`
			Username       string   `json:"username"`
			DisplayName    string   `json:"display_name"`
			QuotaPerUnit   *float64 `json:"quota_per_unit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	if payload.Success != nil && !*payload.Success {
		return nil, false
	}
	if payload.Data == nil || payload.Data.Quota == nil {
		return nil, false
	}
	unit := upstreamBalanceDefaultQuotaPerUnit
	if payload.Data.QuotaPerUnit != nil && *payload.Data.QuotaPerUnit > 0 {
		unit = *payload.Data.QuotaPerUnit
	}
	remaining := *payload.Data.Quota / unit
	// 余额可以是负数（欠费），不能截断成 0。
	if math.IsNaN(remaining) || math.IsInf(remaining, 0) {
		return nil, false
	}
	data := map[string]any{
		"kind":          UpstreamBalanceKindNewAPIUser,
		"remaining_usd": remaining,
		"quota_per_unit": unit,
		"observed_at":   now.UTC().Format(time.RFC3339Nano),
	}
	if payload.Data.UsedQuota != nil && finiteNonNegative(*payload.Data.UsedQuota) {
		data["used_usd"] = *payload.Data.UsedQuota / unit
		data["total_usd"] = (*payload.Data.Quota + *payload.Data.UsedQuota) / unit
	}
	if payload.Data.Username != "" {
		data["token_name"] = payload.Data.Username
	}
	return data, true
}

// parseUpstreamBalanceOpenAIBilling reads the new-api / one-api billing pair.
// subscription.hard_limit_usd is the total credit; usage.total_usage is the
// used credit. new-api reports total_usage in the same dollars as the limit;
// legacy one-api multiplied by 100 (US cents). Because used can never exceed
// the total, a total_usage above the limit is normalized by dividing by 100 —
// a heuristic that also self-corrects CNY-displayed relays whose converted
// limit and usage share the same scale factor.
func parseUpstreamBalanceOpenAIBilling(subscriptionBody, usageBody []byte, now time.Time) (map[string]any, error) {
	var subscription struct {
		HardLimitUSD       *float64        `json:"hard_limit_usd"`
		SystemHardLimitUSD *float64        `json:"system_hard_limit_usd"`
		AccessUntil        json.RawMessage `json:"access_until"`
		Error              json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(subscriptionBody, &subscription); err != nil {
		return nil, err
	}
	if len(subscription.Error) > 0 && string(subscription.Error) != "null" && string(subscription.Error) != "" {
		return nil, fmt.Errorf("upstream returned a billing error")
	}
	total := subscription.HardLimitUSD
	if total == nil || *total <= 0 || !finiteNonNegative(*total) {
		total = subscription.SystemHardLimitUSD
	}
	if total == nil || *total <= 0 || math.IsNaN(*total) || math.IsInf(*total, 0) {
		return nil, fmt.Errorf("missing hard_limit_usd")
	}
	var usage struct {
		TotalUsage *float64        `json:"total_usage"`
		Error      json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(usageBody, &usage); err != nil {
		return nil, err
	}
	if len(usage.Error) > 0 && string(usage.Error) != "null" && string(usage.Error) != "" {
		return nil, fmt.Errorf("upstream returned a usage error")
	}
	if usage.TotalUsage == nil || !finiteNonNegative(*usage.TotalUsage) {
		return nil, fmt.Errorf("missing total_usage")
	}
	used := *usage.TotalUsage
	if used > *total {
		used /= 100
	}
	if used < 0 || math.IsNaN(used) || math.IsInf(used, 0) {
		return nil, fmt.Errorf("invalid total_usage")
	}
	remaining := *total - used
	if remaining < 0 {
		remaining = 0
	}
	data := map[string]any{
		"kind":          UpstreamBalanceKindOpenAIBill,
		"remaining_usd": remaining,
		"total_usd":     *total,
		"used_usd":      used,
		"observed_at":   now.UTC().Format(time.RFC3339Nano),
	}
	// new-api 把「令牌/用户组不限额度」直接写成 100000000 这个魔法值
	// （见 controller/billing.go：token.UnlimitedQuota 时 amount=1e8）。
	// 此时 hard_limit_usd 不是真实总额，remaining 也没有意义。
	//
	// 更要紧的是：total_usage 的单位是站点自己的计费单位，不是美元。
	// 实测两个 new-api 站点返回 40.5428 / 4142.237，而钱包页分别是
	// $0.4054 / $49.97——比值 100 与 82.9，没有任何一个站点配置字段
	// （quota_per_unit / price / usd_exchange_rate / custom_currency_exchange_rate）
	// 能同时解释两者。因此这里绝不把它当美元展示，只保留原始数字。
	if upstreamBalanceIsSentinelUnlimited(*total) {
		data["unlimited"] = true
		delete(data, "remaining_usd")
		delete(data, "total_usd")
		delete(data, "used_usd")
		data["upstream_used_raw"] = used
	}
	return data, nil
}

// upstreamBalanceSentinelUnlimitedLimit is the constant new-api writes into
// hard_limit_usd when the token has UnlimitedQuota set.
const upstreamBalanceSentinelUnlimitedLimit = 100000000.0

func upstreamBalanceIsSentinelUnlimited(total float64) bool {
	return math.Abs(total-upstreamBalanceSentinelUnlimitedLimit) < 0.5
}

// parseUpstreamBalanceTokenUsage reads the new-api token-scoped credit
// summary, which is the only balance endpoint reachable with a bare API key.
// new-api exposes the same payload on two paths:
//
//	GET /api/usage/token   (Authorization: Bearer sk-...)
//	GET /v1/usage/token    (relay prefix variant served by the same handler)
//
// The numeric fields are new-api "quota" units, not dollars, so the caller
// passes the relay's QuotaPerUnit (500000 by default) to convert them.
func parseUpstreamBalanceTokenUsage(body []byte, quotaPerUnit float64, now time.Time) (map[string]any, bool) {
	var payload struct {
		Code    *bool  `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Object           string   `json:"object"`
			Name             string   `json:"name"`
			TotalGranted     *float64 `json:"total_granted"`
			TotalUsed        *float64 `json:"total_used"`
			TotalAvailable   *float64 `json:"total_available"`
			UnlimitedQuota   bool     `json:"unlimited_quota"`
			ExpiresAt        *float64 `json:"expires_at"`
			UserQuota        *struct {
				TotalAvailable *float64 `json:"total_available"`
				TotalUsed      *float64 `json:"total_used"`
			} `json:"user_quota"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	// new-api answers with {"code":true,"message":"ok","data":{...}}; anything
	// else (an error envelope, a plain OpenAI relay 404 body) is not this
	// dialect.
	if payload.Code == nil || !*payload.Code || payload.Data == nil {
		return nil, false
	}
	unit := quotaPerUnit
	if unit <= 0 {
		unit = upstreamBalanceDefaultQuotaPerUnit
	}
	data := map[string]any{
		"kind":             UpstreamBalanceKindNewAPIToken,
		"observed_at":      now.UTC().Format(time.RFC3339Nano),
		"unlimited":        payload.Data.UnlimitedQuota,
		"quota_per_unit":   unit,
	}
	if payload.Data.Name != "" {
		data["token_name"] = payload.Data.Name
	}
	// 令牌开启了「允许查询用户额度」时，user_quota 才是站点账户的真实余额
	// （用户余额模式的站点里，令牌自身的 remain_quota 恒为无限）。
	if payload.Data.UserQuota != nil && payload.Data.UserQuota.TotalAvailable != nil &&
		finiteNonNegative(*payload.Data.UserQuota.TotalAvailable) {
		data["remaining_usd"] = *payload.Data.UserQuota.TotalAvailable / unit
		data["source"] = "user_quota"
		if payload.Data.UserQuota.TotalUsed != nil && finiteNonNegative(*payload.Data.UserQuota.TotalUsed) {
			data["used_usd"] = *payload.Data.UserQuota.TotalUsed / unit
			data["total_usd"] = (*payload.Data.UserQuota.TotalAvailable + *payload.Data.UserQuota.TotalUsed) / unit
		}
		return data, true
	}
	if payload.Data.TotalAvailable == nil || !finiteNonNegative(*payload.Data.TotalAvailable) {
		return nil, false
	}
	if payload.Data.UnlimitedQuota {
		// 令牌本身不限额度且站点没给用户额度 → 没有可展示的余额。
		return data, true
	}
	data["remaining_usd"] = *payload.Data.TotalAvailable / unit
	data["source"] = "token_quota"
	if payload.Data.TotalUsed != nil && finiteNonNegative(*payload.Data.TotalUsed) {
		data["used_usd"] = *payload.Data.TotalUsed / unit
	}
	if payload.Data.TotalGranted != nil && finiteNonNegative(*payload.Data.TotalGranted) && *payload.Data.TotalGranted > 0 {
		data["total_usd"] = *payload.Data.TotalGranted / unit
	}
	return data, true
}

// upstreamBalanceDefaultQuotaPerUnit mirrors new-api's common.QuotaPerUnit
// default (500000 quota = 1 USD).
const upstreamBalanceDefaultQuotaPerUnit = 500000.0

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
