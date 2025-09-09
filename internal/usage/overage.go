// internal/usage/overage.go
package usage

import (
	"context"
	"fmt"
	"time"
)

type OverageStatus string

const (
	OverageStatusOK       OverageStatus = "OK"
	OverageStatusWarning  OverageStatus = "WARNING"
	OverageStatusCritical OverageStatus = "CRITICAL"
	OverageStatusBlocked  OverageStatus = "BLOCKED"
)

type OveragePolicy string

const (
	OveragePolicyHardLimit OveragePolicy = "hard_limit"
	OveragePolicySoftLimit OveragePolicy = "soft_limit"
	OveragePolicyBilling   OveragePolicy = "billing"
)

type OverageInfo struct {
	TenantID        string        `json:"tenant_id"`
	Status          OverageStatus `json:"status"`
	UsagePercent    float64       `json:"usage_percent"`
	BytesOver       int64         `json:"bytes_over,omitempty"`
	GraceBytesLeft  int64         `json:"grace_bytes_left,omitempty"`
	EstimatedCharge float64       `json:"estimated_charge,omitempty"`
	BlockedSince    *time.Time    `json:"blocked_since,omitempty"`
}

type OverageAction struct {
	Allowed       bool    `json:"allowed"`
	Reason        string  `json:"reason"`
	OverageCharge float64 `json:"overage_charge,omitempty"`
	GraceUsed     int64   `json:"grace_used,omitempty"`
}

type OverageHandler struct {
	quotaManager *QuotaManager
	policies     map[string]OveragePolicy
	gracePercent float64
	overageRate  float64 // $ per GB over limit
}

func NewOverageHandler(qm *QuotaManager) *OverageHandler {
	return &OverageHandler{
		quotaManager: qm,
		policies:     make(map[string]OveragePolicy),
		gracePercent: 0.1,  // 10% grace by default
		overageRate:  0.01, // $0.01 per GB overage
	}
}

func (oh *OverageHandler) SetPolicy(tenantID string, policy OveragePolicy) {
	oh.policies[tenantID] = policy
}

func (oh *OverageHandler) GetPolicy(tenantID string) OveragePolicy {
	if policy, exists := oh.policies[tenantID]; exists {
		return policy
	}
	return OveragePolicyHardLimit // Default to hard limit
}

func (oh *OverageHandler) CheckOverage(ctx context.Context, tenantID string) (*OverageInfo, error) {
	used, limit, err := oh.quotaManager.GetUsage(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("checking overage: %w", err)
	}

	percent := float64(used) / float64(limit) * 100
	info := &OverageInfo{
		TenantID:     tenantID,
		UsagePercent: percent,
	}

	if used <= limit {
		info.Status = OverageStatusOK
		if percent >= 90 {
			info.Status = OverageStatusWarning
		}
	} else {
		bytesOver := used - limit
		info.BytesOver = bytesOver
		info.Status = OverageStatusCritical

		policy := oh.GetPolicy(tenantID)

		switch policy {
		case OveragePolicyHardLimit:
			info.Status = OverageStatusBlocked
			now := time.Now()
			info.BlockedSince = &now

		case OveragePolicySoftLimit:
			graceBytes := int64(float64(limit) * oh.gracePercent)
			if bytesOver <= graceBytes {
				info.GraceBytesLeft = graceBytes - bytesOver
			} else {
				info.Status = OverageStatusBlocked
			}

		case OveragePolicyBilling:
			gbOver := float64(bytesOver) / 1073741824
			info.EstimatedCharge = gbOver * oh.overageRate
		}
	}

	return info, nil
}

func (oh *OverageHandler) HandleOverage(ctx context.Context, tenantID string, requestedBytes int64) (*OverageAction, error) {
	used, limit, err := oh.quotaManager.GetUsage(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("handling overage: %w", err)
	}

	newUsage := used + requestedBytes
	policy := oh.GetPolicy(tenantID)

	action := &OverageAction{}

	if newUsage <= limit {
		action.Allowed = true
		action.Reason = "within quota"
		return action, nil
	}

	// Would exceed limit
	bytesOver := newUsage - limit

	switch policy {
	case OveragePolicyHardLimit:
		action.Allowed = false
		action.Reason = fmt.Sprintf("would exceed quota by %d bytes", bytesOver)

	case OveragePolicySoftLimit:
		graceBytes := int64(float64(limit) * oh.gracePercent)
		if bytesOver <= graceBytes {
			action.Allowed = true
			action.Reason = "using grace period"
			action.GraceUsed = bytesOver
		} else {
			action.Allowed = false
			action.Reason = fmt.Sprintf("would exceed grace limit by %d bytes", bytesOver-graceBytes)
		}

	case OveragePolicyBilling:
		action.Allowed = true
		gbOver := float64(bytesOver) / 1073741824
		action.OverageCharge = gbOver * oh.overageRate
		action.Reason = fmt.Sprintf("overage will be billed at $%.4f", action.OverageCharge)
	}

	return action, nil
}

func (oh *OverageHandler) RecordOverageEvent(ctx context.Context, tenantID string, bytes int64, charged float64) error {
	// Record overage event for billing
	query := `
        INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key, timestamp)
        VALUES ($1, 'OVERAGE', $2, $3, NOW())`

	_, err := oh.quotaManager.db.ExecContext(ctx, query, tenantID, bytes, fmt.Sprintf("charge:%.4f", charged))
	return err
}
