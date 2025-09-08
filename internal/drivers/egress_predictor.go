package drivers

import (
	"sync"
	"time"
)

// AlertLevel represents severity of usage alert
type AlertLevel int

const (
	AlertNone AlertLevel = iota
	AlertInfo
	AlertWarning
	AlertCritical
)

// UsageAlert contains alert information
type UsageAlert struct {
	Level          AlertLevel
	Message        string
	CurrentUsage   int64
	PredictedUsage int64
	Quota          int64
	PercentUsed    float64
}

// EgressPredictor predicts bandwidth usage and generates alerts
type EgressPredictor struct {
	mu         sync.RWMutex
	dailyUsage map[string]map[string]int64 // tenant -> date -> bytes
	totalUsage map[string]int64            // tenant -> total bytes this month
	quotas     map[string]int64            // tenant -> quota bytes
	lastUpdate map[string]time.Time        // tenant -> last update time
}

// NewEgressPredictor creates a new egress predictor
func NewEgressPredictor() *EgressPredictor {
	return &EgressPredictor{
		dailyUsage: make(map[string]map[string]int64),
		totalUsage: make(map[string]int64),
		quotas:     make(map[string]int64),
		lastUpdate: make(map[string]time.Time),
	}
}

// RecordUsage records cumulative usage for prediction
func (ep *EgressPredictor) RecordUsage(tenantID string, totalBytes int64, at time.Time) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.totalUsage[tenantID] = totalBytes
	ep.lastUpdate[tenantID] = at

	// Record daily snapshot
	dateKey := at.Format("2006-01-02")
	if ep.dailyUsage[tenantID] == nil {
		ep.dailyUsage[tenantID] = make(map[string]int64)
	}
	ep.dailyUsage[tenantID][dateKey] = totalBytes
}

// RecordDailyUsage records usage for a specific day
func (ep *EgressPredictor) RecordDailyUsage(tenantID string, bytes int64, date time.Time) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	dateKey := date.Format("2006-01-02")
	if ep.dailyUsage[tenantID] == nil {
		ep.dailyUsage[tenantID] = make(map[string]int64)
	}
	ep.dailyUsage[tenantID][dateKey] = bytes
}

// PredictMonthlyUsage predicts total usage for the month
func (ep *EgressPredictor) PredictMonthlyUsage(tenantID string, now time.Time) int64 {
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	currentUsage := ep.totalUsage[tenantID]
	if currentUsage == 0 {
		return 0
	}

	// Calculate days elapsed and days in month
	dayOfMonth := now.Day()
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()

	// Simple linear projection
	dailyRate := currentUsage / int64(dayOfMonth)
	predicted := dailyRate * int64(daysInMonth)

	return predicted
}

// GetAverageDailyUsage calculates average daily usage
func (ep *EgressPredictor) GetAverageDailyUsage(tenantID string) int64 {
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	usage := ep.dailyUsage[tenantID]
	if len(usage) == 0 {
		return 0
	}

	var total int64
	for _, bytes := range usage {
		total += bytes
	}

	return total / int64(len(usage))
}

// SetQuota sets the quota for alerts
func (ep *EgressPredictor) SetQuota(tenantID string, bytes int64) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.quotas[tenantID] = bytes
}

// CheckAlert checks if an alert should be generated
func (ep *EgressPredictor) CheckAlert(tenantID string, currentUsage int64) UsageAlert {
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	quota := ep.quotas[tenantID]
	if quota == 0 {
		return UsageAlert{Level: AlertNone}
	}

	percentUsed := float64(currentUsage) / float64(quota) * 100

	alert := UsageAlert{
		CurrentUsage: currentUsage,
		Quota:        quota,
		PercentUsed:  percentUsed,
	}

	switch {
	case percentUsed >= 90:
		alert.Level = AlertCritical
		alert.Message = "Critical: Egress usage at 90% of quota"
	case percentUsed >= 75:
		alert.Level = AlertWarning
		alert.Message = "Warning: Egress usage at 75% of quota"
	case percentUsed >= 50:
		alert.Level = AlertInfo
		alert.Message = "Info: Egress usage at 50% of quota"
	default:
		alert.Level = AlertNone
		alert.Message = "Usage within normal limits"
	}

	// Add prediction if we're tracking
	if ep.lastUpdate[tenantID].Month() == time.Now().Month() {
		alert.PredictedUsage = ep.PredictMonthlyUsage(tenantID, time.Now())
	}

	return alert
}

// GetUsageTrend returns usage trend for the tenant
func (ep *EgressPredictor) GetUsageTrend(tenantID string, days int) []int64 {
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	trend := make([]int64, 0, days)
	usage := ep.dailyUsage[tenantID]

	for i := days - 1; i >= 0; i-- {
		date := time.Now().AddDate(0, 0, -i)
		dateKey := date.Format("2006-01-02")
		if bytes, exists := usage[dateKey]; exists {
			trend = append(trend, bytes)
		} else {
			trend = append(trend, 0)
		}
	}

	return trend
}
