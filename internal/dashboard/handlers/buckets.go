package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// BucketRow is a single bucket for the bucket list template.
type BucketRow struct {
	Name            string
	Visibility      string
	ObjectCount     int
	TotalSize       int64
	LastModified    time.Time
	SizeFmt         string
	LastModifiedFmt string

	// The floor the bucket lives on, in the site's words (Phase 2):
	// tier_preference archive = "attic"; standard/performance/resilient =
	// "downstairs"; auto = "downstairs" too (auto resolves to the primary,
	// a header can still put single objects in the attic).
	TierPreference string
	FloorWord      string // downstairs | attic
	FloorClass     string // badge class
	AtticBytes     int64  // bytes of this bucket recorded in the attic
	AtticFmt       string
}

// floorWords maps a bucket tier preference onto the floor a customer reads.
func floorWords(tierPreference string) (word, class string) {
	if tierPreference == "archive" {
		return "attic", "badge-attic"
	}
	return "downstairs", "badge-downstairs"
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
	PreviewType     string
	CDNURL          string
	// IsArchived: an ATTIC object on an archive-class backend (Geyser tape),
	// which may need a restore before it is readable (V18.2). Derived from
	// the class the customer sees (engine.CustomerStorageClass): a
	// downstairs object the Smart tier parked on that backend is not "in
	// the attic". Drives the Restore button + live status fragment in the
	// object browser.
	IsArchived bool
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
		withCSRF(r.Context(), data)
		withRegionPicker(data)

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

// BucketCreateState is the outcome the shared bucket registry reports
// (mirrors internal/api's createBucketRegistry, which the dashboard cannot
// import — internal/api mounts the dashboard).
type BucketCreateState int

const (
	BucketCreateOK                BucketCreateState = iota // row inserted (or already owned)
	BucketCreateCapMax                                     // 1000-bucket hard cap
	BucketCreateCapFree                                    // free-tier bucket cap
	BucketCreateInvalidRegion                              // unknown region id
	BucketCreateRegionUnavailable                          // region known, no driver on this deployment
)

// BucketCreateResult is what a BucketCreator decided; Region is the stored region.
type BucketCreateResult struct {
	State  BucketCreateState
	Region string
}

// BucketCreator is the one bucket-creation rule, implemented by the API layer
// (`createBucketRegistry`: caps, region validation, sse default, registry row,
// slug, `bucket.created` event). The dashboard used to run its own copy that
// made a directory under DATA_PATH and skipped every rule the S3 and
// management entry points enforce (Review R12-25 / WP-R12-10, the R4-22
// class). region == "" means the deployment default.
type BucketCreator func(ctx context.Context, tenantID, name, region string) (BucketCreateResult, error)

// HandleCreateBucket handles POST /dashboard/buckets to create a new bucket.
// Name validation stays here (the form's error copy); everything else is the
// creator's decision. A nil creator means bucket creation is not wired
// (tests, dev without the API) and every attempt is refused.
func HandleCreateBucket(tmpl *template.Template, db *sql.DB, create BucketCreator, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "buckets")
		withCSRF(r.Context(), data)

		name := strings.TrimSpace(r.FormValue("name"))
		name = strings.ToLower(name)

		// Validate bucket name (S3-compatible rules).
		if !bucketNameRe.MatchString(name) {
			data["CreateError"] = "Invalid bucket name. Use 3-63 lowercase letters, numbers, hyphens, or dots."
			renderBucketList(w, r, tmpl, db, sd, data, logger)
			return
		}

		// Prevent path traversal.
		if strings.Contains(name, "..") {
			data["CreateError"] = "Invalid bucket name."
			renderBucketList(w, r, tmpl, db, sd, data, logger)
			return
		}

		if create == nil {
			data["CreateError"] = "Bucket creation is not available."
			renderBucketList(w, r, tmpl, db, sd, data, logger)
			return
		}

		region := strings.TrimSpace(r.FormValue("region"))
		res, err := create(r.Context(), sd.TenantID, name, region)
		if err != nil {
			logger.Error("create bucket", zap.String("bucket", name), zap.Error(err))
			data["CreateError"] = "Failed to create bucket."
			renderBucketList(w, r, tmpl, db, sd, data, logger)
			return
		}
		switch res.State {
		case BucketCreateCapMax:
			data["CreateError"] = "Bucket limit reached."
		case BucketCreateCapFree:
			data["CreateError"] = fmt.Sprintf("Free tier allows %d bucket. Upgrade your plan for more.", usage.FreeTierLimits.MaxBuckets)
		case BucketCreateInvalidRegion:
			data["CreateError"] = "Invalid region."
		case BucketCreateRegionUnavailable:
			data["CreateError"] = "Region " + drivers.RegionDisplayName(region) + " is not enabled on this deployment."
		}
		if _, refused := data["CreateError"]; refused {
			renderBucketList(w, r, tmpl, db, sd, data, logger)
			return
		}

		data["CreateSuccess"] = name
		renderBucketList(w, r, tmpl, db, sd, data, logger)
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
		withCSRF(r.Context(), data)
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
			var vis, slug string
			_ = db.QueryRowContext(r.Context(),
				`SELECT visibility FROM buckets WHERE tenant_id = $1 AND name = $2`,
				sd.TenantID, bucketName).Scan(&vis)
			_ = db.QueryRowContext(r.Context(),
				`SELECT COALESCE(slug, '') FROM tenants WHERE id = $1`,
				sd.TenantID).Scan(&slug)
			data["Visibility"] = vis
			cdnBase := ""
			if vis == "public-read" && slug != "" {
				cdnBase = cdnBaseHost + slug + "/" + bucketName
				data["CDNBaseURL"] = cdnBase
			}
			populateBucketObjects(r.Context(), db, sd.TenantID, bucketName, prefix, cdnBase, data)
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

// regionPickerOption is one row of the bucket-creation region picker.
type regionPickerOption struct {
	ID, Name  string
	Available bool // a driver is registered for it on this deployment
}

// regionPickerGroup is one residency group of the picker.
type regionPickerGroup struct {
	Label   string
	Regions []regionPickerOption
}

// withRegionPicker adds the region picker data: every account region grouped
// by residency, flagged with whether this deployment can store there, and the
// default (primary) region. The template used to hard-code eight regions,
// three of which never existed (Review R7-01).
func withRegionPicker(data map[string]any) {
	var groups []regionPickerGroup
	for _, g := range drivers.IDriveRegionGroups() {
		pg := regionPickerGroup{Label: g.Label}
		for _, r := range g.Regions {
			pg.Regions = append(pg.Regions, regionPickerOption{ID: r.ID, Name: r.Name, Available: drivers.IDriveRegionAvailable(r.ID)})
		}
		groups = append(groups, pg)
	}
	data["RegionGroups"] = groups
	data["DefaultRegion"] = drivers.IDriveDefaultRegion(os.Getenv)
}

func renderBucketList(w http.ResponseWriter, r *http.Request, tmpl *template.Template, db *sql.DB, sd *dashauth.SessionData, data map[string]any, logger *zap.Logger) {
	withRegionPicker(data)
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
		logger.Error("render bucket list", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func listBuckets(ctx context.Context, db *sql.DB, tenantID string) []BucketRow {
	rows, err := db.QueryContext(ctx,
		`SELECT b.name,
		        b.visibility,
		        b.tier_preference,
		        COALESCE(o.object_count, 0),
		        COALESCE(o.total_size, 0),
		        COALESCE(o.attic_size, 0),
		        COALESCE(o.last_modified, b.created_at)
		 FROM buckets b
		 LEFT JOIN (
		     SELECT bucket,
		            COUNT(*) AS object_count,
		            SUM(size_bytes) AS total_size,
		            SUM(size_bytes) FILTER (WHERE floor = 'vault') AS attic_size,
		            MAX(updated_at) AS last_modified
		     FROM object_head_cache
		     WHERE tenant_id = $1
		     GROUP BY bucket
		 ) o ON o.bucket = b.name
		 WHERE b.tenant_id = $1 AND b.name NOT LIKE '\_%'
		 ORDER BY b.name`, tenantID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var buckets []BucketRow
	for rows.Next() {
		var b BucketRow
		var lastMod time.Time
		if err := rows.Scan(&b.Name, &b.Visibility, &b.TierPreference, &b.ObjectCount, &b.TotalSize, &b.AtticBytes, &lastMod); err != nil {
			continue
		}
		b.LastModified = lastMod
		b.SizeFmt = formatBytes(b.TotalSize)
		b.AtticFmt = formatBytes(b.AtticBytes)
		b.FloorWord, b.FloorClass = floorWords(b.TierPreference)
		b.LastModifiedFmt = relativeTime(lastMod)
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return buckets
}

func previewTypeFromContentType(ct string) string {
	if strings.HasPrefix(ct, "image/") {
		return "image"
	}
	if strings.HasPrefix(ct, "video/") {
		return "video"
	}
	if strings.HasPrefix(ct, "audio/") {
		return "audio"
	}
	if strings.HasPrefix(ct, "text/") || ct == "application/json" || ct == "application/xml" || ct == "application/javascript" {
		return "text"
	}
	return ""
}

func populateBucketObjects(ctx context.Context, db *sql.DB, tenantID, bucket, prefix, cdnBase string, data map[string]any) {
	// Query all objects matching the prefix.
	query := `SELECT object_key, size_bytes, content_type, updated_at, COALESCE(backend_name, ''), floor
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

	hasArchived := false
	for rows.Next() {
		var key, contentType, backendName, floor string
		var size int64
		var lastMod time.Time
		if err := rows.Scan(&key, &size, &contentType, &lastMod, &backendName, &floor); err != nil {
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

		obj := ObjectRow{
			Key:             key,
			Display:         rel,
			Size:            size,
			ContentType:     contentType,
			LastModified:    lastMod,
			SizeFmt:         formatBytes(size),
			LastModifiedFmt: relativeTime(lastMod),
			PreviewType:     previewTypeFromContentType(contentType),
			IsArchived:      engine.IsArchiveClass(engine.CustomerStorageClass(floor, backendName)),
		}
		if obj.IsArchived {
			hasArchived = true
		}
		if cdnBase != "" {
			obj.CDNURL = cdnBase + "/" + key
		}
		objects = append(objects, obj)
	}
	if err := rows.Err(); err != nil {
		data["RowsError"] = err.Error()
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
	data["HasArchived"] = hasArchived
}
