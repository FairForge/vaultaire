package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const defaultMaxKeys = 1000

// ListObjectsV2Params holds parsed S3 ListObjectsV2 query parameters.
type ListObjectsV2Params struct {
	Bucket            string
	Prefix            string
	Delimiter         string
	MaxKeys           int
	ContinuationToken string
	StartAfter        string
	EncodingType      string
}

// ListBucketV2Result is the XML response for ListObjectsV2.
type ListBucketV2Result struct {
	XMLName               xml.Name            `xml:"ListBucketResult"`
	Xmlns                 string              `xml:"xmlns,attr"`
	Name                  string              `xml:"Name"`
	Prefix                string              `xml:"Prefix"`
	Delimiter             string              `xml:"Delimiter,omitempty"`
	MaxKeys               int                 `xml:"MaxKeys"`
	KeyCount              int                 `xml:"KeyCount"`
	IsTruncated           bool                `xml:"IsTruncated"`
	Contents              []ListV2Entry       `xml:"Contents,omitempty"`
	CommonPrefixes        []CommonPrefixEntry `xml:"CommonPrefixes,omitempty"`
	ContinuationToken     string              `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string              `xml:"NextContinuationToken,omitempty"`
	StartAfter            string              `xml:"StartAfter,omitempty"`
	EncodingType          string              `xml:"EncodingType,omitempty"`
}

// ListV2Entry represents a single object in the list response.
type ListV2Entry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

// CommonPrefixEntry represents a grouped prefix when delimiter is used.
type CommonPrefixEntry struct {
	Prefix string `xml:"Prefix"`
}

func encodeContinuationToken(key string) string {
	return base64.URLEncoding.EncodeToString([]byte(key))
}

func decodeContinuationToken(token string) (string, error) {
	data, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("decode continuation token: %w", err)
	}
	return string(data), nil
}

func parseListV2Params(r *http.Request, bucket string) ListObjectsV2Params {
	q := r.URL.Query()
	params := ListObjectsV2Params{
		Bucket:            bucket,
		Prefix:            q.Get("prefix"),
		Delimiter:         q.Get("delimiter"),
		MaxKeys:           defaultMaxKeys,
		ContinuationToken: q.Get("continuation-token"),
		StartAfter:        q.Get("start-after"),
		EncodingType:      q.Get("encoding-type"),
	}

	if params.ContinuationToken == "" && params.StartAfter == "" {
		if marker := q.Get("marker"); marker != "" {
			params.StartAfter = marker
		}
	}

	if mk := q.Get("max-keys"); mk != "" {
		if v, err := strconv.Atoi(mk); err == nil && v >= 0 {
			if v > 1000 {
				v = 1000
			}
			params.MaxKeys = v
		}
	}

	return params
}

// HandleListV2 processes S3 ListObjectsV2 requests with pagination,
// prefix, delimiter, continuation-token, and start-after support.
func (a *S3ToEngine) HandleListV2(w http.ResponseWriter, r *http.Request, bucket string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	params := parseListV2Params(r, bucket)

	cursor := ""
	if params.ContinuationToken != "" {
		cursor, err = decodeContinuationToken(params.ContinuationToken)
		if err != nil {
			WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
			return
		}
	} else if params.StartAfter != "" {
		cursor = params.StartAfter
	}

	var rawEntries []ListV2Entry
	if a.db != nil {
		rawEntries, err = a.fetchListBatch(r.Context(), t.ID, bucket, params.Prefix, cursor, params.MaxKeys, params.Delimiter)
	} else {
		rawEntries, err = a.listFromDriver(r.Context(), t, bucket, params.Prefix, cursor)
	}
	if err != nil {
		a.logger.Error("list objects failed", zap.String("bucket", bucket), zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	contents, commonPrefixes, isTruncated, lastKey := processListEntries(
		rawEntries, params.Prefix, params.Delimiter, params.MaxKeys,
	)

	result := ListBucketV2Result{
		Xmlns:          "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:           bucket,
		Prefix:         params.Prefix,
		MaxKeys:        params.MaxKeys,
		KeyCount:       len(contents) + len(commonPrefixes),
		IsTruncated:    isTruncated,
		Contents:       contents,
		CommonPrefixes: commonPrefixes,
	}

	if params.Delimiter != "" {
		result.Delimiter = params.Delimiter
	}
	if params.ContinuationToken != "" {
		result.ContinuationToken = params.ContinuationToken
	}
	if params.StartAfter != "" {
		result.StartAfter = params.StartAfter
	}
	if params.EncodingType != "" {
		result.EncodingType = params.EncodingType
	}
	if isTruncated && lastKey != "" {
		result.NextContinuationToken = encodeContinuationToken(lastKey)
	}

	w.Header().Set("Content-Type", "application/xml")
	if _, writeErr := w.Write([]byte(xml.Header)); writeErr != nil {
		a.logger.Error("failed to write XML header", zap.Error(writeErr))
		return
	}
	if encErr := xml.NewEncoder(w).Encode(result); encErr != nil {
		a.logger.Error("failed to encode list response", zap.Error(encErr))
	}
}

// fetchListBatch queries object_head_cache for a page of objects.
func (a *S3ToEngine) fetchListBatch(ctx context.Context, tenantID, bucket, prefix, cursor string, maxKeys int, delimiter string) ([]ListV2Entry, error) {
	fetchLimit := maxKeys + 1
	if delimiter != "" {
		fetchLimit = maxKeys * 10
		if fetchLimit < 1000 {
			fetchLimit = 1000
		}
		if fetchLimit > 10000 {
			fetchLimit = 10000
		}
	}

	escapedPrefix := strings.ReplaceAll(prefix, `\`, `\\`)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, `%`, `\%`)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, `_`, `\_`)
	likePattern := escapedPrefix + "%"

	var rows *sql.Rows
	var err error

	switch {
	case prefix == "" && cursor == "":
		rows, err = a.db.QueryContext(ctx,
			`SELECT object_key, size_bytes, etag, content_type, updated_at
			 FROM object_head_cache
			 WHERE tenant_id = $1 AND bucket = $2
			 ORDER BY object_key ASC
			 LIMIT $3`, tenantID, bucket, fetchLimit)
	case prefix == "":
		rows, err = a.db.QueryContext(ctx,
			`SELECT object_key, size_bytes, etag, content_type, updated_at
			 FROM object_head_cache
			 WHERE tenant_id = $1 AND bucket = $2 AND object_key > $3
			 ORDER BY object_key ASC
			 LIMIT $4`, tenantID, bucket, cursor, fetchLimit)
	case cursor == "":
		rows, err = a.db.QueryContext(ctx,
			`SELECT object_key, size_bytes, etag, content_type, updated_at
			 FROM object_head_cache
			 WHERE tenant_id = $1 AND bucket = $2
			   AND object_key LIKE $3 ESCAPE '\'
			 ORDER BY object_key ASC
			 LIMIT $4`, tenantID, bucket, likePattern, fetchLimit)
	default:
		rows, err = a.db.QueryContext(ctx,
			`SELECT object_key, size_bytes, etag, content_type, updated_at
			 FROM object_head_cache
			 WHERE tenant_id = $1 AND bucket = $2
			   AND object_key LIKE $3 ESCAPE '\'
			   AND object_key > $4
			 ORDER BY object_key ASC
			 LIMIT $5`, tenantID, bucket, likePattern, cursor, fetchLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("query object_head_cache: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []ListV2Entry
	for rows.Next() {
		var key, etag, contentType string
		var size int64
		var updatedAt time.Time
		if scanErr := rows.Scan(&key, &size, &etag, &contentType, &updatedAt); scanErr != nil {
			return nil, fmt.Errorf("scan object_head_cache: %w", scanErr)
		}
		if etag != "" && !strings.HasPrefix(etag, `"`) {
			etag = `"` + etag + `"`
		}
		entries = append(entries, ListV2Entry{
			Key:          key,
			Size:         size,
			ETag:         etag,
			LastModified: updatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			StorageClass: "STANDARD",
		})
	}
	if rowErr := rows.Err(); rowErr != nil {
		return nil, fmt.Errorf("iterate object_head_cache: %w", rowErr)
	}

	return entries, nil
}

