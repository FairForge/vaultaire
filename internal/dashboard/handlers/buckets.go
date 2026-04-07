package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// BucketRow is a single bucket for the bucket list template.
type BucketRow struct {
	Name            string
	ObjectCount     int
	TotalSize       int64
	LastModified    time.Time
	SizeFmt         string
	LastModifiedFmt string
}

// ObjectRow is a single object for the bucket browser template.
type ObjectRow struct {
	Key             string
	Display         string
	Size            int64
	ContentType     string
	LastModified    time.Time
	SizeFmt         string
	LastModifiedFmt string
}

// PrefixRow is a common-prefix "folder" in the bucket browser.
type PrefixRow struct {
	FullPrefix string
	Display    string
}

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

// HandleBuckets returns an http.HandlerFunc that lists all buckets for the
// current tenant, with object counts and total sizes.
func HandleBuckets(tmpl *template.Template, db *sql.DB, dataPath string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "buckets")

		if db != nil {
			buckets := listBuckets(r.Context(), db, sd.TenantID)
			data["Buckets"] = buckets
			data["BucketCount"] = len(buckets)
		} else {
			data["Buckets"] = nil
			data["BucketCount"] = 0
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render buckets", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleCreateBucket handles POST /dashboard/buckets to create a new bucket.
func HandleCreateBucket(tmpl *template.Template, db *sql.DB, dataPath string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "buckets")

		name := strings.TrimSpace(r.FormValue("name"))
		name = strings.ToLower(name)

		// Validate bucket name (S3-compatible rules).
		if !bucketNameRe.MatchString(name) {
			data["CreateError"] = "Invalid bucket name. Use 3-63 lowercase letters, numbers, hyphens, or dots."
			renderBucketList(w, tmpl, db, sd, data, logger)
			return
		}

		// Prevent path traversal.
		if strings.Contains(name, "..") {
			data["CreateError"] = "Invalid bucket name."
			renderBucketList(w, tmpl, db, sd, data, logger)
			return
		}

		// Create the directory under the data path.
		if dataPath != "" {
			dirPath := filepath.Join(dataPath, name)
			cleanPath := filepath.Clean(dirPath)
			if !strings.HasPrefix(cleanPath, filepath.Clean(dataPath)) {
				data["CreateError"] = "Invalid bucket name."
				renderBucketList(w, tmpl, db, sd, data, logger)
				return
			}
			if err := os.MkdirAll(cleanPath, 0750); err != nil {
				logger.Error("create bucket directory", zap.Error(err))
				data["CreateError"] = "Failed to create bucket."
				renderBucketList(w, tmpl, db, sd, data, logger)
				return
			}
		}

		data["CreateSuccess"] = name
		renderBucketList(w, tmpl, db, sd, data, logger)
	}
}

