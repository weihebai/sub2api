package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ProbeUpstreamBalance refreshes one account's upstream balance snapshot.
// POST /api/v1/admin/accounts/:id/upstream-balance-probe
func (h *AccountHandler) ProbeUpstreamBalance(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	snapshot, err := h.upstreamBillingProbe.ProbeUpstreamBalanceAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, service.UpstreamBalanceProbeResult{AccountID: accountID, Snapshot: snapshot})
}

// ProbeUpstreamBalanceBatch refreshes a bounded selection of accounts.
// POST /api/v1/admin/accounts/upstream-balance-probe/batch
func (h *AccountHandler) ProbeUpstreamBalanceBatch(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req upstreamBillingProbeBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.AccountIDs) == 0 || len(req.AccountIDs) > service.UpstreamBillingProbeMaxBatchSize {
		response.BadRequest(c, "account_ids must contain between 1 and 20 items")
		return
	}
	seen := make(map[int64]struct{}, len(req.AccountIDs))
	accountIDs := make([]int64, 0, len(req.AccountIDs))
	for _, accountID := range req.AccountIDs {
		if accountID <= 0 {
			response.BadRequest(c, "account_ids must contain positive IDs")
			return
		}
		if _, exists := seen[accountID]; exists {
			continue
		}
		seen[accountID] = struct{}{}
		accountIDs = append(accountIDs, accountID)
	}
	response.Success(c, gin.H{"results": h.upstreamBillingProbe.ProbeUpstreamBalanceAccounts(c.Request.Context(), accountIDs)})
}

// GetUpstreamBalanceProbeSettings returns the global balance runner settings.
func (h *AccountHandler) GetUpstreamBalanceProbeSettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	settings, err := h.upstreamBillingProbe.GetUpstreamBalanceProbeSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateUpstreamBalanceProbeSettings persists the global balance runner settings.
func (h *AccountHandler) UpdateUpstreamBalanceProbeSettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req service.UpstreamBalanceProbeSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.UpdateUpstreamBalanceProbeSettings(c.Request.Context(), &req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	settings, err := h.upstreamBillingProbe.GetUpstreamBalanceProbeSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// SetUpstreamBalanceProbeEnabled toggles the per-account auto refresh opt-in.
// PUT /api/v1/admin/accounts/:id/upstream-balance-probe
func (h *AccountHandler) SetUpstreamBalanceProbeEnabled(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req upstreamBillingProbeEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.SetUpstreamBalanceProbeEnabled(c.Request.Context(), accountID, *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_id": accountID, "enabled": *req.Enabled})
}
