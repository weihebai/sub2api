package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// upstreamBalanceRepo is the minimal repository surface the balance probe
// needs: account loading plus the CAS snapshot write.
type upstreamBalanceRepo struct {
	AccountRepository
	mu        sync.Mutex
	accounts  map[int64]*Account
	snapshots map[int64]*UpstreamBalanceProbeSnapshot
	writeErr  error
	writes    int
}

func (r *upstreamBalanceRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return nil, ErrAccountNotFound
	}
	clone := *account
	clone.Credentials = mergeMap(nil, account.Credentials)
	clone.Extra = mergeMap(nil, account.Extra)
	return &clone, nil
}

func (r *upstreamBalanceRepo) UpdateUpstreamBalanceProbeSnapshot(_ context.Context, account *Account, snapshot *UpstreamBalanceProbeSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.snapshots == nil {
		r.snapshots = make(map[int64]*UpstreamBalanceProbeSnapshot)
	}
	r.snapshots[account.ID] = snapshot
	r.writes++
	return nil
}

type upstreamBalanceSettingsRepo struct {
	SettingRepository
	values map[string]string
}

func (r *upstreamBalanceSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (r *upstreamBalanceSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

func newUpstreamBalanceTestService(repo AccountRepository, upstream *httpUpstreamRecorder, settings SettingRepository) *UpstreamBillingProbeService {
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		Enabled:           false,
		AllowInsecureHTTP: true,
	}}}
	accountTestService := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: cfg}
	svc := NewUpstreamBillingProbeService(repo, accountTestService, NewSettingService(settings, cfg))
	svc.now = func() time.Time { return time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC) }
	return svc
}

func balanceProbeAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "relay",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"api_key": "sk-upstream", "base_url": "https://relay.example.com"},
	}
}

func balanceResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestProbeUpstreamBalanceReadsSub2APIUsageRemaining(t *testing.T) {
	accountID := int64(301)
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: balanceProbeAccount(accountID)}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		balanceResponse(http.StatusOK, `{"mode":"quota_limited","remaining":12.5,"quota":{"limit":20,"used":7.5,"remaining":12.5}}`),
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.Equal(t, 12.5, snapshot.Data["remaining_usd"])
	require.Equal(t, UpstreamBalanceKindSub2API, snapshot.Data["kind"])
	require.Equal(t, "quota_limited", snapshot.Data["mode"])
	require.Equal(t, 20.0, snapshot.Data["quota_limit_usd"])
	require.Equal(t, 1, repo.writes)

	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://relay.example.com/v1/usage", upstream.requests[0].URL.String())
	require.Equal(t, "Bearer sk-upstream", upstream.requests[0].Header.Get("Authorization"))
}

func TestProbeUpstreamBalanceFallsBackToNewAPIBilling(t *testing.T) {
	accountID := int64(302)
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: balanceProbeAccount(accountID)}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		// /v1/usage 不存在 → 继续探测 new-api 方言。
		balanceResponse(http.StatusNotFound, `{"error":{"message":"not found"}}`),
		// 令牌配额端点也不存在 → 落到 OpenAI 兼容 billing 方言。
		balanceResponse(http.StatusNotFound, `{"error":{"message":"not found"}}`),
		balanceResponse(http.StatusNotFound, `{"error":{"message":"not found"}}`),
		balanceResponse(http.StatusOK, `{"object":"billing_subscription","hard_limit_usd":100,"soft_limit_usd":90}`),
		balanceResponse(http.StatusOK, `{"object":"list","total_usage":37.25}`),
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.Equal(t, UpstreamBalanceKindOpenAIBill, snapshot.Data["kind"])
	require.Equal(t, 62.75, snapshot.Data["remaining_usd"])
	require.Equal(t, 100.0, snapshot.Data["total_usd"])
	require.Equal(t, 37.25, snapshot.Data["used_usd"])

	require.Len(t, upstream.requests, 5)
	require.Equal(t, "/v1/dashboard/billing/subscription", upstream.requests[3].URL.Path)
	require.Equal(t, "relay.example.com", upstream.requests[3].URL.Host)
	require.Equal(t, "/v1/dashboard/billing/usage", upstream.requests[4].URL.Path)
	require.Equal(t, "relay.example.com", upstream.requests[4].URL.Host)
	require.Equal(t, "2025-05-21", upstream.requests[4].URL.Query().Get("start_date"))
	require.Equal(t, "2026-05-21", upstream.requests[4].URL.Query().Get("end_date"))
}

