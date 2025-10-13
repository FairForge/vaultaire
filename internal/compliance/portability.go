package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PortabilityService handles GDPR Article 20 - Right to Data Portability
type PortabilityService struct {
	db      PortabilityDatabase
	storage StorageProvider
	logger  *zap.Logger
}

// StorageProvider defines the storage operations needed for portability
type StorageProvider interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	GeneratePresignedURL(ctx context.Context, key string, duration time.Duration) (string, error)
}

// NewPortabilityService creates a new portability service
func NewPortabilityService(db PortabilityDatabase, storage StorageProvider, logger *zap.Logger) *PortabilityService {
	return &PortabilityService{
		db:      db,
		storage: storage,
		logger:  logger,
	}
}

// CreateExportRequest creates a new data export request
func (p *PortabilityService) CreateExportRequest(ctx context.Context, userID uuid.UUID, format string) (*PortabilityRequest, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("user_id required")
	}

	validFormats := map[string]bool{
		"json":    true,
		"archive": true,
		"s3":      true,
	}
	if !validFormats[format] {
		return nil, fmt.Errorf("invalid format: %s", format)
	}

	request := &PortabilityRequest{
		ID:          uuid.New(),
		UserID:      userID,
		RequestType: "export",
		Status:      StatusPending,
		Format:      format,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(7 * 24 * time.Hour), // 7 days
	}

	if p.db != nil {
		if err := p.db.CreatePortabilityRequest(ctx, request); err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
	}

	// Make a copy for async processing to avoid race conditions
	requestCopy := *request

	// Start async processing with copy
	go p.processExportRequest(context.Background(), &requestCopy)

	// Return original request (which won't be modified by goroutine)
	return request, nil
}

// processExportRequest processes an export request asynchronously
func (p *PortabilityService) processExportRequest(ctx context.Context, request *PortabilityRequest) {
	p.logger.Info("processing export request",
		zap.String("request_id", request.ID.String()),
		zap.String("format", request.Format))

	// Update status to processing
	request.Status = StatusProcessing
	if p.db != nil {
		if err := p.db.UpdatePortabilityRequest(ctx, request); err != nil {
			p.logger.Error("failed to update request status", zap.Error(err))
		}
	}

	var exportURL string
	var err error

	switch request.Format {
	case "json":
		exportURL, err = p.exportJSON(ctx, request.UserID)
	case "archive":
		exportURL, err = p.exportArchive(ctx, request.UserID)
	case "s3":
		exportURL, err = p.exportS3Credentials(ctx, request.UserID)
	}

	if err != nil {
		p.logger.Error("export failed", zap.Error(err))
		request.Status = StatusFailed
		request.Metadata = map[string]interface{}{
			"error": err.Error(),
		}
	} else {
		request.Status = StatusReady
		request.ExportURL = exportURL
		request.CompletedAt = time.Now()
	}

	if p.db != nil {
		if err := p.db.UpdatePortabilityRequest(ctx, request); err != nil {
			p.logger.Error("failed to update request status", zap.Error(err))
		}
	}
}

// exportJSON exports all user data as JSON
func (p *PortabilityService) exportJSON(ctx context.Context, userID uuid.UUID) (string, error) {
	export := &UserDataExport{
		ExportDate: time.Now(),
		Format:     "json",
		Version:    "1.0",
	}

	// Export personal data
	if p.db != nil {
		user, err := p.db.GetUser(ctx, userID)
		if err == nil {
			export.PersonalData = &PersonalData{
				UserID:    user.ID,
				Email:     user.Email,
				Name:      user.Name,
				CreatedAt: user.CreatedAt,
			}
		}

		// Export API keys (masked)
		keys, err := p.db.ListAPIKeys(ctx, userID)
		if err == nil {
			for _, key := range keys {
				export.APIKeys = append(export.APIKeys, APIKeyExport{
					ID:        key.ID,
					Name:      key.Name,
					Masked:    maskKey(key.Key),
					CreatedAt: key.CreatedAt,
				})
			}
		}

		// Export usage records
		usage, err := p.db.GetUsageRecords(ctx, userID)
		if err == nil {
			export.UsageRecords = usage
		}

		// Export file metadata (not files themselves)
		files, err := p.db.ListFiles(ctx, userID)
		if err == nil {
			for _, file := range files {
				export.Files = append(export.Files, FileMetadataExport{
					Path:        file.Path,
					Size:        file.Size,
					Uploaded:    file.CreatedAt,
					Modified:    file.ModifiedAt,
					Container:   file.Container,
					ContentType: file.ContentType,
				})
			}
		}

		// Export containers
		containers, err := p.db.ListContainers(ctx, userID)
		if err == nil {
			for _, container := range containers {
				export.Containers = append(export.Containers, ContainerExport{
					Name:      container.Name,
					CreatedAt: container.CreatedAt,
				})
			}
		}
	}

	// Generate JSON
	jsonData, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Upload to temporary location
	exportKey := fmt.Sprintf("exports/%s/%s.json", userID, uuid.New())
	if p.storage != nil {
		if err := p.storage.Put(ctx, exportKey, jsonData); err != nil {
			return "", fmt.Errorf("failed to upload export: %w", err)
		}

		// Generate pre-signed URL (valid for 7 days)
		url, err := p.storage.GeneratePresignedURL(ctx, exportKey, 7*24*time.Hour)
		if err != nil {
			return "", fmt.Errorf("failed to generate URL: %w", err)
		}
		return url, nil
	}

	return "", nil
}

// exportArchive exports all files as a zip archive
func (p *PortabilityService) exportArchive(ctx context.Context, userID uuid.UUID) (string, error) {
	// This would create a zip file containing all user's files
	// For MVP, we can skip this or implement simplified version
	return "", fmt.Errorf("archive export not yet implemented")
}

// exportS3Credentials generates temporary S3 credentials for direct transfer
func (p *PortabilityService) exportS3Credentials(ctx context.Context, userID uuid.UUID) (string, error) {
	// Generate temporary S3 access
	credentials := S3Credentials{
		AccessKey:  generateTempAccessKey(),
		SecretKey:  generateTempSecretKey(),
		Endpoint:   "s3.stored.ge",
		Bucket:     fmt.Sprintf("user-%s", userID),
		ValidUntil: time.Now().Add(7 * 24 * time.Hour),
	}

	// Store credentials temporarily
	credsJSON, err := json.Marshal(credentials)
	if err != nil {
		return "", err
	}

	credKey := fmt.Sprintf("temp-creds/%s.json", uuid.New())
	if p.storage != nil {
		if err := p.storage.Put(ctx, credKey, credsJSON); err != nil {
			return "", err
		}

		return p.storage.GeneratePresignedURL(ctx, credKey, 7*24*time.Hour)
	}

	return "", nil
}

// GetExportRequest retrieves an export request status
func (p *PortabilityService) GetExportRequest(ctx context.Context, requestID uuid.UUID) (*PortabilityRequest, error) {
	if p.db == nil {
		return nil, fmt.Errorf("database not configured")
	}

	return p.db.GetPortabilityRequest(ctx, requestID)
}

// Helper functions
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

func generateTempAccessKey() string {
	return fmt.Sprintf("TEMP%s", uuid.New().String()[:16])
}

func generateTempSecretKey() string {
	return uuid.New().String()
}
