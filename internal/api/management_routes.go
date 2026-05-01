package api

import (
	"encoding/json"
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

	s.router.Route("/api/v1/manage", func(r chi.Router) {
		r.Use(s.requireJWT)
		r.Use(rl.Middleware)

		r.Get("/buckets", s.handleMgmtListBuckets)
		r.Post("/buckets", s.handleMgmtCreateBucket)
		r.Get("/buckets/{name}", s.handleMgmtGetBucket)
		r.Delete("/buckets/{name}", s.handleMgmtDeleteBucket)

		r.Get("/buckets/{name}/objects", s.handleMgmtListObjects)

		r.Get("/keys", s.handleMgmtListKeys)
		r.Post("/keys", s.handleMgmtCreateKey)
		r.Delete("/keys/{id}", s.handleMgmtDeleteKey)

		r.Get("/usage", s.handleMgmtGetUsage)
	})
}

// --- Buckets ---

type mgmtBucket struct {
	Object    string    `json:"object"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	RequestID string    `json:"request_id,omitempty"`
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

	var rows_ interface{ Close() error }
	var scanErr error

	query := `SELECT name, created_at FROM buckets WHERE tenant_id = $1`
	args := []interface{}{tenantID}
	argIdx := 2

	if startingAfter != "" {
		query += ` AND name > $` + strconv.Itoa(argIdx)
		args = append(args, startingAfter)
		argIdx++
	}
	query += ` ORDER BY name LIMIT $` + strconv.Itoa(argIdx)
	args = append(args, limit+1)

	dbRows, dbErr := s.db.QueryContext(r.Context(), query, args...)
	if dbErr != nil {
		s.logger.Error("management list buckets", zap.Error(dbErr))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to list buckets", "")
		return
	}
	rows_ = dbRows
	defer func() { _ = rows_.Close() }()

	var buckets []mgmtBucket
	for dbRows.Next() {
		var b mgmtBucket
		if scanErr = dbRows.Scan(&b.Name, &b.CreatedAt); scanErr != nil {
			s.logger.Error("scan bucket", zap.Error(scanErr))
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
	dirPath := filepath.Join(dataPath, tenantID, req.Name)
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
	err := s.db.QueryRowContext(r.Context(),
		`SELECT name, created_at FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, name).Scan(&b.Name, &b.CreatedAt)
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found",
			"bucket not found", "name")
		return
	}
	b.Object = "bucket"
	b.RequestID = getRequestID(w)
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
	dirPath := filepath.Join(dataPath, tenantID, name)

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

	query := `SELECT object_key, size, etag, content_type, last_modified
		FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`
	args := []interface{}{tenantID, bucket}
	argIdx := 3

	if prefix != "" {
		query += ` AND object_key LIKE $` + strconv.Itoa(argIdx)
		args = append(args, prefix+"%")
		argIdx++
	}
	if startingAfter != "" {
		query += ` AND object_key > $` + strconv.Itoa(argIdx)
		args = append(args, startingAfter)
		argIdx++
	}
	query += ` ORDER BY object_key LIMIT $` + strconv.Itoa(argIdx)
	args = append(args, limit+1)

	dbRows, err := s.db.QueryContext(r.Context(), query, args...)
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
			"object":      "api_key",
			"id":          k.ID,
			"name":        k.Name,
			"key":         k.Key,
			"permissions": k.Permissions,
			"expires_at":  k.ExpiresAt,
			"created_at":  k.CreatedAt,
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

	var req struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	key, err := s.auth.GenerateAPIKey(r.Context(), userID, req.Name)
	if err != nil {
		s.logger.Error("management create key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create API key", "")
		return
	}

	if len(req.Permissions) > 0 {
		key.Permissions = req.Permissions
	}

	resp := map[string]interface{}{
		"object":      "api_key",
		"id":          key.ID,
		"name":        key.Name,
		"key":         key.Key,
		"secret":      key.Secret,
		"permissions": key.Permissions,
		"created_at":  key.CreatedAt,
		"request_id":  getRequestID(w),
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
