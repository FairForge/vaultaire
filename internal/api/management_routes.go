package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// jsonAPIMiddleware returns the one rate limiter and one idempotency
// middleware shared by every JSON API group (built lazily; tests that mount
// routes by hand call it too).
func (s *Server) jsonAPIMiddleware() (*ManagementRateLimiter, *idempotencyMiddleware) {
	s.jsonAPIOnce.Do(func() {
		s.jsonAPILimiter = NewManagementRateLimiter()
		s.jsonAPIIdempotency = newIdempotencyMiddleware(s.db, s.logger)
	})
	return s.jsonAPILimiter, s.jsonAPIIdempotency
}

func (s *Server) registerManagementRoutes() {
	rl, im := s.jsonAPIMiddleware()

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
		r.Put("/buckets/{name}/tier", s.handleMgmtSetBucketTier)
		r.Put("/buckets/{name}/residency", s.handleMgmtSetBucketResidency)

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
	Region    string            `json:"region,omitempty"`
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
	if err := rows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
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
		Name   string `json:"name"`
		Region string `json:"region"`
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

	outcome, err := s.createBucketRegistry(r.Context(), tenantID, req.Name, req.Region)
	if err != nil {
		s.logger.Error("management create bucket", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create bucket", "")
		return
	}
	switch outcome.state {
	case bucketCreateCapMax:
		writeManagementError(w, ErrTypeConflict, "bucket_limit_exceeded",
			fmt.Sprintf("maximum %d buckets per account", maxBucketsPerTenant), "")
		return
	case bucketCreateCapFree:
		writeManagementError(w, ErrTypePermission, "free_tier_bucket_limit",
			fmt.Sprintf("Free tier allows %d bucket. Upgrade at https://stored.ge/dashboard/billing", usage.FreeTierLimits.MaxBuckets), "")
		return
	case bucketCreateInvalidRegion:
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_region", "region is not a known region id", "region")
		return
	case bucketCreateRegionUnavailable:
		writeManagementError(w, ErrTypeInvalidRequest, "region_unavailable",
			fmt.Sprintf("Region %s is not enabled on this deployment.", req.Region), "region")
		return
	}
	if outcome.alreadyOwned {
		writeManagementError(w, ErrTypeConflict, "bucket_exists",
			"a bucket with this name already exists", "name")
		return
	}

	bucket := mgmtBucket{
		Object:    "bucket",
		Name:      req.Name,
		Region:    outcome.region,
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
	// A name that cannot be a bucket cannot be one of ours; it also keeps
	// the marker path below inside DATA_PATH/<tenant>/ (no "." / "..").
	if !validateBucketName(name) {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return
	}

	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire"
	}
	dirPath, safe := safeBucketPath(dataPath, tenantID, name)
	if !safe {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return
	}

	if s.db != nil {
		// The registry decides (post-merge R4-22): this handler used to
		// read the DATA_PATH directory — which object writes never touch —
		// and deleted the row of a non-empty bucket. Same rule as S3
		// DeleteBucket now.
		outcome, err := s.deleteBucketRegistry(r.Context(), tenantID, name)
		if err != nil {
			s.logger.Error("delete bucket", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to delete bucket", "")
			return
		}
		switch outcome.state {
		case bucketDeleteMissing:
			writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
			return
		case bucketDeleteNotEmpty:
			writeManagementError(w, ErrTypeConflict, "bucket_not_empty", outcome.contents(), "name")
			return
		}
		_ = os.RemoveAll(dirPath) // the marker directory, if any
		emitEvent(r.Context(), s.db, s.logger, "bucket.deleted", tenantID, map[string]interface{}{
			"bucket": name,
		})
	} else {
		// No database (dev): the marker directory is all there is.
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
			`SELECT object_key, size_bytes, etag, content_type, updated_at FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key LIKE $3 AND object_key > $4 ORDER BY object_key LIMIT $5`,
			tenantID, bucket, prefix+"%", startingAfter, lim)
	case prefix != "":
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size_bytes, etag, content_type, updated_at FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key LIKE $3 ORDER BY object_key LIMIT $4`,
			tenantID, bucket, prefix+"%", lim)
	case startingAfter != "":
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size_bytes, etag, content_type, updated_at FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key > $3 ORDER BY object_key LIMIT $4`,
			tenantID, bucket, startingAfter, lim)
	default:
		dbRows, dbErr2 = s.db.QueryContext(r.Context(),
			`SELECT object_key, size_bytes, etag, content_type, updated_at FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 ORDER BY object_key LIMIT $3`,
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
	if err := dbRows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := dbRows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := dbRows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := dbRows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
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
		// api_keys has no tenant_id (R5-16 / WP-R9-6); resolve the tenant the
		// same way the S3 auth path does — users.email = tenants.email.
		_ = s.db.QueryRowContext(r.Context(), `
			SELECT COUNT(*) FROM api_keys ak
			JOIN users u ON u.id = ak.user_id
			JOIN tenants t ON t.email = u.email
			WHERE t.id = $1 AND ak.revoked_at IS NULL`, tenantIDForLimit).Scan(&keyCount)
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
		if errors.Is(err, auth.ErrKeyLimitReached) {
			writeManagementError(w, ErrTypeConflict, "key_limit_exceeded",
				"this plan's API key limit is reached; revoke a key or upgrade", "")
			return
		}
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

// accounts returns the deletion state machine (a per-call instance when the
// server was built without NewServer, as tests do).
func (s *Server) accounts() *account.Service {
	if s.accountSvc != nil {
		return s.accountSvc
	}
	return account.NewService(s.db, s.logger)
}

// mgmtDeletionMessage is the contract text of DELETE /api/v1/manage/account
// and DELETE /api/v1/user (WP-R10-3): what the runner does on the date.
func mgmtDeletionMessage(at time.Time) string {
	return fmt.Sprintf("Account scheduled for deletion on %s. You can cancel any time before then (POST /api/v1/manage/account/cancel-deletion); on that date your subscription is cancelled and your objects, keys and account records are erased by the deletion job. Backups age out within 7 days.",
		at.UTC().Format("2006-01-02"))
}

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

	audit.Record(r.Context(), s.db, audit.Entry{UserID: userID, TenantID: tenantID, Action: "account.exported",
		Resource: "export:" + result.ID, Metadata: map[string]any{"bytes": result.SizeBytes, "via": "management_api"}})

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

	scheduledAt, err := s.accounts().Schedule(r.Context(), userID, tenantID, req.Reason)
	if err != nil {
		s.logger.Error("management delete account", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "deletion_failed", "failed to schedule account deletion", "")
		return
	}

	audit.Record(r.Context(), s.db, audit.Entry{UserID: userID, TenantID: tenantID, Action: "account.deletion_scheduled",
		Resource: "user:" + userID, Severity: "warning", Metadata: map[string]any{"scheduled_at": scheduledAt, "reason": req.Reason, "via": "management_api"}})

	resp := map[string]interface{}{
		"object":       "account_deletion",
		"scheduled_at": scheduledAt,
		"message":      mgmtDeletionMessage(scheduledAt),
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

	if err := s.accounts().Cancel(r.Context(), userID); err != nil {
		if errors.Is(err, account.ErrNoPendingDeletion) {
			writeManagementError(w, ErrTypeInvalidRequest, "no_pending_deletion", "no account deletion is scheduled", "")
			return
		}
		s.logger.Error("management cancel deletion", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "cancel_failed", "failed to cancel account deletion", "")
		return
	}

	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: userID, TenantID: tenantID, Action: "account.deletion_cancelled",
		Resource: "user:" + userID, Metadata: map[string]any{"via": "management_api"}})

	resp := map[string]interface{}{
		"object":     "account_deletion",
		"cancelled":  true,
		"message":    "Account deletion has been cancelled.",
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Bucket Tier ---

var validTierPreferences = map[string]bool{
	"auto":        true,
	"performance": true,
	"standard":    true,
	"archive":     true,
	"resilient":   true, // Lyve-backed tier (engine/storage_class.go RESILIENT)
}

func (s *Server) handleMgmtSetBucketTier(w http.ResponseWriter, r *http.Request) {
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
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if !validTierPreferences[req.Tier] {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_tier",
			"tier must be one of: auto, performance, standard, archive, resilient", "tier")
		return
	}

	result, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET tier_preference = $1, updated_at = NOW()
		 WHERE tenant_id = $2 AND name = $3`,
		req.Tier, tenantID, name)
	if err != nil {
		s.logger.Error("set bucket tier", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to update tier preference", "")
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return
	}

	resp := map[string]interface{}{
		"object":          "bucket",
		"name":            name,
		"tier_preference": req.Tier,
		"request_id":      getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Bucket Data Residency ---

var validResidencyValues = map[string]bool{
	"us": true,
	"eu": true,
}

func (s *Server) handleMgmtSetBucketResidency(w http.ResponseWriter, r *http.Request) {
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
		Residency *string `json:"residency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	var residency sql.NullString
	if req.Residency != nil {
		if !validResidencyValues[*req.Residency] {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_residency",
				"residency must be one of: us, eu, or null", "residency")
			return
		}
		residency = sql.NullString{String: *req.Residency, Valid: true}
	}

	result, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET data_residency = $1, updated_at = NOW()
		 WHERE tenant_id = $2 AND name = $3`,
		residency, tenantID, name)
	if err != nil {
		s.logger.Error("set bucket residency", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to update data residency", "")
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return
	}

	respResidency := interface{}(nil)
	if residency.Valid {
		respResidency = residency.String
	}

	resp := map[string]interface{}{
		"object":         "bucket",
		"name":           name,
		"data_residency": respResidency,
		"request_id":     getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}
