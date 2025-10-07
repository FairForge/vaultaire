package retention

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PolicyService manages retention policies
type PolicyService struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewPolicyService creates a new policy service
func NewPolicyService(db *sql.DB, logger *zap.Logger) *PolicyService {
	return &PolicyService{
		db:     db,
		logger: logger,
	}
}

// CreatePolicy creates a new retention policy
func (s *PolicyService) CreatePolicy(ctx context.Context, policy *RetentionPolicy) (*RetentionPolicy, error) {
	// Validate data category
	validCategories := map[string]bool{
		CategoryFiles:     true,
		CategoryAuditLogs: true,
		CategoryUserData:  true,
		CategoryBackups:   true,
		CategoryTempFiles: true,
	}
	if !validCategories[policy.DataCategory] {
		return nil, fmt.Errorf("invalid data category: %s", policy.DataCategory)
	}

	// Validate retention period
	if policy.RetentionPeriod <= 0 {
		return nil, fmt.Errorf("retention period must be positive")
	}

	// Validate action
	validActions := map[string]bool{
		ActionDelete:    true,
		ActionArchive:   true,
		ActionAnonymize: true,
	}
	if policy.Action != "" && !validActions[policy.Action] {
		return nil, fmt.Errorf("invalid action: %s", policy.Action)
	}

	// Set defaults
	if policy.Action == "" {
		policy.Action = ActionDelete
	}
	if policy.GracePeriod == 0 {
		policy.GracePeriod = 7 * 24 * time.Hour
	}

	// Generate ID and timestamps
	policy.ID = uuid.New()
	policy.CreatedAt = time.Now()
	policy.UpdatedAt = time.Now()

	// If we have a database, persist it
	if s.db != nil {
		query := `
			INSERT INTO retention_policies
			(id, name, description, data_category, retention_period, grace_period,
			 action, enabled, tenant_id, backend_id, container_name,
			 use_backend_object_lock, use_backend_versioning, use_backend_lifecycle,
			 created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		`
		_, err := s.db.ExecContext(ctx, query,
			policy.ID, policy.Name, policy.Description, policy.DataCategory,
			policy.RetentionPeriod, policy.GracePeriod, policy.Action,
			policy.Enabled, sqlNullString(policy.TenantID),
			sqlNullString(policy.BackendID), sqlNullString(policy.ContainerName),
			policy.UseBackendObjectLock, policy.UseBackendVersioning, policy.UseBackendLifecycle,
			policy.CreatedAt, policy.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to create policy: %w", err)
		}

		s.logger.Info("created retention policy",
			zap.String("policy_id", policy.ID.String()),
			zap.String("name", policy.Name),
			zap.String("category", policy.DataCategory),
			zap.String("backend_id", policy.BackendID))
	}

	return policy, nil
}

// GetPolicy retrieves a policy by ID
func (s *PolicyService) GetPolicy(ctx context.Context, id uuid.UUID) (*RetentionPolicy, error) {
	if s.db == nil {
		return nil, fmt.Errorf("database not configured")
	}

	query := `
		SELECT id, name, description, data_category, retention_period, grace_period,
		       action, enabled, tenant_id, backend_id, container_name,
		       use_backend_object_lock, use_backend_versioning, use_backend_lifecycle,
		       created_at, updated_at
		FROM retention_policies
		WHERE id = $1
	`

	var policy RetentionPolicy
	var tenantID, backendID, containerName sql.NullString
	var retentionNanos, graceNanos int64

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&policy.ID, &policy.Name, &policy.Description, &policy.DataCategory,
		&retentionNanos, &graceNanos, &policy.Action, &policy.Enabled,
		&tenantID, &backendID, &containerName,
		&policy.UseBackendObjectLock, &policy.UseBackendVersioning, &policy.UseBackendLifecycle,
		&policy.CreatedAt, &policy.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("policy not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get policy: %w", err)
	}

	policy.RetentionPeriod = time.Duration(retentionNanos)
	policy.GracePeriod = time.Duration(graceNanos)
	policy.TenantID = tenantID.String
	policy.BackendID = backendID.String
	policy.ContainerName = containerName.String

	return &policy, nil
}

