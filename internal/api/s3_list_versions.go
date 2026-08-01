package api

import (
	"database/sql"
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// ListVersionsResult is the XML response for GET /{bucket}?versions.
type ListVersionsResult struct {
	XMLName             xml.Name            `xml:"ListVersionsResult"`
	Xmlns               string              `xml:"xmlns,attr"`
	Name                string              `xml:"Name"`
	Prefix              string              `xml:"Prefix"`
	KeyMarker           string              `xml:"KeyMarker"`
	VersionIdMarker     string              `xml:"VersionIdMarker"`
	NextKeyMarker       string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIdMarker string              `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int                 `xml:"MaxKeys"`
	IsTruncated         bool                `xml:"IsTruncated"`
	Versions            []VersionEntry      `xml:"Version"`
	DeleteMarkers       []DeleteMarkerEntry `xml:"DeleteMarker"`
}

type VersionEntry struct {
	Key          string     `xml:"Key"`
	VersionId    string     `xml:"VersionId"`
	IsLatest     bool       `xml:"IsLatest"`
	LastModified string     `xml:"LastModified"`
	ETag         string     `xml:"ETag"`
	Size         int64      `xml:"Size"`
	StorageClass string     `xml:"StorageClass"`
	Owner        OwnerEntry `xml:"Owner"`
}

type DeleteMarkerEntry struct {
	Key          string     `xml:"Key"`
	VersionId    string     `xml:"VersionId"`
	IsLatest     bool       `xml:"IsLatest"`
	LastModified string     `xml:"LastModified"`
	Owner        OwnerEntry `xml:"Owner"`
}

type OwnerEntry struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// versionRow is one entry of the merged version listing before rendering.
type versionRow struct {
	key            string
	versionID      string
	size           int64
	etag           string
	isLatest       bool
	isDeleteMarker bool
	createdAt      time.Time
}

// handleListObjectVersions serves GET /{bucket}?versions. Rows come from two
// sources: object_versions (buckets that have had versioning on) and, for
// keys with no version rows, the head cache as S3 "null" versions — matching
// AWS semantics for objects written while versioning was off. Ordering is
// key ascending, then newest version first within a key.
func (s *Server) handleListObjectVersions(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if s.db == nil {
		WriteS3Error(w, ErrNotImplemented, r.URL.Path, generateRequestID())
		return
	}

	var bucketExists bool
	err = s.db.QueryRowContext(r.Context(),
		`SELECT TRUE FROM buckets WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket).Scan(&bucketExists)
	if err == sql.ErrNoRows {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}
	if err != nil {
		s.logger.Error("list versions: bucket lookup failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	prefix := req.Query["prefix"]
	keyMarker := req.Query["key-marker"]
	versionIDMarker := req.Query["version-id-marker"]
	maxKeys := defaultMaxKeys
	if raw := req.Query["max-keys"]; raw != "" {
		if n, convErr := strconv.Atoi(raw); convErr == nil && n >= 0 && n <= defaultMaxKeys {
			maxKeys = n
		}
	}

	// A version-id-marker positions within keyMarker's versions via that
	// version's timestamp: resume strictly after (older than) it.
	var markerCreatedAt time.Time
	if keyMarker != "" && versionIDMarker != "" {
		err = s.db.QueryRowContext(r.Context(), `
			SELECT created_at FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			t.ID, req.Bucket, keyMarker, versionIDMarker).Scan(&markerCreatedAt)
		if err != nil && err != sql.ErrNoRows {
			s.logger.Error("list versions: marker lookup failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	fetch := maxKeys + 1

	// Source 1: real version rows.
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT object_key, version_id, size_bytes, etag, is_latest, is_delete_marker, created_at
		FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2
		  AND ($3 = '' OR object_key LIKE $3 || '%')
		  AND ($4 = '' OR object_key > $4
		       OR (object_key = $4 AND $5::timestamptz IS NOT NULL AND created_at < $5))
		ORDER BY object_key ASC, created_at DESC
		LIMIT $6`,
		t.ID, req.Bucket, prefix, keyMarker, nullableTime(markerCreatedAt), fetch)
	if err != nil {
		s.logger.Error("list versions: version query failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	var merged []versionRow
	for rows.Next() {
		var vr versionRow
		if scanErr := rows.Scan(&vr.key, &vr.versionID, &vr.size, &vr.etag,
			&vr.isLatest, &vr.isDeleteMarker, &vr.createdAt); scanErr != nil {
			_ = rows.Close()
			s.logger.Error("list versions: scan failed", zap.Error(scanErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		merged = append(merged, vr)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Error("list versions: version rows failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Source 2: head-cache objects with no version rows = S3 "null" versions.
	rows, err = s.db.QueryContext(r.Context(), `
		SELECT h.object_key, h.size_bytes, h.etag, h.updated_at
		FROM object_head_cache h
		WHERE h.tenant_id = $1 AND h.bucket = $2
		  AND ($3 = '' OR h.object_key LIKE $3 || '%')
		  AND ($4 = '' OR h.object_key > $4)
		  AND NOT EXISTS (
			SELECT 1 FROM object_versions v
			WHERE v.tenant_id = h.tenant_id AND v.bucket = h.bucket
			  AND v.object_key = h.object_key)
		ORDER BY h.object_key ASC
		LIMIT $5`,
		t.ID, req.Bucket, prefix, keyMarker, fetch)
	if err != nil {
		s.logger.Error("list versions: null-version query failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	for rows.Next() {
		vr := versionRow{versionID: "null", isLatest: true}
		if scanErr := rows.Scan(&vr.key, &vr.size, &vr.etag, &vr.createdAt); scanErr != nil {
			_ = rows.Close()
			s.logger.Error("list versions: null-version scan failed", zap.Error(scanErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		merged = append(merged, vr)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Error("list versions: null-version rows failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].key != merged[j].key {
			return merged[i].key < merged[j].key
		}
		return merged[i].createdAt.After(merged[j].createdAt)
	})

	result := ListVersionsResult{
		Xmlns:           "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:            req.Bucket,
		Prefix:          prefix,
		KeyMarker:       keyMarker,
		VersionIdMarker: versionIDMarker,
		MaxKeys:         maxKeys,
	}
	owner := OwnerEntry{ID: t.ID, DisplayName: t.ID}

	emitted := 0
	for _, vr := range merged {
		if emitted == maxKeys {
			result.IsTruncated = true
			break
		}
		if vr.isDeleteMarker {
			result.DeleteMarkers = append(result.DeleteMarkers, DeleteMarkerEntry{
				Key:          vr.key,
				VersionId:    vr.versionID,
				IsLatest:     vr.isLatest,
				LastModified: vr.createdAt.UTC().Format("2006-01-02T15:04:05.000Z"),
				Owner:        owner,
			})
		} else {
			etag := vr.etag
			if etag != "" && !strings.HasPrefix(etag, `"`) {
				etag = `"` + etag + `"`
			}
			result.Versions = append(result.Versions, VersionEntry{
				Key:          vr.key,
				VersionId:    vr.versionID,
				IsLatest:     vr.isLatest,
				LastModified: vr.createdAt.UTC().Format("2006-01-02T15:04:05.000Z"),
				ETag:         etag,
				Size:         vr.size,
				StorageClass: "STANDARD",
				Owner:        owner,
			})
		}
		result.NextKeyMarker = vr.key
		result.NextVersionIdMarker = vr.versionID
		emitted++
	}
	if !result.IsTruncated {
		result.NextKeyMarker = ""
		result.NextVersionIdMarker = ""
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if encErr := xml.NewEncoder(w).Encode(result); encErr != nil {
		s.logger.Error("list versions: encode failed", zap.Error(encErr))
	}
}

// nullableTime maps the zero time to SQL NULL so the marker predicate can
// distinguish "no version-id-marker" from a real timestamp.
func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}
