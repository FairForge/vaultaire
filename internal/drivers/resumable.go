// internal/drivers/resumable.go
package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

type ResumableUpload struct {
	backend engine.Driver
	logger  *zap.Logger
	tempDir string
}

type UploadMetadata struct {
	UploadID  string `json:"upload_id"`
	Container string `json:"container"`
	Artifact  string `json:"artifact"`
	TotalSize int64  `json:"total_size"`
	Uploaded  int64  `json:"uploaded"`
}

func NewResumableUpload(backend engine.Driver, logger *zap.Logger) *ResumableUpload {
	return &ResumableUpload{
		backend: backend,
		logger:  logger,
		tempDir: "/tmp/resumable", // In production, make this configurable
	}
}

func (r *ResumableUpload) StartUpload(ctx context.Context, uploadID, container, artifact string, totalSize int64) error {
	// Create metadata
	metadata := UploadMetadata{
		UploadID:  uploadID,
		Container: container,
		Artifact:  artifact,
		TotalSize: totalSize,
		Uploaded:  0,
	}

	// Save metadata
	metaPath := r.getMetadataPath(uploadID)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0755); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	// Create temp file for chunks
	tempPath := r.getTempPath(uploadID)
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		return fmt.Errorf("create chunks dir: %w", err)
	}
	file, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	_ = file.Close()

	r.logger.Info("started resumable upload",
		zap.String("upload_id", uploadID),
		zap.Int64("total_size", totalSize))

	return nil
}

func (r *ResumableUpload) UploadChunk(ctx context.Context, uploadID string, offset int64, data io.Reader) error {
	// Get metadata
	metadata, err := r.getMetadata(uploadID)
	if err != nil {
		return fmt.Errorf("get metadata: %w", err)
	}

	// Validate offset
	if offset != metadata.Uploaded {
		return fmt.Errorf("invalid offset: expected %d, got %d", metadata.Uploaded, offset)
	}

	// Write chunk to temp file
	tempPath := r.getTempPath(uploadID)
	file, err := os.OpenFile(tempPath, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Seek to offset
	if _, err := file.Seek(offset, 0); err != nil {
		return fmt.Errorf("seek to offset %d: %w", offset, err)
	}

	// Write data
	written, err := io.Copy(file, data)
	if err != nil {
		return fmt.Errorf("write chunk: %w", err)
	}

	// Update metadata
	metadata.Uploaded = offset + written
	if err := r.saveMetadata(metadata); err != nil {
		return fmt.Errorf("update metadata: %w", err)
	}

	r.logger.Debug("uploaded chunk",
		zap.String("upload_id", uploadID),
		zap.Int64("offset", offset),
		zap.Int64("size", written),
		zap.Int64("total_uploaded", metadata.Uploaded))

	return nil
}

func (r *ResumableUpload) GetUploadOffset(ctx context.Context, uploadID string) (int64, error) {
	metadata, err := r.getMetadata(uploadID)
	if err != nil {
		return 0, fmt.Errorf("get metadata: %w", err)
	}
	return metadata.Uploaded, nil
}

func (r *ResumableUpload) CompleteUpload(ctx context.Context, uploadID string) error {
	metadata, err := r.getMetadata(uploadID)
	if err != nil {
		return fmt.Errorf("get metadata: %w", err)
	}

	// Verify upload is complete
	if metadata.Uploaded != metadata.TotalSize {
		return fmt.Errorf("upload incomplete: %d/%d bytes", metadata.Uploaded, metadata.TotalSize)
	}

	// Move temp file to final location
	tempPath := r.getTempPath(uploadID)
	file, err := os.Open(tempPath)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Upload to backend
	if err := r.backend.Put(ctx, metadata.Container, metadata.Artifact, file); err != nil {
		return fmt.Errorf("put to backend: %w", err)
	}

	// Clean up
	_ = os.Remove(tempPath)
	_ = os.Remove(r.getMetadataPath(uploadID))

	r.logger.Info("completed resumable upload",
		zap.String("upload_id", uploadID),
		zap.String("artifact", metadata.Artifact))

	return nil
}

func (r *ResumableUpload) getMetadataPath(uploadID string) string {
	return filepath.Join(r.tempDir, "metadata", uploadID+".json")
}

func (r *ResumableUpload) getTempPath(uploadID string) string {
	return filepath.Join(r.tempDir, "chunks", uploadID+".tmp")
}

func (r *ResumableUpload) getMetadata(uploadID string) (*UploadMetadata, error) {
	data, err := os.ReadFile(r.getMetadataPath(uploadID))
	if err != nil {
		return nil, err
	}

	var metadata UploadMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

func (r *ResumableUpload) saveMetadata(metadata *UploadMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return os.WriteFile(r.getMetadataPath(metadata.UploadID), data, 0644)
}
