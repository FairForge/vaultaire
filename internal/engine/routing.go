package engine

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
)

type LocationStore struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewLocationStore(db *sql.DB, logger *zap.Logger) *LocationStore {
	return &LocationStore{db: db, logger: logger}
}

func (s *LocationStore) RecordLocation(ctx context.Context, tenant, bucket, key, backend, storageClass string, sizeBytes int64) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name, storage_class, size_bytes, stored_at, last_accessed)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			backend_name  = EXCLUDED.backend_name,
			storage_class = EXCLUDED.storage_class,
			size_bytes    = EXCLUDED.size_bytes,
			stored_at     = NOW()`,
		tenant, bucket, key, backend, storageClass, sizeBytes)
	if err != nil {
		s.logger.Error("failed to record object location",
			zap.Error(err),
			zap.String("tenant", tenant),
			zap.String("bucket", bucket),
			zap.String("key", key))
	}
	return err
}

func (s *LocationStore) LookupBackend(ctx context.Context, tenant, bucket, key string) (string, error) {
	if s.db == nil {
		return "", nil
	}
	var backend string
	err := s.db.QueryRowContext(ctx, `
		SELECT backend_name FROM object_locations
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenant, bucket, key).Scan(&backend)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	go func() { // #nosec G118 -- fire-and-forget access-time touch; must outlive the request, request ctx would cancel it
		_, _ = s.db.ExecContext(context.Background(), `
			UPDATE object_locations SET last_accessed = NOW()
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			tenant, bucket, key)
	}()
	return backend, nil
}

func (s *LocationStore) RemoveLocation(ctx context.Context, tenant, bucket, key string) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM object_locations
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenant, bucket, key)
	return err
}

func (s *LocationStore) CountByBackend(ctx context.Context) (map[string]int64, error) {
	if s.db == nil {
		return map[string]int64{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, COUNT(*) FROM object_locations GROUP BY backend_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int64)
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	return counts, rows.Err()
}

func (s *LocationStore) TouchLastAccessed(ctx context.Context, tenant, bucket, key string) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE object_locations SET last_accessed = NOW()
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenant, bucket, key)
	return err
}