// new-api 是唯一能用裸 API Key 拿到「令牌真实剩余」的方言：/api/usage/token
// 返回的数值是 quota 单位（默认 500000 = 1 USD），必须换算成美元。
func TestProbeUpstreamBalancePrefersNewAPITokenQuota(t *testing.T) {
	accountID := int64(311)
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: balanceProbeAccount(accountID)}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		balanceResponse(http.StatusNotFound, ``),
		balanceResponse(http.StatusOK, `{"code":true,"message":"","data":{
			"object":"token_usage","name":"my-token",
			"total_granted":2500000,"total_used":2500000,"total_available":107550,
			"unlimited_quota":false,
			"user_quota":{"total_available":107550,"total_used":20976980}}}`),
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.Equal(t, UpstreamBalanceKindNewAPIToken, snapshot.Data["kind"])
	require.InDelta(t, 0.21510, snapshot.Data["remaining_usd"], 1e-9)
	require.InDelta(t, 41.95396, snapshot.Data["used_usd"], 1e-9)
	require.Equal(t, "user_quota", snapshot.Data["source"])
	// 令牌配额端点成功就不该再去敲 billing 端点。
	require.Len(t, upstream.requests, 2)
}

func TestParseUpstreamBalanceTokenUsageConvertsQuotaUnits(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"code":true,"data":{"total_available":1500000,"total_used":500000,"total_granted":2000000,"unlimited_quota":false}}`)
	data, ok := parseUpstreamBalanceTokenUsage(body, 500000, now)
	require.True(t, ok)
	require.InDelta(t, 3.0, data["remaining_usd"], 1e-9)
	require.InDelta(t, 1.0, data["used_usd"], 1e-9)
	require.InDelta(t, 4.0, data["total_usd"], 1e-9)
	require.Equal(t, "token_quota", data["source"])
}

func TestParseUpstreamBalanceTokenUsageRejectsErrorEnvelope(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	_, ok := parseUpstreamBalanceTokenUsage([]byte(`{"error":{"message":"unauthorized"}}`), 500000, now)
	require.False(t, ok)
	_, ok = parseUpstreamBalanceTokenUsage([]byte(`{"code":false,"message":"token not found"}`), 500000, now)
	require.False(t, ok)
}

// /api/user/self 是唯一能拿到站点钱包真实余额的端点，但必须配访问令牌；
// quota / used_quota 都是 new-api 的 quota 单位。
func TestParseUpstreamBalanceNewAPIUserSelf(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"success":true,"data":{"id":1,"username":"demo","quota":107550,"used_quota":20976980}}`)
	data, ok := parseUpstreamBalanceNewAPIUserSelf(body, now)
	require.True(t, ok)
	require.Equal(t, UpstreamBalanceKindNewAPIUser, data["kind"])
	require.InDelta(t, 0.2151, data["remaining_usd"], 1e-9)
	require.InDelta(t, 41.95396, data["used_usd"], 1e-9)
}