// listFromDriver falls back to the engine driver when DB is unavailable.
func (a *S3ToEngine) listFromDriver(ctx context.Context, t *tenant.Tenant, bucket, prefix, cursor string) ([]ListV2Entry, error) {
	container := t.NamespaceContainer(bucket)
	artifacts, err := a.engine.List(ctx, container, prefix)
	if err != nil {
		return nil, fmt.Errorf("engine list: %w", err)
	}

	sort.Slice(artifacts, func(i, j int) bool {
		return artifacts[i].Key < artifacts[j].Key
	})

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	var entries []ListV2Entry
	for _, art := range artifacts {
		if prefix != "" && !strings.HasPrefix(art.Key, prefix) {
			continue
		}
		if cursor != "" && art.Key <= cursor {
			continue
		}
		modified := now
		if !art.Modified.IsZero() {
			modified = art.Modified.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		etag := art.ETag
		if etag != "" && !strings.HasPrefix(etag, `"`) {
			etag = `"` + etag + `"`
		}
		entries = append(entries, ListV2Entry{
			Key:          art.Key,
			Size:         art.Size,
			ETag:         etag,
			LastModified: modified,
			StorageClass: "STANDARD",
		})
	}

	return entries, nil
}

// processListEntries applies delimiter grouping and max-keys truncation.
// Returns contents, commonPrefixes, isTruncated, and the last key examined.
func processListEntries(entries []ListV2Entry, prefix, delimiter string, maxKeys int) ([]ListV2Entry, []CommonPrefixEntry, bool, string) {
	if delimiter == "" {
		if len(entries) > maxKeys {
			lastKey := ""
			if maxKeys > 0 {
				lastKey = entries[maxKeys-1].Key
			}
			return entries[:maxKeys], nil, true, lastKey
		}
		lastKey := ""
		if len(entries) > 0 {
			lastKey = entries[len(entries)-1].Key
		}
		return entries, nil, false, lastKey
	}

	var contents []ListV2Entry
	var commonPrefixes []CommonPrefixEntry
	seen := make(map[string]bool)
	count := 0
	var lastKey string

	for _, entry := range entries {
		lastKey = entry.Key

		keyAfterPrefix := entry.Key[len(prefix):]
		idx := strings.Index(keyAfterPrefix, delimiter)

		if idx >= 0 {
			cp := prefix + keyAfterPrefix[:idx+len(delimiter)]
			if !seen[cp] {
				if count >= maxKeys {
					return contents, commonPrefixes, true, lastKey
				}
				seen[cp] = true
				commonPrefixes = append(commonPrefixes, CommonPrefixEntry{Prefix: cp})
				count++
			}
		} else {
			if count >= maxKeys {
				return contents, commonPrefixes, true, lastKey
			}
			contents = append(contents, entry)
			count++
		}
	}

	return contents, commonPrefixes, false, lastKey
}
