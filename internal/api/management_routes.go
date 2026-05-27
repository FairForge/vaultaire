package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

func (s *Server) registerManagementRoutes() {
	rl := NewManagementRateLimiter()
	im := newIdempotencyMiddleware(s.db, s.logger)

	s.router.Route("/api/v1/manage", func(r chi.Router) {
		r.Use(s.requireJWT)
		r.Use(rl.Middleware)
		r.Use(im.Middleware)

		r.Get("/buckets", s.handleMgmtListBuckets)
		r.Post("/buckets", s.handleMgmtCreateBucket)
		r.Get("/buckets/{name}", s.handleMgmtGetBucket)
		r.Patch("/buckets/{name}", s.handleMgmtPatchBucket)
		r.Delete("/buckets/{name}", s.handleMgmtDeleteBucket)

		r.Get("/buckets/{name}/objects", s.handleMgmtListObjects)

		r.Get("/keys", s.handleMgmtListKeys)
		r.Post("/keys", s.handleMgmtCreateKey)
		r.Delete("/keys/{id}", s.handleMgmtDeleteKey)

		r.Get("/usage", s.handleMgmtGetUsage)

		r.Post("/account/export", s.handleMgmtExportData)
		r.Get("/account/export/{id}", s.handleMgmtGetExport)
		r.Delete("/account", s.handleMgmtDeleteAccount)
		r.Post("/account/cancel-deletion", s.handleMgmtCancelDeletion)
	})
}

// --- Buckets ---

type mgmtBucket struct {
	Object    string            `json:"object"`
	Name      string            `json:"name"`
	Metadata  map[string]string `json:"metadata"`
	CreatedAt time.Time         `json:"created_at"`
	RequestID string            `json:"request_id,omitempty"`
}

