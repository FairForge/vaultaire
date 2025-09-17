package api

import (
	"context"
	"crypto/md5"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"

	"go.uber.org/zap"
)

// MultipartUpload tracks an in-progress multipart upload
type MultipartUpload struct {
	UploadID string
	Bucket   string
	Key      string
	TenantID string
	Parts    map[int]*Part
	mu       sync.Mutex

	// Idempotency fields
	Completed     bool
	CompletedETag string
	CompletedAt   time.Time
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

	// Get tenant from context
	tenantID := "default"

	// DEBUG: Check what's actually in the context
	ctxValue := r.Context().Value(common.TenantIDKey)
	s.logger.Info("multipart init context check",
		zap.Any("raw_value", ctxValue),
		zap.String("type", fmt.Sprintf("%T", ctxValue)))

	if t := r.Context().Value(common.TenantIDKey); t != nil {
		if tid, ok := t.(string); ok {
			tenantID = tid
			s.logger.Info("extracted tenant", zap.String("tenant", tenantID))
		} else {
			s.logger.Info("failed to cast tenant", zap.Any("value", t))
		}
	} else {
		s.logger.Info("no tenant in context")
	}

	// Store the upload session
	upload := &MultipartUpload{
		UploadID: uploadID,
		Bucket:   bucket,
		Key:      object,
		TenantID: tenantID,
		Parts:    make(map[int]*Part),
	}

	uploadsMu.Lock()
	activeUploads[uploadID] = upload
	uploadsMu.Unlock()

	s.logger.Info("initiated multipart upload",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID))

	response := InitiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      object,
		UploadID: uploadID,
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("failed to encode initiate response", zap.Error(err))
		return
	}
}

