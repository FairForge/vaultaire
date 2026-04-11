package api

import (
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"

	"go.uber.org/zap"
)

const (
	multipartTempBase = "/tmp/vaultaire-multipart"
	maxPartNumber     = 10000
)

// In-memory fallback for when DB is not available (test mode).
// Production always uses PostgreSQL.
var (
	memUploads   = make(map[string]*memUpload)
	memUploadsMu sync.RWMutex
)

type memUpload struct {
	TenantID string
	Bucket   string
	Key      string
	Status   string // "active", "completed", "aborted"
	Parts    map[int]memPart
	Created  time.Time
}

type memPart struct {
	ETag string
	Size int64
}

// multipartDir returns the temp directory for a specific upload's parts.
func multipartDir(uploadID string) string {
	return filepath.Join(multipartTempBase, uploadID)
}

// partFilePath returns the temp file path for a specific part.
func partFilePath(uploadID string, partNumber int) string {
	return filepath.Join(multipartTempBase, uploadID, fmt.Sprintf("part-%05d", partNumber))
}

func (s *Server) handleInitiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := fmt.Sprintf("upload-%d-%d", time.Now().Unix(), time.Now().Nanosecond())

	// Persist upload record
	if s.db != nil {
		_, err := s.db.ExecContext(r.Context(), `
			INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status)
			VALUES ($1, $2, $3, $4, 'active')
		`, uploadID, t.ID, bucket, object)
		if err != nil {
			s.logger.Error("failed to create multipart upload record", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		memUploads[uploadID] = &memUpload{
			TenantID: t.ID,
			Bucket:   bucket,
			Key:      object,
			Status:   "active",
			Parts:    make(map[int]memPart),
			Created:  time.Now(),
		}
		memUploadsMu.Unlock()
	}

	// Create temp directory for part files
	if err := os.MkdirAll(multipartDir(uploadID), 0700); err != nil {
		s.logger.Error("failed to create multipart temp dir", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("initiated multipart upload",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.String("tenant_id", t.ID))

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(InitiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      object,
		UploadID: uploadID,
	}); err != nil {
		s.logger.Error("failed to encode initiate response", zap.Error(err))
	}
}

func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")
	partNumberStr := r.URL.Query().Get("partNumber")

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > maxPartNumber {
		WriteS3Error(w, ErrInvalidPartNumber, r.URL.Path, generateRequestID())
		return
	}

	// Verify upload exists, is active, and belongs to this tenant
	if s.db != nil {
		var status string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status FROM multipart_uploads
			WHERE upload_id = $1 AND tenant_id = $2
		`, uploadID, t.ID).Scan(&status)
		if err == sql.ErrNoRows || (err == nil && status != "active") {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
		if err != nil {
			s.logger.Error("failed to query multipart upload", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.RLock()
		mu, ok := memUploads[uploadID]
		memUploadsMu.RUnlock()
		if !ok || mu.TenantID != t.ID || mu.Status != "active" {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
	}

	// Stream part data to temp file while computing MD5 ETag
	pp := partFilePath(uploadID, partNumber)
	f, err := os.Create(pp) // #nosec G304 — path derived from validated uploadID + partNumber
	if err != nil {
		s.logger.Error("failed to create part temp file", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	size, err := io.Copy(f, io.TeeReader(r.Body, hasher))
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(pp)
		s.logger.Error("failed to write part data", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("\"%x\"", hasher.Sum(nil))

	// Record part metadata
	if s.db != nil {
		_, err := s.db.ExecContext(r.Context(), `
			INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (upload_id, part_number) DO UPDATE SET
				etag       = EXCLUDED.etag,
				size_bytes = EXCLUDED.size_bytes,
				created_at = NOW()
		`, uploadID, partNumber, etag, size)
		if err != nil {
			s.logger.Error("failed to record part metadata", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		if mu, ok := memUploads[uploadID]; ok {
			mu.Parts[partNumber] = memPart{ETag: etag, Size: size}
		}
		memUploadsMu.Unlock()
	}

	s.logger.Debug("uploaded part",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.Int("partNumber", partNumber),
		zap.Int64("size", size),
		zap.String("etag", etag))

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// partRecord holds part metadata from either DB or in-memory store.
type partRecord struct {
	PartNumber int
	ETag       string
	Size       int64
}

func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")

	// Verify upload is active and belongs to this tenant
	if s.db != nil {
		var status string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status FROM multipart_uploads
			WHERE upload_id = $1 AND tenant_id = $2
		`, uploadID, t.ID).Scan(&status)
		if err == sql.ErrNoRows || (err == nil && status != "active") {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
		if err != nil {
			s.logger.Error("failed to query multipart upload", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.RLock()
		mu, ok := memUploads[uploadID]
		memUploadsMu.RUnlock()
		if !ok || mu.TenantID != t.ID || mu.Status != "active" {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
	}

	// Parse the CompleteMultipartUpload XML body (AWS clients send this)
	var completeReq CompleteMultipartUploadRequest
	if r.Body != nil {
		if decErr := xml.NewDecoder(r.Body).Decode(&completeReq); decErr != nil {
			s.logger.Debug("no complete request body, using all uploaded parts", zap.Error(decErr))
		}
	}

	// Load all uploaded parts, ordered by part number
	var parts []partRecord
	if s.db != nil {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT part_number, etag, size_bytes FROM multipart_parts
			WHERE upload_id = $1
			ORDER BY part_number ASC
		`, uploadID)
		if err != nil {
			s.logger.Error("failed to query parts", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p partRecord
			if err := rows.Scan(&p.PartNumber, &p.ETag, &p.Size); err != nil {
				s.logger.Error("failed to scan part row", zap.Error(err))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			parts = append(parts, p)
		}
		if err := rows.Err(); err != nil {
			s.logger.Error("parts iteration error", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.RLock()
		mu := memUploads[uploadID]
		for pn, mp := range mu.Parts {
			parts = append(parts, partRecord{PartNumber: pn, ETag: mp.ETag, Size: mp.Size})
		}
		memUploadsMu.RUnlock()
		// Sort by part number (map iteration is random)
		sortParts(parts)
	}

	if len(parts) == 0 {
		WriteS3Error(w, ErrInvalidPart, r.URL.Path, generateRequestID())
		return
	}

	// If the client sent a specific part list, validate and select those parts
	if len(completeReq.Parts) > 0 {
		// Validate ascending order
		for i := 1; i < len(completeReq.Parts); i++ {
			if completeReq.Parts[i].PartNumber <= completeReq.Parts[i-1].PartNumber {
				WriteS3Error(w, ErrInvalidPartOrder, r.URL.Path, generateRequestID())
				return
			}
		}
		// Build lookup of uploaded parts
		uploaded := make(map[int]partRecord, len(parts))
		for _, p := range parts {
			uploaded[p.PartNumber] = p
		}
		// Match requested parts against uploaded parts
		selected := make([]partRecord, 0, len(completeReq.Parts))
		for _, rp := range completeReq.Parts {
			up, ok := uploaded[rp.PartNumber]
			if !ok {
				WriteS3Error(w, ErrInvalidPart, r.URL.Path, generateRequestID())
				return
			}
			if strings.Trim(rp.ETag, "\"") != strings.Trim(up.ETag, "\"") {
				WriteS3Error(w, ErrInvalidPart, r.URL.Path, generateRequestID())
				return
			}
			selected = append(selected, up)
		}
		parts = selected
	}

	// Compute total size and S3-compatible multipart ETag:
	// ETag = MD5(concat(MD5_part1 + MD5_part2 + ...))-N
	var totalSize int64
	etagHasher := md5.New() // #nosec G401 — S3 spec requires MD5 for multipart ETags
	for _, p := range parts {
		totalSize += p.Size
		raw := strings.Trim(p.ETag, "\"")
		if decoded, err := hex.DecodeString(raw); err == nil {
			etagHasher.Write(decoded)
		}
	}
	finalETag := fmt.Sprintf("\"%x-%d\"", etagHasher.Sum(nil), len(parts))

	// Stream assembled parts to backend via pipe
	pr, pw := io.Pipe()
	containerName := t.NamespaceContainer(bucket)

	errCh := make(chan error, 1)

	// Writer goroutine: read temp files in order, write into pipe
	go func() {
		defer func() {
			if err := pw.Close(); err != nil {
				s.logger.Debug("pipe writer close", zap.Error(err))
			}
		}()
		for _, p := range parts {
			pp := partFilePath(uploadID, p.PartNumber)
			f, err := os.Open(pp) // #nosec G304 — path derived from validated uploadID
			if err != nil {
				_ = pw.CloseWithError(fmt.Errorf("open part %d: %w", p.PartNumber, err))
				return
			}
			_, copyErr := io.Copy(pw, f)
			_ = f.Close()
			if copyErr != nil {
				_ = pw.CloseWithError(fmt.Errorf("stream part %d: %w", p.PartNumber, copyErr))
				return
			}
		}
	}()

	// Upload assembled stream to backend
	go func() {
		_, putErr := s.engine.Put(r.Context(), containerName, object, pr)
		_ = pr.Close()
		errCh <- putErr
	}()

	if uploadErr := <-errCh; uploadErr != nil {
		s.logger.Error("multipart backend storage failed",
			zap.Error(uploadErr),
			zap.String("bucket", bucket),
			zap.String("key", object))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Mark completed and update head cache
	etagValue := strings.Trim(finalETag, "\"")
	if s.db != nil {
		_, _ = s.db.ExecContext(r.Context(), `
			UPDATE multipart_uploads SET status = 'completed' WHERE upload_id = $1
		`, uploadID)

		contentType := r.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		_, dbErr := s.db.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes   = EXCLUDED.size_bytes,
				etag         = EXCLUDED.etag,
				content_type = EXCLUDED.content_type,
				updated_at   = NOW()
		`, t.ID, bucket, object, totalSize, etagValue, contentType)
		if dbErr != nil {
			s.logger.Error("failed to update head cache after multipart complete", zap.Error(dbErr))
		}
	} else {
		memUploadsMu.Lock()
		if mu, ok := memUploads[uploadID]; ok {
			mu.Status = "completed"
		}
		memUploadsMu.Unlock()
	}

	// Clean up temp files
	_ = os.RemoveAll(multipartDir(uploadID))

	s.logger.Info("multipart upload completed",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.Int64("totalSize", totalSize),
		zap.Int("parts", len(parts)),
		zap.String("etag", finalETag))

	location := fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object)
	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(CompleteMultipartUploadResult{
		Location: location,
		Bucket:   bucket,
		Key:      object,
		ETag:     finalETag,
	}); err != nil {
		s.logger.Error("failed to encode complete response", zap.Error(err))
	}
}

func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")

	if s.db != nil {
		result, err := s.db.ExecContext(r.Context(), `
			UPDATE multipart_uploads SET status = 'aborted'
			WHERE upload_id = $1 AND tenant_id = $2 AND status = 'active'
		`, uploadID, t.ID)
		if err != nil {
			s.logger.Error("failed to abort multipart upload", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if rows, _ := result.RowsAffected(); rows == 0 {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		mu, ok := memUploads[uploadID]
		if !ok || mu.TenantID != t.ID || mu.Status != "active" {
			memUploadsMu.Unlock()
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
		mu.Status = "aborted"
		memUploadsMu.Unlock()
	}

	_ = os.RemoveAll(multipartDir(uploadID))

	s.logger.Info("aborted multipart upload",
		zap.String("bucket", bucket),
		zap.String("object", object),
		zap.String("uploadID", uploadID))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")

	// Verify upload exists and is active
	if s.db != nil {
		var status string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status FROM multipart_uploads
			WHERE upload_id = $1 AND tenant_id = $2
		`, uploadID, t.ID).Scan(&status)
		if err == sql.ErrNoRows || (err == nil && status != "active") {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
		if err != nil {
			s.logger.Error("failed to query multipart upload for list parts", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.RLock()
		mu, ok := memUploads[uploadID]
		memUploadsMu.RUnlock()
		if !ok || mu.TenantID != t.ID || mu.Status != "active" {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
	}

	// Fetch parts
	var items []ListPartItem
	if s.db != nil {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT part_number, etag, size_bytes, created_at FROM multipart_parts
			WHERE upload_id = $1
			ORDER BY part_number ASC
		`, uploadID)
		if err != nil {
			s.logger.Error("failed to list parts", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var item ListPartItem
			var createdAt time.Time
			if err := rows.Scan(&item.PartNumber, &item.ETag, &item.Size, &createdAt); err != nil {
				continue
			}
			item.LastModified = createdAt.UTC().Format(time.RFC3339)
			items = append(items, item)
		}
	} else {
		memUploadsMu.RLock()
		mu := memUploads[uploadID]
		for pn, mp := range mu.Parts {
			items = append(items, ListPartItem{
				PartNumber:   pn,
				ETag:         mp.ETag,
				Size:         mp.Size,
				LastModified: time.Now().UTC().Format(time.RFC3339),
			})
		}
		memUploadsMu.RUnlock()
		sortPartItems(items)
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(ListPartsResult{
		Bucket:   bucket,
		Key:      object,
		UploadID: uploadID,
		Parts:    items,
	}); err != nil {
		s.logger.Error("failed to encode list parts response", zap.Error(err))
	}
}

func (s *Server) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	var uploads []ListMultipartUploadItem
	if s.db != nil {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT upload_id, object_key, created_at FROM multipart_uploads
			WHERE tenant_id = $1 AND bucket = $2 AND status = 'active'
			ORDER BY created_at ASC
		`, t.ID, bucket)
		if err != nil {
			s.logger.Error("failed to list multipart uploads", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var item ListMultipartUploadItem
			var createdAt time.Time
			if err := rows.Scan(&item.UploadID, &item.Key, &createdAt); err != nil {
				continue
			}
			item.Initiated = createdAt.UTC().Format(time.RFC3339)
			uploads = append(uploads, item)
		}
	} else {
		memUploadsMu.RLock()
		for uid, mu := range memUploads {
			if mu.TenantID == t.ID && mu.Bucket == bucket && mu.Status == "active" {
				uploads = append(uploads, ListMultipartUploadItem{
					UploadID:  uid,
					Key:       mu.Key,
					Initiated: mu.Created.UTC().Format(time.RFC3339),
				})
			}
		}
		memUploadsMu.RUnlock()
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(ListMultipartUploadsResult{
		Bucket:  bucket,
		Uploads: uploads,
	}); err != nil {
		s.logger.Error("failed to encode list uploads response", zap.Error(err))
	}
}

// sortParts sorts partRecord slices by PartNumber ascending.
func sortParts(parts []partRecord) {
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j].PartNumber < parts[j-1].PartNumber; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
}

// sortPartItems sorts ListPartItem slices by PartNumber ascending.
func sortPartItems(items []ListPartItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].PartNumber < items[j-1].PartNumber; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// XML structures for multipart upload requests and responses.

type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type CompleteMultipartUploadRequest struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type ListPartsResult struct {
	XMLName  xml.Name       `xml:"ListPartsResult"`
	Bucket   string         `xml:"Bucket"`
	Key      string         `xml:"Key"`
	UploadID string         `xml:"UploadId"`
	Parts    []ListPartItem `xml:"Part"`
}

type ListPartItem struct {
	PartNumber   int    `xml:"PartNumber"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

type ListMultipartUploadsResult struct {
	XMLName xml.Name                  `xml:"ListMultipartUploadsResult"`
	Bucket  string                    `xml:"Bucket"`
	Uploads []ListMultipartUploadItem `xml:"Upload"`
}

type ListMultipartUploadItem struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}