func (s *Server) handleMgmtListBuckets(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	startingAfter := r.URL.Query().Get("starting_after")

	if s.db == nil {
		writeListResponse(w, nil, false, "", 0)
		return
	}

	var rows *sql.Rows
	var dbErr error
	if startingAfter != "" {
		rows, dbErr = s.db.QueryContext(r.Context(),
			`SELECT name, created_at FROM buckets WHERE tenant_id = $1 AND name > $2 ORDER BY name LIMIT $3`,
			tenantID, startingAfter, limit+1)
	} else {
		rows, dbErr = s.db.QueryContext(r.Context(),
			`SELECT name, created_at FROM buckets WHERE tenant_id = $1 ORDER BY name LIMIT $2`,
			tenantID, limit+1)
	}
	if dbErr != nil {
		s.logger.Error("management list buckets", zap.Error(dbErr))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to list buckets", "")
		return
	}
	defer func() { _ = rows.Close() }()

	var buckets []mgmtBucket
	for rows.Next() {
		var b mgmtBucket
		if err := rows.Scan(&b.Name, &b.CreatedAt); err != nil {
			s.logger.Error("scan bucket", zap.Error(err))
			continue
		}
		b.Object = "bucket"
		buckets = append(buckets, b)
	}

	hasMore := len(buckets) > limit
	if hasMore {
		buckets = buckets[:limit]
	}

	var countResult int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`, tenantID).Scan(&countResult)

	items := make([]interface{}, len(buckets))
	for i, b := range buckets {
		items[i] = b
	}

	nextCursor := ""
	if hasMore {
		nextCursor = buckets[len(buckets)-1].Name
	}

	writeListResponse(w, items, hasMore, nextCursor, countResult)
}

func (s *Server) handleMgmtCreateBucket(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if req.Name == "" {
		writeManagementError(w, ErrTypeInvalidRequest, "missing_parameter", "bucket name is required", "name")
		return
	}

	if !validateBucketName(req.Name) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_bucket_name",
			"bucket name must be 3-63 characters, lowercase alphanumeric and hyphens", "name")
		return
	}

	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire"
	}
	dirPath := filepath.Clean(filepath.Join(dataPath, tenantID, req.Name)) // #nosec G703 — name validated by validateBucketName (a-z0-9.-)
	if err := os.MkdirAll(dirPath, 0750); err != nil {
		s.logger.Error("create bucket dir", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create bucket", "")
		return
	}

	if s.db != nil {
		result, dbErr := s.db.ExecContext(r.Context(), `
			INSERT INTO buckets (tenant_id, name, visibility)
			VALUES ($1, $2, 'private')
			ON CONFLICT (tenant_id, name) DO NOTHING
		`, tenantID, req.Name)
		if dbErr != nil {
			s.logger.Error("persist bucket", zap.Error(dbErr))
			writeManagementError(w, ErrTypeAPI, "db_error", "failed to persist bucket", "")
			return
		}
		if rows, _ := result.RowsAffected(); rows == 0 {
			writeManagementError(w, ErrTypeConflict, "bucket_exists",
				"a bucket with this name already exists", "name")
			return
		}

		auth.EnsureTenantSlug(r.Context(), s.db, tenantID, s.logger)
	}

	bucket := mgmtBucket{
		Object:    "bucket",
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
		RequestID: getRequestID(w),
	}
	writeJSON(w, http.StatusCreated, bucket)
}

func (s *Server) handleMgmtGetBucket(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	name := chi.URLParam(r, "name")

	if s.db == nil {
		writeManagementError(w, ErrTypeAPI, "no_database", "database unavailable", "")
		return
	}

	var b mgmtBucket
	var metaJSON []byte
	err := s.db.QueryRowContext(r.Context(),
		`SELECT name, COALESCE(metadata, '{}'), created_at FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, name).Scan(&b.Name, &metaJSON, &b.CreatedAt)
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found",
			"bucket not found", "name")
		return
	}
	b.Object = "bucket"
	b.Metadata = make(map[string]string)
	_ = json.Unmarshal(metaJSON, &b.Metadata)
	b.RequestID = getRequestID(w)
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleMgmtPatchBucket(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	name := chi.URLParam(r, "name")

	if s.db == nil {
		writeManagementError(w, ErrTypeAPI, "no_database", "database unavailable", "")
		return
	}

	var req struct {
		Metadata map[string]interface{} `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if req.Metadata == nil {
		writeManagementError(w, ErrTypeInvalidRequest, "missing_parameter", "metadata field is required", "metadata")
		return
	}

	var existing json.RawMessage
	var createdAt time.Time
	err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(metadata, '{}'), created_at FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, name).Scan(&existing, &createdAt)
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return
	}

	merged, mergeErr := mergeMetadata(existing, req.Metadata)
	if mergeErr != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_metadata", mergeErr.Error(), "metadata")
		return
	}

	_, err = s.db.ExecContext(r.Context(),
		`UPDATE buckets SET metadata = $1, updated_at = NOW() WHERE tenant_id = $2 AND name = $3`,
		merged, tenantID, name)
	if err != nil {
		s.logger.Error("update bucket metadata", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to update metadata", "")
		return
	}

	b := mgmtBucket{
		Object:    "bucket",
		Name:      name,
		Metadata:  make(map[string]string),
		CreatedAt: createdAt,
		RequestID: getRequestID(w),
	}
	_ = json.Unmarshal(merged, &b.Metadata)
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleMgmtDeleteBucket(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	name := chi.URLParam(r, "name")

	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire"
	}
	dirPath := filepath.Clean(filepath.Join(dataPath, tenantID, name)) // #nosec G703 — name from URL param, tenant from JWT

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
			return
		}
		s.logger.Error("read bucket dir", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read bucket", "")
		return
	}

	for _, entry := range entries {
		n := entry.Name()
		if !entry.IsDir() && n != "" && n[0] != '.' {
			writeManagementError(w, ErrTypeConflict, "bucket_not_empty",
				"bucket is not empty", "name")
			return
		}
	}

	if err := os.RemoveAll(dirPath); err != nil {
		s.logger.Error("delete bucket dir", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to delete bucket", "")
		return
	}

	if s.db != nil {
		_, _ = s.db.ExecContext(r.Context(),
			`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, name)
	}

	resp := map[string]interface{}{
		"object":     "bucket",
		"name":       name,
		"deleted":    true,
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Objects ---

type mgmtObject struct {
	Object       string    `json:"object"`
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	ContentType  string    `json:"content_type"`
	LastModified time.Time `json:"last_modified"`
}

func (s *Server) handleMgmtListObjects(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	bucket := chi.URLParam(r, "name")

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	prefix := r.URL.Query().Get("prefix")
	startingAfter := r.URL.Query().Get("starting_after")

	if s.db == nil {
		writeListResponse(w, nil, false, "", 0)
		return
	}

	var dbRows *sql.Rows
	var dbErr2 error
	lim := limit + 1
	switch {
	case prefix != "" && startingAfter != "":
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size, etag, content_type, last_modified FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key LIKE $3 AND object_key > $4 ORDER BY object_key LIMIT $5`,
			tenantID, bucket, prefix+"%", startingAfter, lim)
	case prefix != "":
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size, etag, content_type, last_modified FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key LIKE $3 ORDER BY object_key LIMIT $4`,
			tenantID, bucket, prefix+"%", lim)
	case startingAfter != "":
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size, etag, content_type, last_modified FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key > $3 ORDER BY object_key LIMIT $4`,
			tenantID, bucket, startingAfter, lim)
	default:
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size, etag, content_type, last_modified FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 ORDER BY object_key LIMIT $3`,
			tenantID, bucket, lim)
	}
	err := dbErr2
	if err != nil {
		s.logger.Error("management list objects", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to list objects", "")
		return
	}
	defer func() { _ = dbRows.Close() }()

	var objects []mgmtObject
	for dbRows.Next() {
		var o mgmtObject
		if err := dbRows.Scan(&o.Key, &o.Size, &o.ETag, &o.ContentType, &o.LastModified); err != nil {
			s.logger.Error("scan object", zap.Error(err))
			continue
		}
		o.Object = "object"
		objects = append(objects, o)
	}

	hasMore := len(objects) > limit
	if hasMore {
		objects = objects[:limit]
	}

	items := make([]interface{}, len(objects))
	for i, o := range objects {
		items[i] = o
	}

	nextCursor := ""
	if hasMore {
		nextCursor = objects[len(objects)-1].Key
	}

	writeListResponse(w, items, hasMore, nextCursor, len(items))
}

