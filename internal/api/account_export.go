package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

type ExportResult struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
	SizeBytes int64           `json:"file_size_bytes"`
	CreatedAt time.Time       `json:"created_at"`
}

type AccountExporter struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewAccountExporter(db *sql.DB, logger *zap.Logger) *AccountExporter {
	return &AccountExporter{db: db, logger: logger}
}

func (e *AccountExporter) CreateExport(ctx context.Context, userID, tenantID string) (*ExportResult, error) {
	if e.db == nil {
		return &ExportResult{Status: "completed", Data: json.RawMessage(`{}`), CreatedAt: time.Now().UTC()}, nil
	}

	var exportID string
	err := e.db.QueryRowContext(ctx,
		`INSERT INTO account_exports (user_id, tenant_id, status) VALUES ($1, $2, 'processing') RETURNING id`,
		userID, tenantID).Scan(&exportID)
	if err != nil {
		return nil, fmt.Errorf("create export record: %w", err)
	}

	data := make(map[string]interface{})

	// User profile (never expose password_hash).
	var profile struct {
		ID        string         `json:"id"`
		Email     string         `json:"email"`
		Company   sql.NullString `json:"-"`
		Role      sql.NullString `json:"-"`
		Status    sql.NullString `json:"-"`
		CreatedAt sql.NullTime   `json:"-"`
	}
	err = e.db.QueryRowContext(ctx,
		`SELECT id, email, company, role, status, created_at FROM users WHERE id = $1`, userID).
		Scan(&profile.ID, &profile.Email, &profile.Company, &profile.Role, &profile.Status, &profile.CreatedAt)
	if err == nil {
		userData := map[string]interface{}{
			"id":    profile.ID,
			"email": profile.Email,
		}
		if profile.Company.Valid {
			userData["company"] = profile.Company.String
		}
		if profile.Role.Valid {
			userData["role"] = profile.Role.String
		}
		if profile.Status.Valid {
			userData["status"] = profile.Status.String
		}
		if profile.CreatedAt.Valid {
			userData["created_at"] = profile.CreatedAt.Time
		}
		data["user"] = userData
	} else {
		e.logger.Warn("export: failed to query user profile", zap.Error(err))
	}

	// Tenant info.
	var tenantName, tenantPlan sql.NullString
	err = e.db.QueryRowContext(ctx,
		`SELECT name, plan FROM tenants WHERE id = $1`, tenantID).Scan(&tenantName, &tenantPlan)
	if err == nil {
		tenantData := map[string]interface{}{"id": tenantID}
		if tenantName.Valid {
			tenantData["name"] = tenantName.String
		}
		if tenantPlan.Valid {
			tenantData["plan"] = tenantPlan.String
		}
		data["tenant"] = tenantData
	} else {
		e.logger.Warn("export: failed to query tenant", zap.Error(err))
	}

	// Quota info.
	var storageUsed, storageLimit sql.NullInt64
	var tier sql.NullString
	err = e.db.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes, tier FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&storageUsed, &storageLimit, &tier)
	if err == nil {
		quotaData := map[string]interface{}{}
		if storageUsed.Valid {
			quotaData["storage_used_bytes"] = storageUsed.Int64
		}
		if storageLimit.Valid {
			quotaData["storage_limit_bytes"] = storageLimit.Int64
		}
		if tier.Valid {
			quotaData["tier"] = tier.String
		}
		data["quota"] = quotaData
	} else {
		e.logger.Warn("export: failed to query quota", zap.Error(err))
	}

	// Buckets.
	data["buckets"] = e.collectBuckets(ctx, tenantID)

	// Objects (from head cache).
	data["objects"] = e.collectObjects(ctx, tenantID)

	// API keys (never expose secret_hash or secret_key).
	data["api_keys"] = e.collectAPIKeys(ctx, userID)

	// Bandwidth usage (last 90 days).
	data["bandwidth_usage"] = e.collectBandwidth(ctx, tenantID)

	// Events (last 1000).
	data["events"] = e.collectEvents(ctx, tenantID)

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		_, _ = e.db.ExecContext(ctx,
			`UPDATE account_exports SET status = 'failed', error_message = $1 WHERE id = $2`,
			err.Error(), exportID)
		return nil, fmt.Errorf("marshal export data: %w", err)
	}

	_, _ = e.db.ExecContext(ctx,
		`UPDATE account_exports SET status = 'completed', file_size_bytes = $1, completed_at = NOW() WHERE id = $2`,
		len(jsonData), exportID)

	return &ExportResult{
		ID:        exportID,
		Status:    "completed",
		Data:      jsonData,
		SizeBytes: int64(len(jsonData)),
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (e *AccountExporter) GetExport(ctx context.Context, exportID, userID string) (*ExportResult, error) {
	if e.db == nil {
		return nil, fmt.Errorf("database unavailable")
	}

	var result ExportResult
	err := e.db.QueryRowContext(ctx,
		`SELECT id, status, file_size_bytes, created_at FROM account_exports WHERE id = $1 AND user_id = $2`,
		exportID, userID).Scan(&result.ID, &result.Status, &result.SizeBytes, &result.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get export: %w", err)
	}
	return &result, nil
}

func (e *AccountExporter) collectBuckets(ctx context.Context, tenantID string) []map[string]interface{} {
	rows, err := e.db.QueryContext(ctx,
		`SELECT name, visibility, created_at, COALESCE(metadata, '{}') FROM buckets WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		e.logger.Warn("export: failed to query buckets", zap.Error(err))
		return []map[string]interface{}{}
	}
	defer func() { _ = rows.Close() }()

	var buckets []map[string]interface{}
	for rows.Next() {
		var name, visibility string
		var createdAt time.Time
		var metaJSON []byte
		if err := rows.Scan(&name, &visibility, &createdAt, &metaJSON); err != nil {
			continue
		}
		b := map[string]interface{}{
			"name":       name,
			"visibility": visibility,
			"created_at": createdAt,
		}
		var meta map[string]interface{}
		if json.Unmarshal(metaJSON, &meta) == nil && len(meta) > 0 {
			b["metadata"] = meta
		}
		buckets = append(buckets, b)
	}
	if buckets == nil {
		return []map[string]interface{}{}
	}
	return buckets
}

func (e *AccountExporter) collectObjects(ctx context.Context, tenantID string) []map[string]interface{} {
	rows, err := e.db.QueryContext(ctx,
		`SELECT bucket, object_key, size, content_type, last_modified FROM object_head_cache WHERE tenant_id = $1 ORDER BY bucket, object_key`, tenantID)
	if err != nil {
		e.logger.Warn("export: failed to query objects", zap.Error(err))
		return []map[string]interface{}{}
	}
	defer func() { _ = rows.Close() }()

	var objects []map[string]interface{}
	for rows.Next() {
		var bucket, key, contentType string
		var size int64
		var lastModified time.Time
		if err := rows.Scan(&bucket, &key, &size, &contentType, &lastModified); err != nil {
			continue
		}
		objects = append(objects, map[string]interface{}{
			"bucket":        bucket,
			"key":           key,
			"size":          size,
			"content_type":  contentType,
			"last_modified": lastModified,
		})
	}
	if objects == nil {
		return []map[string]interface{}{}
	}
	return objects
}

func (e *AccountExporter) collectAPIKeys(ctx context.Context, userID string) []map[string]interface{} {
	rows, err := e.db.QueryContext(ctx,
		`SELECT id, name, COALESCE(permissions, '["*"]'), created_at FROM api_keys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		e.logger.Warn("export: failed to query api keys", zap.Error(err))
		return []map[string]interface{}{}
	}
	defer func() { _ = rows.Close() }()

	var keys []map[string]interface{}
	for rows.Next() {
		var id, name string
		var permsJSON []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &permsJSON, &createdAt); err != nil {
			continue
		}
		k := map[string]interface{}{
			"id":         id,
			"name":       name,
			"created_at": createdAt,
		}
		var perms []string
		if json.Unmarshal(permsJSON, &perms) == nil {
			k["permissions"] = perms
		}
		keys = append(keys, k)
	}
	if keys == nil {
		return []map[string]interface{}{}
	}
	return keys
}