// 欠费站点的余额是负数，不能截断成 0。
func TestParseUpstreamBalanceNewAPIUserSelfKeepsNegativeBalance(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"success":true,"data":{"quota":-4750,"used_quota":24985000}}`)
	data, ok := parseUpstreamBalanceNewAPIUserSelf(body, now)
	require.True(t, ok)
	require.InDelta(t, -0.0095, data["remaining_usd"], 1e-9)
}

func TestParseUpstreamBalanceNewAPIUserSelfRejectsFailure(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	_, ok := parseUpstreamBalanceNewAPIUserSelf([]byte(`{"success":false,"message":"Unauthorized, invalid access token"}`), now)
	require.False(t, ok)
	_, ok = parseUpstreamBalanceNewAPIUserSelf([]byte(`{"success":true,"data":{}}`), now)
	require.False(t, ok)
}

// 配了访问令牌时必须优先走 /api/user/self，而不是继续敲注定 401 的公开端点。
func TestProbeUpstreamBalancePrefersAccessToken(t *testing.T) {
	accountID := int64(321)
	account := balanceProbeAccount(accountID)
	account.Credentials[NewAPIAccessTokenCredentialKey] = "user-access-token"
	account.Extra = map[string]any{NewAPIAccessTokenUserIDExtraKey: "1"}
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: account}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		balanceResponse(http.StatusOK, `{"success":true,"data":{"id":1,"quota":107550,"used_quota":20976980}}`),
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
	require.Equal(t, UpstreamBalanceKindNewAPIUser, snapshot.Data["kind"])
	require.InDelta(t, 0.2151, snapshot.Data["remaining_usd"], 1e-9)

	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/api/user/self", upstream.requests[0].URL.Path)
	require.Equal(t, "Bearer user-access-token", upstream.requests[0].Header.Get("Authorization"))
	require.Equal(t, "1", upstream.requests[0].Header.Get("New-API-User"))
}

// 访问令牌被拒时不应再回退到公开端点，而是明确记为令牌失效。
func TestProbeUpstreamBalanceReportsRejectedAccessToken(t *testing.T) {
	accountID := int64(322)
	account := balanceProbeAccount(accountID)
	account.Credentials[NewAPIAccessTokenCredentialKey] = "stale-token"
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: account}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		balanceResponse(http.StatusUnauthorized, `{"success":false,"message":"Unauthorized"}`),
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusFailed, snapshot.Status)
	require.Equal(t, "access_token_rejected", snapshot.LastError)
	require.Len(t, upstream.requests, 1)
}

// new-api 对不限额度的令牌把 hard_limit_usd 直接写成 100000000；此时绝不能
// 把它当成"还剩一亿美元"展示。total_usage 的单位是站点自己的计费单位而非
// 美元（实测两站比值分别为 100 和 82.9，无法用站点配置统一解释），所以只保留
// 原始数字，不给它套美元符号。
func TestParseUpstreamBalanceOpenAIBillingFlagsSentinelUnlimited(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	data, err := parseUpstreamBalanceOpenAIBilling(
		[]byte(`{"hard_limit_usd":100000000}`),
		[]byte(`{"total_usage":40.5428}`),
		now,
	)
	require.NoError(t, err)
	require.Equal(t, true, data["unlimited"])
	require.NotContains(t, data, "remaining_usd")
	require.NotContains(t, data, "total_usd")
	require.NotContains(t, data, "used_usd")
	require.Equal(t, 40.5428, data["upstream_used_raw"])
}

func TestParseUpstreamBalanceOpenAIBillingNormalizesLegacyCents(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	// one-api 老实现把 total_usage 乘以 100（美分）；用量不可能超过额度，
	// 因此超过即按美分折算。
	data, err := parseUpstreamBalanceOpenAIBilling(
		[]byte(`{"hard_limit_usd":50}`),
		[]byte(`{"total_usage":1250}`),
		now,
	)
	require.NoError(t, err)
	require.Equal(t, 37.5, data["remaining_usd"])
	require.Equal(t, 12.5, data["used_usd"])
}

func TestParseUpstreamBalanceOpenAIBillingRejectsErrorBodies(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	_, err := parseUpstreamBalanceOpenAIBilling(
		[]byte(`{"error":{"message":"invalid token","type":"new_api_error"}}`),
		[]byte(`{"total_usage":0}`),
		now,
	)
	require.Error(t, err)

	_, err = parseUpstreamBalanceOpenAIBilling(
		[]byte(`{"hard_limit_usd":10}`),
		[]byte(`{"total_usage":0}`),
		now,
	)
	require.NoError(t, err)
}

func TestParseUpstreamBalanceSub2APIUsageAcceptsWalletBalance(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	data, ok := parseUpstreamBalanceSub2APIUsage([]byte(`{"mode":"unrestricted","balance":8.25}`), now)
	require.True(t, ok)
	require.Equal(t, 8.25, data["remaining_usd"])

	_, ok = parseUpstreamBalanceSub2APIUsage([]byte(`{"mode":"unrestricted"}`), now)
	require.False(t, ok)

	_, ok = parseUpstreamBalanceSub2APIUsage([]byte(`{"remaining":-1}`), now)
	require.False(t, ok)
}

func TestProbeUpstreamBalanceRecordsUnsupportedWhenNeitherDialectAnswers(t *testing.T) {
	accountID := int64(303)
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: balanceProbeAccount(accountID)}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		balanceResponse(http.StatusNotFound, ``), // /v1/usage
		balanceResponse(http.StatusNotFound, ``), // /api/usage/token
		balanceResponse(http.StatusNotFound, ``), // /v1/usage/token
		balanceResponse(http.StatusNotFound, ``), // billing/subscription
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusUnsupported, snapshot.Status)
	require.Equal(t, "unsupported", snapshot.LastError)
	require.Equal(t, 1, repo.writes)
}

func TestProbeUpstreamBalanceSkipsOfficialDomains(t *testing.T) {
	accountID := int64(304)
	account := balanceProbeAccount(accountID)
	account.Credentials = map[string]any{"api_key": "sk-upstream", "base_url": "https://api.openai.com"}
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: account}}
	upstream := &httpUpstreamRecorder{}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusUnsupported, snapshot.Status)
	require.Empty(t, upstream.requests, "official API domains must not be probed")
}

func TestProbeUpstreamBalanceFailurePreservesLastKnownBalance(t *testing.T) {
	accountID := int64(305)
	account := balanceProbeAccount(accountID)
	account.Extra = map[string]any{
		UpstreamBalanceProbeExtraKey: &UpstreamBalanceProbeSnapshot{
			Status:        UpstreamBillingProbeStatusOK,
			Data:          map[string]any{"kind": UpstreamBalanceKindSub2API, "remaining_usd": 9.5},
			ReceivedAt:    probeTimePtr(time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)),
			LastAttemptAt: time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC),
			NextProbeAt:   time.Date(2026, 5, 20, 9, 30, 0, 0, time.UTC),
		},
	}
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: account}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		balanceResponse(http.StatusInternalServerError, `oops`),
	}}
	svc := newUpstreamBalanceTestService(repo, upstream, &upstreamBalanceSettingsRepo{})

	snapshot, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.Equal(t, UpstreamBillingProbeStatusFailed, snapshot.Status)
	require.Equal(t, "http_error", snapshot.LastError)
	require.Equal(t, 500, snapshot.HTTPStatus)
	require.Equal(t, 9.5, snapshot.Data["remaining_usd"], "last known balance must survive a failed probe")
	require.NotNil(t, snapshot.ReceivedAt)
	require.Equal(t, 1, snapshot.FailureCount)
}

func TestProbeUpstreamBalanceRequiresAPIKeyIdentity(t *testing.T) {
	accountID := int64(306)
	account := balanceProbeAccount(accountID)
	account.Type = AccountTypeOAuth
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{accountID: account}}
	svc := newUpstreamBalanceTestService(repo, &httpUpstreamRecorder{}, &upstreamBalanceSettingsRepo{})

	_, err := svc.ProbeUpstreamBalanceAccount(context.Background(), accountID)
	require.ErrorIs(t, err, ErrUpstreamBillingProbeAccountInvalid)
}

// balanceByAPIKeyStub answers per account (by Authorization header) so batch
// concurrency cannot reorder the responses under the test's feet.
type balanceByAPIKeyStub struct {
	mu        sync.Mutex
	responses map[string][]*http.Response
	calls     int
}

func (u *balanceByAPIKeyStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *balanceByAPIKeyStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	key := req.Header.Get("Authorization")
	queue := u.responses[key]
	var resp *http.Response
	if len(queue) > 0 {
		resp = queue[0]
		u.responses[key] = queue[1:]
	}
	u.mu.Unlock()
	if resp != nil {
		return resp, nil
	}
	return balanceResponse(http.StatusNotFound, ``), nil
}

func TestProbeUpstreamBalanceBatchReturnsPerAccountResults(t *testing.T) {
	okID := int64(307)
	badID := int64(308)
	okAccount := balanceProbeAccount(okID)
	okAccount.Credentials["api_key"] = "sk-ok"
	badAccount := balanceProbeAccount(badID)
	badAccount.Credentials["api_key"] = "sk-bad"
	repo := &upstreamBalanceRepo{accounts: map[int64]*Account{
		okID:  okAccount,
		badID: badAccount,
	}}
	upstream := &balanceByAPIKeyStub{
		responses: map[string][]*http.Response{
			"Bearer sk-ok":  {balanceResponse(http.StatusOK, `{"remaining":3.25}`)},
			"Bearer sk-bad": {balanceResponse(http.StatusUnauthorized, `{"error":"bad key"}`)},
		},
	}
	svc := newUpstreamBalanceTestService(repo, &httpUpstreamRecorder{}, &upstreamBalanceSettingsRepo{})
	svc.accountTestService.httpUpstream = upstream

	results := svc.ProbeUpstreamBalanceAccounts(context.Background(), []int64{okID, badID})
	require.Len(t, results, 2)
	require.Equal(t, okID, results[0].AccountID)
	require.NotNil(t, results[0].Snapshot)
	require.Equal(t, 3.25, results[0].Snapshot.Data["remaining_usd"])
	require.Equal(t, badID, results[1].AccountID)
	require.NotNil(t, results[1].Snapshot)
	require.Equal(t, UpstreamBillingProbeStatusFailed, results[1].Snapshot.Status)
	require.Empty(t, results[1].Error, "an upstream HTTP error is a recorded failure, not a transport error")
}

func TestDecodeUpstreamBalanceProbeSnapshotIgnoresUnknownStatus(t *testing.T) {
	require.Nil(t, decodeUpstreamBalanceProbeSnapshot(nil))
	require.Nil(t, decodeUpstreamBalanceProbeSnapshot(map[string]any{}))
	require.Nil(t, decodeUpstreamBalanceProbeSnapshot(map[string]any{
		UpstreamBalanceProbeExtraKey: map[string]any{"status": "bogus"},
	}))
	snapshot := decodeUpstreamBalanceProbeSnapshot(map[string]any{
		UpstreamBalanceProbeExtraKey: map[string]any{"status": "ok", "last_attempt_at": "2026-05-20T10:00:00Z", "next_probe_at": "2026-05-20T10:30:00Z"},
	})
	require.NotNil(t, snapshot)
	require.Equal(t, UpstreamBillingProbeStatusOK, snapshot.Status)
}