// --- Keys ---

func (s *Server) handleMgmtListKeys(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	if userID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_user", "user not found in token", "")
		return
	}

	keys, err := s.auth.ListAPIKeys(r.Context(), userID)
	if err != nil {
		s.logger.Error("management list keys", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list API keys", "")
		return
	}

	items := make([]interface{}, len(keys))
	for i, k := range keys {
		items[i] = map[string]interface{}{
			"object":       "api_key",
			"id":           k.ID,
			"name":         k.Name,
			"key":          k.Key,
			"permissions":  k.Permissions,
			"bucket_scope": k.BucketScope,
			"ip_allowlist": k.IPAllowlist,
			"expires_at":   k.ExpiresAt,
			"created_at":   k.CreatedAt,
		}
	}

	writeListResponse(w, items, false, "", len(items))
}

func (s *Server) handleMgmtCreateKey(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	if userID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_user", "user not found in token", "")
		return
	}

	const maxKeysPerTenant = 50
	tenantIDForLimit, _ := r.Context().Value(tenantIDKey).(string)
	if s.db != nil && tenantIDForLimit != "" {
		var keyCount int
		_ = s.db.QueryRowContext(r.Context(),
			"SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1", tenantIDForLimit).Scan(&keyCount)
		if keyCount >= maxKeysPerTenant {
			writeManagementError(w, ErrTypeConflict, "key_limit_exceeded",
				fmt.Sprintf("maximum %d API keys per account", maxKeysPerTenant), "")
			return
		}
	}

	var req struct {
		Name        string     `json:"name"`
		Permissions []string   `json:"permissions"`
		BucketScope []string   `json:"bucket_scope"`
		IPAllowlist []string   `json:"ip_allowlist"`
		ExpiresAt   *time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if len(req.Permissions) > 0 {
		if err := auth.ValidatePermissions(req.Permissions); err != nil {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_permissions", err.Error(), "permissions")
			return
		}
	}

	var opts *auth.KeyCreateOptions
	if len(req.Permissions) > 0 || len(req.BucketScope) > 0 || len(req.IPAllowlist) > 0 || req.ExpiresAt != nil {
		opts = &auth.KeyCreateOptions{
			Permissions: req.Permissions,
			BucketScope: req.BucketScope,
			IPAllowlist: req.IPAllowlist,
			ExpiresAt:   req.ExpiresAt,
		}
	}

	key, err := s.auth.GenerateAPIKey(r.Context(), userID, req.Name, opts)
	if err != nil {
		s.logger.Error("management create key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create API key", "")
		return
	}

	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	emitEvent(r.Context(), s.db, s.logger, "key.created", tenantID, map[string]interface{}{
		"key_id": key.ID, "name": key.Name,
	})

	resp := map[string]interface{}{
		"object":       "api_key",
		"id":           key.ID,
		"name":         key.Name,
		"key":          key.Key,
		"secret":       key.Secret,
		"permissions":  key.Permissions,
		"bucket_scope": key.BucketScope,
		"ip_allowlist": key.IPAllowlist,
		"expires_at":   key.ExpiresAt,
		"created_at":   key.CreatedAt,
		"request_id":   getRequestID(w),
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleMgmtDeleteKey(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	if userID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_user", "user not found in token", "")
		return
	}

	keyID := chi.URLParam(r, "id")

	if err := s.auth.RevokeAPIKey(r.Context(), userID, keyID); err != nil {
		s.logger.Error("management delete key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to revoke API key", "")
		return
	}

	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	emitEvent(r.Context(), s.db, s.logger, "key.revoked", tenantID, map[string]interface{}{
		"key_id": keyID,
	})

	resp := map[string]interface{}{
		"object":     "api_key",
		"id":         keyID,
		"deleted":    true,
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Usage ---

func (s *Server) handleMgmtGetUsage(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	used, limit, err := s.quotaManager.GetUsage(r.Context(), tenantID)
	if err != nil {
		s.logger.Error("management get usage", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to get usage", "")
		return
	}

	tier, _ := s.quotaManager.GetTier(r.Context(), tenantID)

	resp := map[string]interface{}{
		"object":        "usage",
		"tenant_id":     tenantID,
		"storage_used":  used,
		"storage_limit": limit,
		"usage_percent": float64(used) / float64(limit) * 100,
		"tier":          tier,
		"request_id":    getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Account (GDPR) ---

func (s *Server) handleMgmtExportData(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if userID == "" || tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_credentials", "user or tenant not found in token", "")
		return
	}

	exporter := NewAccountExporter(s.db, s.logger)
	result, err := exporter.CreateExport(r.Context(), userID, tenantID)
	if err != nil {
		s.logger.Error("management export data", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "export_failed", "failed to create data export", "")
		return
	}

	resp := map[string]interface{}{
		"object":     "data_export",
		"id":         result.ID,
		"data":       json.RawMessage(result.Data),
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMgmtGetExport(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	if userID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_user", "user not found in token", "")
		return
	}

	exportID := chi.URLParam(r, "id")
	exporter := NewAccountExporter(s.db, s.logger)
	result, err := exporter.GetExport(r.Context(), exportID, userID)
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "export_not_found", "export not found", "id")
		return
	}

	resp := map[string]interface{}{
		"object":          "data_export",
		"id":              result.ID,
		"status":          result.Status,
		"file_size_bytes": result.SizeBytes,
		"created_at":      result.CreatedAt,
		"request_id":      getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMgmtDeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if userID == "" || tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_credentials", "user or tenant not found in token", "")
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if req.Reason == "" {
		writeManagementError(w, ErrTypeInvalidRequest, "missing_parameter", "reason is required", "reason")
		return
	}

	svc := NewAccountDeletionService(s.db, s.logger)
	scheduledAt, err := svc.ScheduleDeletion(r.Context(), userID, tenantID, req.Reason)
	if err != nil {
		s.logger.Error("management delete account", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "deletion_failed", "failed to schedule account deletion", "")
		return
	}

	resp := map[string]interface{}{
		"object":       "account_deletion",
		"scheduled_at": scheduledAt,
		"message":      "Account scheduled for deletion. You have 30 days to cancel.",
		"request_id":   getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMgmtCancelDeletion(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	if userID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_user", "user not found in token", "")
		return
	}

	svc := NewAccountDeletionService(s.db, s.logger)
	if err := svc.CancelDeletion(r.Context(), userID); err != nil {
		s.logger.Error("management cancel deletion", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "cancel_failed", "failed to cancel account deletion", "")
		return
	}

	resp := map[string]interface{}{
		"object":     "account_deletion",
		"cancelled":  true,
		"message":    "Account deletion has been cancelled.",
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}