func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := r.URL.Query().Get("uploadId")
	partNumberStr := r.URL.Query().Get("partNumber")

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

	// Generate ETag using MD5
	hash := md5.Sum(data)
	etag := fmt.Sprintf("\"%x\"", hash)

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

	s.logger.Debug("uploaded part",
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
	startTime := time.Now()
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

	// Check if already completed (idempotency)
	upload.mu.Lock()
	if upload.Completed {
		upload.mu.Unlock()
		s.logger.Info("upload already completed, returning cached response",
			zap.String("uploadID", uploadID))

		// Return the same success response
		location := fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object)
		response := CompleteMultipartUploadResult{
			Location: location,
			Bucket:   bucket,
			Key:      object,
			ETag:     upload.CompletedETag,
		}
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(response); err != nil {
			s.logger.Error("failed to encode cached response", zap.Error(err))
			return
		}
		return
	}
	upload.mu.Unlock()

	// Parse the request body to get part list (AWS sends this)
	var completeRequest CompleteMultipartUploadRequest
	if r.Body != nil {
		if err := xml.NewDecoder(r.Body).Decode(&completeRequest); err != nil {
			s.logger.Debug("no complete request body, using all parts",
				zap.Error(err))
		}
	}

	// Sort parts by part number
	upload.mu.Lock()
	partNumbers := make([]int, 0, len(upload.Parts))
	totalSize := int64(0)
	for partNum := range upload.Parts {
		partNumbers = append(partNumbers, partNum)
		totalSize += upload.Parts[partNum].Size
	}
	sort.Ints(partNumbers)

	// Copy parts for assembly
	partsToAssemble := make([]*Part, len(partNumbers))
	for i, partNum := range partNumbers {
		partsToAssemble[i] = upload.Parts[partNum]
	}
	upload.mu.Unlock()

	// Use streaming assembly with pipe
	pr, pw := io.Pipe()

	// IMPORTANT FIX: Use just the bucket name, not tenant_bucket
	// The engine/driver will handle tenant isolation internally
	containerName := fmt.Sprintf("%s_%s", upload.TenantID, bucket)

	var assemblyErr error
	var uploadErr error
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: Write parts to pipe
	go func() {
		defer wg.Done()
		defer func() {
			if err := pw.Close(); err != nil {
				s.logger.Error("failed to close pipe writer", zap.Error(err))
			}
		}()

		for i, part := range partsToAssemble {
			if _, err := pw.Write(part.Data); err != nil {
				assemblyErr = fmt.Errorf("failed to write part %d: %w", partNumbers[i], err)
				pw.CloseWithError(assemblyErr)
				return
			}
		}

		s.logger.Info("parts assembled",
			zap.Int64("total_size", totalSize),
			zap.Int("parts", len(partNumbers)))
	}()

	// Goroutine 2: Upload from pipe to backend
	go func() {
		defer wg.Done()
		defer func() {
			if err := pr.Close(); err != nil {
				s.logger.Error("failed to close pipe reader", zap.Error(err))
			}
		}()

		// Create context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		// Add tenant to context for the engine
		ctx = context.WithValue(ctx, common.TenantIDKey, upload.TenantID)

		// Store to backend
		uploadErr = s.engine.Put(ctx, containerName, object, pr)

		if uploadErr != nil {
			s.logger.Error("failed to store to backend",
				zap.Error(uploadErr),
				zap.String("container", containerName),
				zap.String("key", object))
		} else {
			s.logger.Info("stored to backend successfully",
				zap.String("container", containerName),
				zap.String("key", object),
				zap.Int64("size", totalSize))
		}
	}()

	// Wait for both operations to complete
	wg.Wait()

	// Check for errors
	if assemblyErr != nil {
		s.logger.Error("assembly failed",
			zap.Error(assemblyErr),
			zap.String("bucket", bucket),
			zap.String("key", object))
		http.Error(w, "Failed to assemble upload", http.StatusInternalServerError)
		return
	}

	if uploadErr != nil {
		s.logger.Error("storage failed",
			zap.Error(uploadErr),
			zap.String("bucket", bucket),
			zap.String("key", object))
		http.Error(w, "Failed to store upload", http.StatusInternalServerError)
		return
	}

	// Generate final ETag
	finalETag := fmt.Sprintf("\"multipart-%d\"", len(partNumbers))

	// Mark as completed
	upload.mu.Lock()
	upload.Completed = true
	upload.CompletedETag = finalETag
	upload.CompletedAt = time.Now()
	upload.mu.Unlock()

	// Schedule cleanup
	go func() {
		time.Sleep(5 * time.Minute)
		uploadsMu.Lock()
		delete(activeUploads, uploadID)
		uploadsMu.Unlock()
		s.logger.Debug("cleaned up completed upload", zap.String("uploadID", uploadID))
	}()

	s.logger.Info("multipart upload completed",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.Int64("totalSize", totalSize),
		zap.Int("parts", len(partNumbers)),
		zap.Duration("total_time", time.Since(startTime)))

	// Return success response
	location := fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object)
	response := CompleteMultipartUploadResult{
		Location: location,
		Bucket:   bucket,
		Key:      object,
		ETag:     finalETag,
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("failed to encode complete response", zap.Error(err))
		return
	}
}

func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := r.URL.Query().Get("uploadId")

	// Log the abort operation
	s.logger.Info("aborting multipart upload",
		zap.String("bucket", bucket),
		zap.String("object", object),
		zap.String("uploadID", uploadID))

	// Clean up the upload session
	uploadsMu.Lock()
	delete(activeUploads, uploadID)
	uploadsMu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := r.URL.Query().Get("uploadId")

	uploadsMu.RLock()
	upload, exists := activeUploads[uploadID]
	uploadsMu.RUnlock()

	if !exists {
		http.Error(w, "Upload not found", http.StatusNotFound)
		return
	}

	// Build parts list
	upload.mu.Lock()
	parts := make([]ListPartItem, 0, len(upload.Parts))
	for _, part := range upload.Parts {
		parts = append(parts, ListPartItem{
			PartNumber:   part.PartNumber,
			ETag:         part.ETag,
			Size:         part.Size,
			LastModified: time.Now().Format(time.RFC3339),
		})
	}
	upload.mu.Unlock()

	// Sort by part number
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	response := ListPartsResult{
		Bucket:   bucket,
		Key:      object,
		UploadID: uploadID,
		Parts:    parts,
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("failed to encode list parts response", zap.Error(err))
		return
	}
}

// XML structures for requests and responses
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
