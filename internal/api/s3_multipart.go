package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MultipartUpload tracks an in-progress multipart upload
type MultipartUpload struct {
	UploadID string
	Bucket   string
	Key      string
	Parts    map[int]*Part
	mu       sync.Mutex
}

// Part represents a single part of a multipart upload
type Part struct {
	PartNumber int
	ETag       string
	Size       int64
	Data       []byte
}

// Global storage for active uploads (in production, use database)
var (
	activeUploads = make(map[string]*MultipartUpload)
	uploadsMu     sync.RWMutex
)

func (s *Server) handleInitiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := fmt.Sprintf("upload-%d-%d", time.Now().Unix(), time.Now().Nanosecond())

	// Store the upload session
	upload := &MultipartUpload{
		UploadID: uploadID,
		Bucket:   bucket,
		Key:      object,
		Parts:    make(map[int]*Part),
	}

	uploadsMu.Lock()
	activeUploads[uploadID] = upload
	uploadsMu.Unlock()

	s.logger.Info("initiated multipart upload",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID))

	response := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult>
    <Bucket>%s</Bucket>
    <Key>%s</Key>
    <UploadId>%s</UploadId>
</InitiateMultipartUploadResult>`, bucket, object, uploadID)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(response))
}

func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// Get upload ID and part number from query params
	uploadID := r.URL.Query().Get("uploadId")
	partNumberStr := r.URL.Query().Get("partNumber")

	s.logger.Info("handling upload part",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.String("partNumber", partNumberStr))

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil {
		http.Error(w, "Invalid part number", http.StatusBadRequest)
		return
	}

	// Find the upload session
	uploadsMu.RLock()
	upload, exists := activeUploads[uploadID]
	uploadsMu.RUnlock()

	if !exists {
		s.logger.Error("upload not found", zap.String("uploadID", uploadID))
		http.Error(w, "Upload not found", http.StatusNotFound)
		return
	}

	// Read the part data
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read part", http.StatusInternalServerError)
		return
	}

	// Generate ETag (simplified - use MD5 in production)
	etag := fmt.Sprintf("\"part-%d-%d\"", partNumber, len(data))

	// Store the part
	part := &Part{
		PartNumber: partNumber,
		ETag:       etag,
		Size:       int64(len(data)),
		Data:       data,
	}

	upload.mu.Lock()
	upload.Parts[partNumber] = part
	upload.mu.Unlock()

	s.logger.Info("uploaded part",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.Int("partNumber", partNumber),
		zap.Int("size", len(data)))

	// Return ETag header
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := r.URL.Query().Get("uploadId")

	s.logger.Info("completing multipart upload",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID))

	// Find the upload session
	uploadsMu.RLock()
	upload, exists := activeUploads[uploadID]
	uploadsMu.RUnlock()

	if !exists {
		s.logger.Error("upload not found for completion", zap.String("uploadID", uploadID))
		http.Error(w, "Upload not found", http.StatusNotFound)
		return
	}

	// Sort parts by part number
	upload.mu.Lock()
	partNumbers := make([]int, 0, len(upload.Parts))
	for partNum := range upload.Parts {
		partNumbers = append(partNumbers, partNum)
	}
	sort.Ints(partNumbers)
	s.logger.Info("assembling parts in sorted order",
		zap.Ints("order", partNumbers))

	// Add detailed logging before combining parts
	s.logger.Info("parts before sorting",
		zap.Int("count", len(upload.Parts)))

	for partNum, part := range upload.Parts {
		s.logger.Info("part details",
			zap.Int("partNumber", partNum),
			zap.Int64("size", part.Size),
			zap.String("first_bytes", fmt.Sprintf("%x", part.Data[:min(16, len(part.Data))])))
	}

	// Combine all parts into final object
	var finalData bytes.Buffer
	for _, partNum := range partNumbers {
		part := upload.Parts[partNum]
		s.logger.Info("writing part",
			zap.Int("partNumber", partNum),
			zap.Int64("size", part.Size))
		finalData.Write(part.Data)
	}
	upload.mu.Unlock()

	// Get tenant from context
	tenantID := "test-tenant"
	if t := r.Context().Value(tenantIDKey); t != nil {
		if tid, ok := t.(string); ok {
			tenantID = tid
		}
	}

	// Store the complete object using your storage engine
	containerName := fmt.Sprintf("%s_%s", tenantID, bucket)
	reader := bytes.NewReader(finalData.Bytes())

	err := s.engine.Put(r.Context(), containerName, object, reader)
	if err != nil {
		s.logger.Error("failed to store multipart object",
			zap.Error(err),
			zap.String("bucket", bucket),
			zap.String("key", object))
		http.Error(w, "Failed to complete upload", http.StatusInternalServerError)
		return
	}

	// Clean up the upload session
	uploadsMu.Lock()
	delete(activeUploads, uploadID)
	uploadsMu.Unlock()

	s.logger.Info("completed multipart upload",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.Int("totalSize", finalData.Len()),
		zap.Int("parts", len(partNumbers)))

	// Return success response
	response := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult>
    <Location>http://localhost:8000/%s/%s</Location>
    <Bucket>%s</Bucket>
    <Key>%s</Key>
    <ETag>"completed-%s"</ETag>
</CompleteMultipartUploadResult>`, bucket, object, bucket, object, uploadID)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(response))
}

func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := r.URL.Query().Get("uploadId")

	uploadsMu.RLock()
	_, exists := activeUploads[uploadID]
	uploadsMu.RUnlock()

	if !exists {
		http.Error(w, "Upload not found", http.StatusNotFound)
		return
	}

	// List parts (simplified)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListPartsResult>
    <Bucket>%s</Bucket>
    <Key>%s</Key>
    <UploadId>%s</UploadId>
</ListPartsResult>`, bucket, object, uploadID)
}