// HandleBucketObjects returns an http.HandlerFunc that lists objects in a
// specific bucket with prefix-based "folder" navigation.
func HandleBucketObjects(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		bucketName := chi.URLParam(r, "name")
		if bucketName == "" {
			http.Redirect(w, r, "/dashboard/buckets", http.StatusSeeOther)
			return
		}

		prefix := r.URL.Query().Get("prefix")

		data := sessionData(sd, "buckets")
		data["BucketName"] = bucketName
		data["Prefix"] = prefix

		if prefix != "" {
			// Parent prefix: strip the last path component.
			parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
			if len(parts) > 1 {
				data["ParentPrefix"] = strings.Join(parts[:len(parts)-1], "/") + "/"
			}
			// else ParentPrefix is empty → goes back to bucket root
		}

		if db != nil {
			populateBucketObjects(r.Context(), db, sd.TenantID, bucketName, prefix, data)
		} else {
			data["ObjectCount"] = 0
			data["TotalSizeFmt"] = "0 B"
			data["Objects"] = nil
			data["Prefixes"] = nil
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render bucket objects", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func renderBucketList(w http.ResponseWriter, tmpl *template.Template, db *sql.DB, sd *dashauth.SessionData, data map[string]any, logger *zap.Logger) {
	if db != nil {
		buckets := listBuckets(context.Background(), db, sd.TenantID)
		data["Buckets"] = buckets
		data["BucketCount"] = len(buckets)
	} else {
		data["Buckets"] = nil
		data["BucketCount"] = 0
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		logger.Error("render bucket list", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func listBuckets(ctx context.Context, db *sql.DB, tenantID string) []BucketRow {
	rows, err := db.QueryContext(ctx,
		`SELECT bucket,
		        COUNT(*) AS object_count,
		        COALESCE(SUM(size_bytes), 0) AS total_size,
		        MAX(updated_at) AS last_modified
		 FROM object_head_cache
		 WHERE tenant_id = $1
		 GROUP BY bucket
		 ORDER BY bucket`, tenantID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var buckets []BucketRow
	for rows.Next() {
		var b BucketRow
		var lastMod time.Time
		if err := rows.Scan(&b.Name, &b.ObjectCount, &b.TotalSize, &lastMod); err != nil {
			continue
		}
		b.LastModified = lastMod
		b.SizeFmt = formatBytes(b.TotalSize)
		b.LastModifiedFmt = relativeTime(lastMod)
		buckets = append(buckets, b)
	}
	return buckets
}

func populateBucketObjects(ctx context.Context, db *sql.DB, tenantID, bucket, prefix string, data map[string]any) {
	// Query all objects matching the prefix.
	query := `SELECT object_key, size_bytes, content_type, updated_at
		 FROM object_head_cache
		 WHERE tenant_id = $1 AND bucket = $2`
	args := []any{tenantID, bucket}

	if prefix != "" {
		query += ` AND object_key LIKE $3`
		args = append(args, prefix+"%")
	}
	query += ` ORDER BY object_key`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		data["ObjectCount"] = 0
		data["TotalSizeFmt"] = "0 B"
		data["Objects"] = nil
		data["Prefixes"] = nil
		return
	}
	defer func() { _ = rows.Close() }()

	// Collect objects and extract common prefixes (simulated folder structure).
	prefixSet := make(map[string]bool)
	var objects []ObjectRow
	var totalSize int64
	var totalCount int

	for rows.Next() {
		var key, contentType string
		var size int64
		var lastMod time.Time
		if err := rows.Scan(&key, &size, &contentType, &lastMod); err != nil {
			continue
		}
		totalCount++
		totalSize += size

		// Strip the current prefix to get the relative key.
		rel := strings.TrimPrefix(key, prefix)

		// If the relative key contains a slash, it's in a "subfolder".
		if idx := strings.Index(rel, "/"); idx >= 0 {
			pfx := prefix + rel[:idx+1]
			prefixSet[pfx] = true
			continue
		}

		objects = append(objects, ObjectRow{
			Key:             key,
			Display:         rel,
			Size:            size,
			ContentType:     contentType,
			LastModified:    lastMod,
			SizeFmt:         formatBytes(size),
			LastModifiedFmt: relativeTime(lastMod),
		})
	}

	// Build sorted prefix list.
	var prefixes []PrefixRow
	for p := range prefixSet {
		display := strings.TrimPrefix(p, prefix)
		display = strings.TrimSuffix(display, "/")
		prefixes = append(prefixes, PrefixRow{
			FullPrefix: p,
			Display:    display,
		})
	}

	data["ObjectCount"] = totalCount
	data["TotalSizeFmt"] = formatBytes(totalSize)
	data["Objects"] = objects
	data["Prefixes"] = prefixes
}

// sessionData builds the base template data map from session.
func sessionData(sd *dashauth.SessionData, page string) map[string]any {
	return map[string]any{
		"Email":    sd.Email,
		"Role":     sd.Role,
		"UserID":   sd.UserID,
		"TenantID": sd.TenantID,
		"Page":     page,
	}
}
