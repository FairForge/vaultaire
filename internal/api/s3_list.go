package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const defaultMaxKeys = 1000

// listPageSize bounds one database round trip while walking a listing; the
// walk keeps fetching until max-keys results are in hand or the bucket is
// exhausted, so a common prefix wider than one page cannot end the listing
// early (Review R4-05).
const listPageSize = 1000

// ListObjectsV2Params holds parsed S3 ListObjects / ListObjectsV2 query parameters.
type ListObjectsV2Params struct {
	Bucket            string
	Prefix            string
	Delimiter         string
	MaxKeys           int
	ContinuationToken string
	StartAfter        string
	EncodingType      string
	// V1 is a ListObjects (not list-type=2) request: paged with marker /
	// NextMarker instead of continuation tokens.
	V1     bool
	Marker string
}

// ListBucketV2Result is the XML response for ListObjects (v1) and ListObjectsV2.
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
	Marker                string              `xml:"Marker,omitempty"`
	NextMarker            string              `xml:"NextMarker,omitempty"`
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

// s3URLEncode encodes a key/prefix for an encoding-type=url list response the
// way AWS does: query-style escaping (space → '+', '+' → %2B, control and
// non-ASCII bytes percent-encoded) with '/' kept literal so prefixes stay
// readable. Clients decode with unquote_plus/QueryUnescape equivalents.
// Without this, keys containing XML-illegal bytes (e.g. control characters)
// are silently replaced with U+FFFD by the XML encoder and the client sees a
// different key than it stored.
func s3URLEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/")
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
		V1:                q.Get("list-type") != "2",
		Marker:            q.Get("marker"),
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

// prefixSuccessor returns the smallest string greater (in byte order) than
// every string that starts with p: the last rune advanced by one, carrying
// past U+10FFFF and skipping the surrogate gap so the result stays valid
// UTF-8 (PostgreSQL text rejects invalid UTF-8). "" has no successor.
// Used to skip a whole common prefix with one bound, and as the upper bound
// of a prefix range so the listing never needs LIKE (R9-19).
func prefixSuccessor(p string) string {
	for p != "" {
		r, size := utf8.DecodeLastRuneInString(p)
		head := p[:len(p)-size]
		if r == utf8.RuneError && size <= 1 {
			// Not valid UTF-8: advance the last byte.
			b := p[len(p)-1]
			if b < 0xff {
				return head + string([]byte{b + 1})
			}
			p = head
			continue
		}
		next := r + 1
		if next >= 0xD800 && next <= 0xDFFF {
			next = 0xE000
		}
		if next > utf8.MaxRune {
			// Carry into the previous rune.
			p = head
			continue
		}
		return head + string(next)
	}
	return ""
}

// listSource yields the keys of one bucket (already restricted to the prefix)
// in UTF-8 byte order, starting at lower (inclusive or exclusive) and below
// upper ("" = unbounded), at most limit of them.
type listSource interface {
	fetch(ctx context.Context, lower string, inclusive bool, upper string, limit int) ([]ListV2Entry, error)
}

// walkList is the listing algorithm shared by the database and driver
// sources. It keeps fetching pages until maxKeys results (keys + common
// prefixes) are in hand or the source is exhausted; a common prefix is left
// behind with its successor as the next lower bound, so a prefix holding a
// million keys costs one row. Returns the results, whether more exist, and
// the last emitted key or prefix (the continuation token / NextMarker).
func walkList(ctx context.Context, src listSource, prefix, delimiter, lower string, inclusive bool, maxKeys int) ([]ListV2Entry, []CommonPrefixEntry, bool, string, error) {
	upper := prefixSuccessor(prefix)
	if lower < prefix {
		lower, inclusive = prefix, true
	}
	var contents []ListV2Entry
	var prefixes []CommonPrefixEntry
	count := 0
	last := ""
	for {
		need := maxKeys - count + 1 // one extra row tells us whether more exist
		if need > listPageSize {
			need = listPageSize
		}
		batch, err := src.fetch(ctx, lower, inclusive, upper, need)
		if err != nil {
			return nil, nil, false, "", err
		}
		if len(batch) == 0 {
			return contents, prefixes, false, last, nil
		}
		skipped := false
		for _, e := range batch {
			if !strings.HasPrefix(e.Key, prefix) {
				// Defensive: the sources filter by prefix; never stall on a
				// stray row — move past it.
				lower, inclusive = e.Key, false
				continue
			}
			rest := e.Key[len(prefix):]
			if delimiter != "" {
				if idx := strings.Index(rest, delimiter); idx >= 0 {
					cp := prefix + rest[:idx+len(delimiter)]
					if count >= maxKeys {
						return contents, prefixes, true, last, nil
					}
					prefixes = append(prefixes, CommonPrefixEntry{Prefix: cp})
					count++
					last = cp
					lower, inclusive = prefixSuccessor(cp), true
					skipped = true
					break
				}
			}
			if count >= maxKeys {
				return contents, prefixes, true, last, nil
			}
			contents = append(contents, e)
			count++
			last = e.Key
			lower, inclusive = e.Key, false
		}
		if !skipped && len(batch) < need {
			return contents, prefixes, false, last, nil
		}
	}
}