func (e *AccountExporter) collectBandwidth(ctx context.Context, tenantID string) []map[string]interface{} {
	rows, err := e.db.QueryContext(ctx,
		`SELECT date, ingress_bytes, egress_bytes, requests FROM bandwidth_usage_daily WHERE tenant_id = $1 AND date >= NOW() - INTERVAL '90 days' ORDER BY date DESC`, tenantID)
	if err != nil {
		e.logger.Warn("export: failed to query bandwidth", zap.Error(err))
		return []map[string]interface{}{}
	}
	defer func() { _ = rows.Close() }()

	var usage []map[string]interface{}
	for rows.Next() {
		var date time.Time
		var ingress, egress, requests int64
		if err := rows.Scan(&date, &ingress, &egress, &requests); err != nil {
			continue
		}
		usage = append(usage, map[string]interface{}{
			"date":          date.Format("2006-01-02"),
			"ingress_bytes": ingress,
			"egress_bytes":  egress,
			"requests":      requests,
		})
	}
	if usage == nil {
		return []map[string]interface{}{}
	}
	return usage
}

func (e *AccountExporter) collectEvents(ctx context.Context, tenantID string) []map[string]interface{} {
	rows, err := e.db.QueryContext(ctx,
		`SELECT id, type, data, created_at FROM events WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1000`, tenantID)
	if err != nil {
		e.logger.Warn("export: failed to query events", zap.Error(err))
		return []map[string]interface{}{}
	}
	defer func() { _ = rows.Close() }()

	var events []map[string]interface{}
	for rows.Next() {
		var id, eventType string
		var dataJSON []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &eventType, &dataJSON, &createdAt); err != nil {
			continue
		}
		ev := map[string]interface{}{
			"id":         id,
			"type":       eventType,
			"created_at": createdAt,
		}
		var evData map[string]interface{}
		if json.Unmarshal(dataJSON, &evData) == nil {
			ev["data"] = evData
		}
		events = append(events, ev)
	}
	if events == nil {
		return []map[string]interface{}{}
	}
	return events
}
