package rbac

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AuditLogEntry represents a single audit log entry
type AuditLogEntry struct {
	ID          uuid.UUID         `json:"id" db:"id"`
	UserID      uuid.UUID         `json:"user_id" db:"user_id"`
	PerformedBy uuid.UUID         `json:"performed_by,omitempty" db:"performed_by"`
	Timestamp   time.Time         `json:"timestamp" db:"timestamp"`
	Action      string            `json:"action" db:"action"`
	Permission  string            `json:"permission,omitempty" db:"permission"`
	Role        string            `json:"role,omitempty" db:"role"`
	Resource    string            `json:"resource,omitempty" db:"resource"`
	Granted     bool              `json:"granted" db:"granted"`
	Context     map[string]string `json:"context,omitempty" db:"context"`
	IP          string            `json:"ip,omitempty" db:"ip"`
	UserAgent   string            `json:"user_agent,omitempty" db:"user_agent"`
}

// AuditQuery defines parameters for querying audit logs
type AuditQuery struct {
	UserID      *uuid.UUID `json:"user_id,omitempty"`
	PerformedBy *uuid.UUID `json:"performed_by,omitempty"`
	Action      *string    `json:"action,omitempty"`
	Permission  *string    `json:"permission,omitempty"`
	Role        *string    `json:"role,omitempty"`
	StartTime   *time.Time `json:"start_time,omitempty"`
	EndTime     *time.Time `json:"end_time,omitempty"`
	Limit       int        `json:"limit"`
}

// PermissionStats represents statistics for a permission
type PermissionStats struct {
	Permission  string `json:"permission"`
	TotalChecks int    `json:"total_checks"`
	Granted     int    `json:"granted"`
	Denied      int    `json:"denied"`
	UniqueUsers int    `json:"unique_users"`
}

// PermissionAuditor handles audit logging for permissions
type PermissionAuditor struct {
	logs []AuditLogEntry
	mu   sync.RWMutex
}

// NewPermissionAuditor creates a new permission auditor
func NewPermissionAuditor() *PermissionAuditor {
	return &PermissionAuditor{
		logs: []AuditLogEntry{},
	}
}

// LogPermissionCheck logs a permission check
func (pa *PermissionAuditor) LogPermissionCheck(userID uuid.UUID, permission string, granted bool) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	entry := AuditLogEntry{
		ID:         uuid.New(),
		UserID:     userID,
		Timestamp:  time.Now(),
		Action:     "check",
		Permission: permission,
		Granted:    granted,
	}

	pa.logs = append(pa.logs, entry)
}

// LogRoleAssignment logs a role assignment or revocation
func (pa *PermissionAuditor) LogRoleAssignment(userID, performedBy uuid.UUID, role string, action string) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	entry := AuditLogEntry{
		ID:          uuid.New(),
		UserID:      userID,
		PerformedBy: performedBy,
		Timestamp:   time.Now(),
		Action:      "role_" + action,
		Role:        role,
		Granted:     action == "assigned",
	}

	pa.logs = append(pa.logs, entry)
}

// LogPermissionGrant logs a permission grant or revoke
func (pa *PermissionAuditor) LogPermissionGrant(userID, performedBy uuid.UUID, permission string, granted bool) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	action := "permission_granted"
	if !granted {
		action = "permission_revoked"
	}

	entry := AuditLogEntry{
		ID:          uuid.New(),
		UserID:      userID,
		PerformedBy: performedBy,
		Timestamp:   time.Now(),
		Action:      action,
		Permission:  permission,
		Granted:     granted,
	}

	pa.logs = append(pa.logs, entry)
}

// GetUserAuditLogs returns audit logs for a specific user
func (pa *PermissionAuditor) GetUserAuditLogs(userID uuid.UUID, limit int) []AuditLogEntry {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	var userLogs []AuditLogEntry
	count := 0

	// Iterate from newest to oldest
	for i := len(pa.logs) - 1; i >= 0 && count < limit; i-- {
		if pa.logs[i].UserID == userID {
			userLogs = append(userLogs, pa.logs[i])
			count++
		}
	}

	return userLogs
}

// QueryAuditLogs queries audit logs with filters
func (pa *PermissionAuditor) QueryAuditLogs(query AuditQuery) []AuditLogEntry {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	var results []AuditLogEntry
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}

	for i := len(pa.logs) - 1; i >= 0 && len(results) < limit; i-- {
		entry := pa.logs[i]

		// Apply filters
		if query.UserID != nil && entry.UserID != *query.UserID {
			continue
		}
		if query.PerformedBy != nil && entry.PerformedBy != *query.PerformedBy {
			continue
		}
		if query.Action != nil && entry.Action != *query.Action {
			continue
		}
		if query.Permission != nil && entry.Permission != *query.Permission {
			continue
		}
		if query.Role != nil && entry.Role != *query.Role {
			continue
		}
		if query.StartTime != nil && entry.Timestamp.Before(*query.StartTime) {
			continue
		}
		if query.EndTime != nil && entry.Timestamp.After(*query.EndTime) {
			continue
		}

		results = append(results, entry)
	}

	return results
}

// CleanOldLogs removes logs older than the retention period
func (pa *PermissionAuditor) CleanOldLogs(retention time.Duration) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	cutoff := time.Now().Add(-retention)
	var newLogs []AuditLogEntry

	for _, log := range pa.logs {
		if log.Timestamp.After(cutoff) {
			newLogs = append(newLogs, log)
		}
	}

	pa.logs = newLogs
}

// GetPermissionStats returns statistics for a permission
func (pa *PermissionAuditor) GetPermissionStats(permission string) PermissionStats {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	stats := PermissionStats{
		Permission: permission,
	}
	users := make(map[uuid.UUID]bool)

	for _, log := range pa.logs {
		if log.Permission == permission && log.Action == "check" {
			stats.TotalChecks++
			if log.Granted {
				stats.Granted++
			} else {
				stats.Denied++
			}
			users[log.UserID] = true
		}
	}

	stats.UniqueUsers = len(users)
	return stats
}

// GetRoleStats returns statistics for role assignments
func (pa *PermissionAuditor) GetRoleStats() map[string]int {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	stats := make(map[string]int)

	for _, log := range pa.logs {
		if log.Action == "role_assigned" {
			stats[log.Role]++
		}
	}

	return stats
}

// ExportAuditLogs exports logs in a specific format
func (pa *PermissionAuditor) ExportAuditLogs(format string, query AuditQuery) ([]byte, error) {
	_ = pa.QueryAuditLogs(query) // Use the logs in production

	switch format {
	case "json":
		// In production, use encoding/json
		return []byte("json export"), nil
	case "csv":
		// In production, use encoding/csv
		return []byte("csv export"), nil
	default:
		return nil, ErrInvalidFormat
	}
}

// Errors
var (
	ErrInvalidFormat = errors.New("invalid export format")
)