// ListPolicies lists all retention policies
func (s *PolicyService) ListPolicies(ctx context.Context, tenantID string) ([]*RetentionPolicy, error) {
	policies := []*RetentionPolicy{}

	if s.db == nil {
		return policies, nil
	}

	query := `
		SELECT id, name, description, data_category, retention_period, grace_period,
		       action, enabled, tenant_id, backend_id, container_name,
		       use_backend_object_lock, use_backend_versioning, use_backend_lifecycle,
		       created_at, updated_at
		FROM retention_policies
		WHERE tenant_id = $1 OR tenant_id IS NULL
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, sqlNullString(tenantID))
	if err != nil {
		return nil, fmt.Errorf("failed to list policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var policy RetentionPolicy
		var tenantID, backendID, containerName sql.NullString
		var retentionNanos, graceNanos int64

		err := rows.Scan(
			&policy.ID, &policy.Name, &policy.Description, &policy.DataCategory,
			&retentionNanos, &graceNanos, &policy.Action, &policy.Enabled,
			&tenantID, &backendID, &containerName,
			&policy.UseBackendObjectLock, &policy.UseBackendVersioning, &policy.UseBackendLifecycle,
			&policy.CreatedAt, &policy.UpdatedAt)
		if err != nil {
			continue
		}

		policy.RetentionPeriod = time.Duration(retentionNanos)
		policy.GracePeriod = time.Duration(graceNanos)
		policy.TenantID = tenantID.String
		policy.BackendID = backendID.String
		policy.ContainerName = containerName.String

		policies = append(policies, &policy)
	}

	return policies, nil
}

// GetPoliciesForBackend gets policies applicable to a specific backend
func (s *PolicyService) GetPoliciesForBackend(ctx context.Context, backendID, tenantID string) ([]*RetentionPolicy, error) {
	policies := []*RetentionPolicy{}

	if s.db == nil {
		return policies, nil
	}

	// Get policies that either:
	// 1. Target this specific backend
	// 2. Are global (no backend specified)
	query := `
		SELECT id, name, description, data_category, retention_period, grace_period,
		       action, enabled, tenant_id, backend_id, container_name,
		       use_backend_object_lock, use_backend_versioning, use_backend_lifecycle,
		       created_at, updated_at
		FROM retention_policies
		WHERE enabled = true
		  AND (backend_id = $1 OR backend_id IS NULL)
		  AND (tenant_id = $2 OR tenant_id IS NULL)
		ORDER BY
		  CASE WHEN backend_id IS NOT NULL THEN 1 ELSE 2 END, -- Specific before global
		  created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, backendID, sqlNullString(tenantID))
	if err != nil {
		return nil, fmt.Errorf("failed to get policies for backend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var policy RetentionPolicy
		var tenantID, backendID, containerName sql.NullString
		var retentionNanos, graceNanos int64

		err := rows.Scan(
			&policy.ID, &policy.Name, &policy.Description, &policy.DataCategory,
			&retentionNanos, &graceNanos, &policy.Action, &policy.Enabled,
			&tenantID, &backendID, &containerName,
			&policy.UseBackendObjectLock, &policy.UseBackendVersioning, &policy.UseBackendLifecycle,
			&policy.CreatedAt, &policy.UpdatedAt)
		if err != nil {
			continue
		}

		policy.RetentionPeriod = time.Duration(retentionNanos)
		policy.GracePeriod = time.Duration(graceNanos)
		policy.TenantID = tenantID.String
		policy.BackendID = backendID.String
		policy.ContainerName = containerName.String

		policies = append(policies, &policy)
	}

	return policies, nil
}

// UpdatePolicy updates an existing retention policy
func (s *PolicyService) UpdatePolicy(ctx context.Context, policy *RetentionPolicy) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	// Validate same as CreatePolicy
	validCategories := map[string]bool{
		CategoryFiles:     true,
		CategoryAuditLogs: true,
		CategoryUserData:  true,
		CategoryBackups:   true,
		CategoryTempFiles: true,
	}
	if !validCategories[policy.DataCategory] {
		return fmt.Errorf("invalid data category: %s", policy.DataCategory)
	}

	if policy.RetentionPeriod <= 0 {
		return fmt.Errorf("retention period must be positive")
	}

	policy.UpdatedAt = time.Now()

	query := `
		UPDATE retention_policies
		SET name = $2, description = $3, data_category = $4,
		    retention_period = $5, grace_period = $6, action = $7,
		    enabled = $8, tenant_id = $9, backend_id = $10, container_name = $11,
		    use_backend_object_lock = $12, use_backend_versioning = $13,
		    use_backend_lifecycle = $14, updated_at = $15
		WHERE id = $1
	`

	result, err := s.db.ExecContext(ctx, query,
		policy.ID, policy.Name, policy.Description, policy.DataCategory,
		policy.RetentionPeriod, policy.GracePeriod, policy.Action,
		policy.Enabled, sqlNullString(policy.TenantID),
		sqlNullString(policy.BackendID), sqlNullString(policy.ContainerName),
		policy.UseBackendObjectLock, policy.UseBackendVersioning, policy.UseBackendLifecycle,
		policy.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to update policy: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("policy not found: %s", policy.ID)
	}

	s.logger.Info("updated retention policy",
		zap.String("policy_id", policy.ID.String()),
		zap.String("name", policy.Name))

	return nil
}

// DeletePolicy deletes a retention policy
func (s *PolicyService) DeletePolicy(ctx context.Context, id uuid.UUID) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	query := `DELETE FROM retention_policies WHERE id = $1`

	result, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("failed to delete policy: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("policy not found: %s", id)
	}

	s.logger.Info("deleted retention policy",
		zap.String("policy_id", id.String()))

	return nil
}

// Helper function for nullable strings
func sqlNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: s, Valid: true}
}