// resumeAfter turns a continuation token / marker (the last emitted key or
// common prefix) into the next lower bound: a common prefix is skipped whole
// via its successor, a key is exclusive.
func resumeAfter(token, delimiter string) (lower string, inclusive bool) {
	if delimiter != "" && strings.HasSuffix(token, delimiter) {
		return prefixSuccessor(token), true
	}
	return token, false
}

// HandleListV2 processes S3 ListObjects (v1) and ListObjectsV2 requests with
// pagination, prefix, delimiter, continuation-token / marker and start-after.
func (a *S3ToEngine) HandleListV2(w http.ResponseWriter, r *http.Request, bucket string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	params := parseListV2Params(r, bucket)

	// AWS accepts only "url" (or absence) for encoding-type.
	if params.EncodingType != "" && params.EncodingType != "url" {
		WriteS3Error(w, ErrInvalidArgument, r.URL.Path, generateRequestID())
		return
	}

	// Cursor precedence: continuation-token, start-after, marker. A v1
	// request (no list-type=2) pages on marker/NextMarker, but every response
	// also carries the v2 fields — clients have always paged on them here.
	lower, inclusive := "", true
	switch {
	case params.ContinuationToken != "":
		token, decErr := decodeContinuationToken(params.ContinuationToken)
		if decErr != nil {
			WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
			return
		}
		lower, inclusive = resumeAfter(token, params.Delimiter)
	case params.StartAfter != "":
		lower, inclusive = params.StartAfter, false
	case params.Marker != "":
		lower, inclusive = resumeAfter(params.Marker, params.Delimiter)
	}

	var src listSource
	if a.db != nil {
		// A bucket with no registry row and no objects does not exist (AWS
		// 404); a phantom container that holds objects still lists (WP-R4-5).
		var exists bool
		if qErr := a.db.QueryRowContext(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)
			    OR EXISTS(SELECT 1 FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2)`,
			t.ID, bucket).Scan(&exists); qErr != nil {
			a.logger.Error("list objects: bucket lookup failed", zap.Error(qErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if !exists {
			reqID := generateRequestID()
			if suggestion := bucketSuggestion(r.Context(), a.db, t.ID, bucket); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
			}
			return
		}
		src = &dbListSource{db: a.db, tenantID: t.ID, bucket: bucket}
	} else {
		src = &driverListSource{eng: a.engine, tenant: t, bucket: bucket, prefix: params.Prefix}
	}

	contents, commonPrefixes, isTruncated, last, err := walkList(r.Context(), src, params.Prefix, params.Delimiter, lower, inclusive, params.MaxKeys)
	if err != nil {
		a.logger.Error("list objects failed", zap.String("bucket", bucket), zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

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
	if isTruncated && last != "" {
		result.NextContinuationToken = encodeContinuationToken(last)
	}
	if params.V1 {
		result.Marker = params.Marker
		if isTruncated && last != "" {
			result.NextMarker = last
		}
	}

	// encoding-type=url: percent-encode every field that carries a key or
	// prefix. Continuation tokens are already base64url and stay as-is.
	if params.EncodingType == "url" {
		result.EncodingType = "url"
		result.Prefix = s3URLEncode(result.Prefix)
		result.Delimiter = s3URLEncode(result.Delimiter)
		result.StartAfter = s3URLEncode(result.StartAfter)
		result.Marker = s3URLEncode(result.Marker)
		result.NextMarker = s3URLEncode(result.NextMarker)
		for i := range result.Contents {
			result.Contents[i].Key = s3URLEncode(result.Contents[i].Key)
		}
		for i := range result.CommonPrefixes {
			result.CommonPrefixes[i].Prefix = s3URLEncode(result.CommonPrefixes[i].Prefix)
		}
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

// dbListSource pages object_head_cache in UTF-8 byte order. The explicit
// COLLATE "C" is what S3 clients rely on (prod's database collation is
// en_US.UTF-8, which sorts `a-b` before `B` and `_x` after `ab` — R4-05);
// migration 069's index carries the same collation so the range and the
// order both use it.
type dbListSource struct {
	db       *sql.DB
	tenantID string
	bucket   string
}

func (s *dbListSource) fetch(ctx context.Context, lower string, inclusive bool, upper string, limit int) ([]ListV2Entry, error) {
	cmp := ">"
	if inclusive {
		cmp = ">="
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, size_bytes, etag, content_type, updated_at, COALESCE(backend_name, '')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2
		  AND object_key COLLATE "C" `+cmp+` $3::text
		  AND ($4::text = '' OR object_key COLLATE "C" < $4::text)
		ORDER BY object_key COLLATE "C" ASC
		LIMIT $5`, s.tenantID, s.bucket, lower, upper, limit)
	if err != nil {
		return nil, fmt.Errorf("query object_head_cache: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []ListV2Entry
	for rows.Next() {
		var key, etag, contentType, backendName string
		var size int64
		var updatedAt time.Time
		if scanErr := rows.Scan(&key, &size, &etag, &contentType, &updatedAt, &backendName); scanErr != nil {
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
			// Same backend→class mapping HEAD uses — the versitygw sweep
			// caught listings saying STANDARD while HEAD said GLACIER/RR for
			// the same object. Backup tools plan restores off the listed
			// class, so the two must agree.
			StorageClass: engine.BackendToStorageClass(backendName),
		})
	}
	if rowErr := rows.Err(); rowErr != nil {
		return nil, fmt.Errorf("iterate object_head_cache: %w", rowErr)
	}
	return entries, nil
}

// driverListSource is the no-database fallback: one engine List, sorted in
// byte order and sliced per fetch.
type driverListSource struct {
	eng    engine.Engine
	tenant *tenant.Tenant
	bucket string
	prefix string
	loaded bool
	all    []ListV2Entry
}

func (s *driverListSource) load(ctx context.Context) error {
	if s.loaded {
		return nil
	}
	container := s.tenant.NamespaceContainer(s.bucket)
	artifacts, err := s.eng.List(ctx, container, s.prefix)
	if err != nil {
		return fmt.Errorf("engine list: %w", err)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Key < artifacts[j].Key })
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for _, art := range artifacts {
		if s.prefix != "" && !strings.HasPrefix(art.Key, s.prefix) {
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
		s.all = append(s.all, ListV2Entry{
			Key:          art.Key,
			Size:         art.Size,
			ETag:         etag,
			LastModified: modified,
			StorageClass: "STANDARD",
		})
	}
	s.loaded = true
	return nil
}

func (s *driverListSource) fetch(ctx context.Context, lower string, inclusive bool, upper string, limit int) ([]ListV2Entry, error) {
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	return sliceListEntries(s.all, lower, inclusive, upper, limit), nil
}

// sliceListEntries applies a fetch window to an in-memory, byte-ordered list.
func sliceListEntries(all []ListV2Entry, lower string, inclusive bool, upper string, limit int) []ListV2Entry {
	var out []ListV2Entry
	for _, e := range all {
		if e.Key < lower || (!inclusive && e.Key == lower) {
			continue
		}
		if upper != "" && e.Key >= upper {
			break
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// memListSource wraps a byte-ordered slice as a listSource.
type memListSource []ListV2Entry

func (m memListSource) fetch(_ context.Context, lower string, inclusive bool, upper string, limit int) ([]ListV2Entry, error) {
	return sliceListEntries(m, lower, inclusive, upper, limit), nil
}

// processListEntries applies delimiter grouping and max-keys truncation to an
// already-sorted slice (kept for the in-memory callers and their tests).
// Returns contents, commonPrefixes, isTruncated, and the last emitted key or
// prefix — the continuation token.
func processListEntries(entries []ListV2Entry, prefix, delimiter string, maxKeys int) ([]ListV2Entry, []CommonPrefixEntry, bool, string) {
	sorted := append([]ListV2Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	contents, prefixes, truncated, last, _ := walkList(context.Background(), memListSource(sorted), prefix, delimiter, prefix, true, maxKeys)
	return contents, prefixes, truncated, last
}
