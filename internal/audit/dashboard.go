package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OverviewStats holds high-level statistics
type OverviewStats struct {
	TotalEvents    int64
	EventsToday    int64
	FailedEvents   int64
	CriticalEvents int64
}

// TimeSeriesPoint represents a data point in time series
type TimeSeriesPoint struct {
	Timestamp time.Time
	Count     int64
}

// UserActivity represents user activity metrics
type UserActivity struct {
	UserID     uuid.UUID
	EventCount int64
}

// ResourceActivity represents resource access metrics
type ResourceActivity struct {
	Resource    string
	AccessCount int64
}

// EventDistribution represents event type distribution
type EventDistribution struct {
	EventType  EventType
	Count      int64
	Percentage float64
}

// HourlyActivity represents activity by hour of day
type HourlyActivity struct {
	Hour  int
	Count int64
}

// FailureRatePoint represents failure rate at a point in time
type FailureRatePoint struct {
	Timestamp    time.Time
	TotalEvents  int64
	FailedEvents int64
	FailureRate  float64
}

// GetOverviewStats returns high-level dashboard statistics
func (s *AuditService) GetOverviewStats(ctx context.Context) (*OverviewStats, error) {
	stats := &OverviewStats{}
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// Total events
	query := `SELECT COUNT(*) FROM audit_logs`
	if err := s.db.QueryRowContext(ctx, query).Scan(&stats.TotalEvents); err != nil {
		return nil, fmt.Errorf("count total events: %w", err)
	}

	// Events today
	query = `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1`
	if err := s.db.QueryRowContext(ctx, query, startOfDay).Scan(&stats.EventsToday); err != nil {
		return nil, fmt.Errorf("count events today: %w", err)
	}

	// Failed events
	query = `SELECT COUNT(*) FROM audit_logs WHERE result = $1`
	if err := s.db.QueryRowContext(ctx, query, ResultFailure).Scan(&stats.FailedEvents); err != nil {
		return nil, fmt.Errorf("count failed events: %w", err)
	}

	// Critical events
	query = `SELECT COUNT(*) FROM audit_logs WHERE severity = $1`
	if err := s.db.QueryRowContext(ctx, query, SeverityCritical).Scan(&stats.CriticalEvents); err != nil {
		return nil, fmt.Errorf("count critical events: %w", err)
	}

	return stats, nil
}

// GetTimeSeriesData returns event counts over time
func (s *AuditService) GetTimeSeriesData(ctx context.Context, start, end time.Time, interval time.Duration) ([]TimeSeriesPoint, error) {
	// Convert interval to PostgreSQL interval string
	intervalStr := fmt.Sprintf("%d seconds", int(interval.Seconds()))

	query := `
		SELECT
			date_trunc('hour', timestamp) as bucket,
			COUNT(*) as count
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2
		GROUP BY bucket
		ORDER BY bucket
	`

	rows, err := s.db.QueryContext(ctx, query, start, end)
	if err != nil {
		return nil, fmt.Errorf("query time series: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var points []TimeSeriesPoint
	for rows.Next() {
		var point TimeSeriesPoint
		if err := rows.Scan(&point.Timestamp, &point.Count); err != nil {
			return nil, fmt.Errorf("scan time series point: %w", err)
		}
		points = append(points, point)
	}

	_ = intervalStr // Use if needed for custom intervals

	return points, rows.Err()
}

// GetTopUsers returns most active users
func (s *AuditService) GetTopUsers(ctx context.Context, start, end time.Time, limit int) ([]UserActivity, error) {
	query := `
		SELECT user_id, COUNT(*) as event_count
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2 AND user_id IS NOT NULL
		GROUP BY user_id
		ORDER BY event_count DESC
		LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, query, start, end, limit)
	if err != nil {
		return nil, fmt.Errorf("query top users: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var users []UserActivity
	for rows.Next() {
		var user UserActivity
		var userIDStr string
		if err := rows.Scan(&userIDStr, &user.EventCount); err != nil {
			return nil, fmt.Errorf("scan user activity: %w", err)
		}
		user.UserID, _ = uuid.Parse(userIDStr)
		users = append(users, user)
	}

	return users, rows.Err()
}

// GetTopResources returns most accessed resources
func (s *AuditService) GetTopResources(ctx context.Context, start, end time.Time, limit int) ([]ResourceActivity, error) {
	query := `
		SELECT resource, COUNT(*) as access_count
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2 AND resource IS NOT NULL
		GROUP BY resource
		ORDER BY access_count DESC
		LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, query, start, end, limit)
	if err != nil {
		return nil, fmt.Errorf("query top resources: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var resources []ResourceActivity
	for rows.Next() {
		var res ResourceActivity
		if err := rows.Scan(&res.Resource, &res.AccessCount); err != nil {
			return nil, fmt.Errorf("scan resource activity: %w", err)
		}
		resources = append(resources, res)
	}

	return resources, rows.Err()
}

// GetEventDistribution returns distribution of event types
func (s *AuditService) GetEventDistribution(ctx context.Context, start, end time.Time) ([]EventDistribution, error) {
	query := `
		SELECT event_type, COUNT(*) as count
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2
		GROUP BY event_type
		ORDER BY count DESC
	`

	rows, err := s.db.QueryContext(ctx, query, start, end)
	if err != nil {
		return nil, fmt.Errorf("query event distribution: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var distribution []EventDistribution
	var totalCount int64

	for rows.Next() {
		var dist EventDistribution
		if err := rows.Scan(&dist.EventType, &dist.Count); err != nil {
			return nil, fmt.Errorf("scan event distribution: %w", err)
		}
		totalCount += dist.Count
		distribution = append(distribution, dist)
	}

	// Calculate percentages
	for i := range distribution {
		if totalCount > 0 {
			distribution[i].Percentage = float64(distribution[i].Count) / float64(totalCount) * 100
		}
	}

	return distribution, rows.Err()
}

// GetRecentEvents returns the most recent events
func (s *AuditService) GetRecentEvents(ctx context.Context, limit int) ([]*AuditEvent, error) {
	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		ORDER BY timestamp DESC
		LIMIT $1
	`

	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent events: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// GetActivityHeatmap returns activity counts by hour of day
func (s *AuditService) GetActivityHeatmap(ctx context.Context, start, end time.Time) ([]HourlyActivity, error) {
	query := `
		SELECT EXTRACT(HOUR FROM timestamp) as hour, COUNT(*) as count
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2
		GROUP BY hour
		ORDER BY hour
	`

	rows, err := s.db.QueryContext(ctx, query, start, end)
	if err != nil {
		return nil, fmt.Errorf("query activity heatmap: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var heatmap []HourlyActivity
	for rows.Next() {
		var activity HourlyActivity
		var hourFloat float64
		if err := rows.Scan(&hourFloat, &activity.Count); err != nil {
			return nil, fmt.Errorf("scan hourly activity: %w", err)
		}
		activity.Hour = int(hourFloat)
		heatmap = append(heatmap, activity)
	}

	return heatmap, rows.Err()
}

// GetFailureRateTrends returns failure rate trends over time
func (s *AuditService) GetFailureRateTrends(ctx context.Context, start, end time.Time, interval time.Duration) ([]FailureRatePoint, error) {
	query := `
		SELECT
			date_trunc('hour', timestamp) as bucket,
			COUNT(*) as total,
			COUNT(*) FILTER (WHERE result = $3) as failed
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2
		GROUP BY bucket
		ORDER BY bucket
	`

	rows, err := s.db.QueryContext(ctx, query, start, end, ResultFailure)
	if err != nil {
		return nil, fmt.Errorf("query failure rate trends: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var trends []FailureRatePoint
	for rows.Next() {
		var point FailureRatePoint
		if err := rows.Scan(&point.Timestamp, &point.TotalEvents, &point.FailedEvents); err != nil {
			return nil, fmt.Errorf("scan failure rate point: %w", err)
		}
		if point.TotalEvents > 0 {
			point.FailureRate = float64(point.FailedEvents) / float64(point.TotalEvents) * 100
		}
		trends = append(trends, point)
	}

	return trends, rows.Err()
}
