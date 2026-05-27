package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const maxInventoryBodyBytes = 4096

// InventoryConfiguration is the S3 XML type for inventory config.
type InventoryConfiguration struct {
	XMLName     xml.Name              `xml:"InventoryConfiguration"`
	Xmlns       string                `xml:"xmlns,attr,omitempty"`
	ID          string                `xml:"Id,omitempty"`
	IsEnabled   bool                  `xml:"IsEnabled"`
	Schedule    *InventorySchedule    `xml:"Schedule,omitempty"`
	Destination *InventoryDestination `xml:"Destination,omitempty"`
	Format      string                `xml:"Format,omitempty"`
}

type InventorySchedule struct {
	Frequency string `xml:"Frequency"`
}

type InventoryDestination struct {
	S3BucketDestination *S3BucketDestination `xml:"S3BucketDestination,omitempty"`
}

type S3BucketDestination struct {
	Bucket string `xml:"Bucket"`
	Prefix string `xml:"Prefix,omitempty"`
	Format string `xml:"Format,omitempty"`
}

func (s *Server) handleGetBucketInventory(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(xml.Header))
		_, _ = w.Write([]byte(`<InventoryConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsEnabled>false</IsEnabled></InventoryConfiguration>`))
		return
	}

	var enabled bool
	var schedule string
	var targetBucket, prefix, format sql.NullString
	err = s.db.QueryRowContext(r.Context(),
		`SELECT inventory_enabled, inventory_schedule, inventory_target_bucket, inventory_prefix, inventory_format
		 FROM buckets WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket).Scan(&enabled, &schedule, &targetBucket, &prefix, &format)
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
		s.logger.Error("query bucket inventory config", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	resp := InventoryConfiguration{
		Xmlns:     "http://s3.amazonaws.com/doc/2006-03-01/",
		ID:        req.Bucket,
		IsEnabled: enabled,
	}
	if enabled && targetBucket.Valid && targetBucket.String != "" {
		resp.Schedule = &InventorySchedule{Frequency: schedule}
		resp.Format = format.String
		resp.Destination = &InventoryDestination{
			S3BucketDestination: &S3BucketDestination{
				Bucket: targetBucket.String,
				Prefix: prefix.String,
				Format: format.String,
			},
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutBucketInventory(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInventoryBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config InventoryConfiguration
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	// Verify source bucket exists.
	var exists bool
	err = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
		t.ID, req.Bucket).Scan(&exists)
	if err != nil || !exists {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}

	if !config.IsEnabled || config.Destination == nil || config.Destination.S3BucketDestination == nil {
		// Disable inventory.
		_, err = s.db.ExecContext(r.Context(),
			`UPDATE buckets SET inventory_enabled = FALSE, inventory_target_bucket = NULL, inventory_prefix = '', updated_at = NOW()
			 WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket)
		if err != nil {
			s.logger.Error("disable bucket inventory", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	dest := config.Destination.S3BucketDestination
	targetBucket := dest.Bucket

	// Validate target bucket exists and belongs to same tenant.
	err = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
		t.ID, targetBucket).Scan(&exists)
	if err != nil || !exists {
		WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, generateRequestID(),
			WithSuggestion("Target bucket does not exist or belongs to a different account."))
		return
	}

	schedule := "daily"
	if config.Schedule != nil && config.Schedule.Frequency != "" {
		freq := strings.ToLower(config.Schedule.Frequency)
		if freq == "daily" || freq == "weekly" {
			schedule = freq
		}
	}

	inventoryFormat := "csv"
	if dest.Format != "" {
		f := strings.ToLower(dest.Format)
		if f == "csv" || f == "orc" || f == "parquet" {
			inventoryFormat = f
		}
	}

	_, err = s.db.ExecContext(r.Context(),
		`UPDATE buckets SET inventory_enabled = TRUE, inventory_schedule = $3,
			inventory_target_bucket = $4, inventory_prefix = $5, inventory_format = $6, updated_at = NOW()
		 WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket, schedule, targetBucket, dest.Prefix, inventoryFormat)
	if err != nil {
		s.logger.Error("enable bucket inventory", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("bucket inventory enabled",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.String("target_bucket", targetBucket),
		zap.String("schedule", schedule),
		zap.String("format", inventoryFormat))

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteBucketInventory(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	result, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET inventory_enabled = FALSE, inventory_target_bucket = NULL,
			inventory_prefix = '', updated_at = NOW()
		 WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("delete bucket inventory", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}

	s.logger.Info("bucket inventory deleted",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket))

	w.WriteHeader(http.StatusNoContent)
}

// InventoryRunner generates inventory reports for buckets that have inventory enabled.
type InventoryRunner struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	logger *zap.Logger
}

func NewInventoryRunner(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *InventoryRunner {
	if db == nil || eng == nil {
		return nil
	}
	return &InventoryRunner{db: db, eng: eng, logger: logger}
}

// StartInventoryJob runs a background goroutine that generates inventory reports.
// Checks once per hour whether any inventory reports are due.
func (ir *InventoryRunner) StartInventoryJob(ctx context.Context) {
	if ir == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ir.runInventory(ctx)
			}
		}
	}()
}

func (ir *InventoryRunner) runInventory(ctx context.Context) {
	now := time.Now().UTC()

	// Only run at midnight UTC (hour 0).
	if now.Hour() != 0 {
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	rows, err := ir.db.QueryContext(runCtx, `
		SELECT tenant_id, name, inventory_schedule, inventory_target_bucket,
			inventory_prefix, inventory_format
		FROM buckets
		WHERE inventory_enabled = TRUE AND inventory_target_bucket IS NOT NULL
	`)
	if err != nil {
		ir.logger.Error("query inventory-enabled buckets", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	type invConfig struct {
		tenantID     string
		bucket       string
		schedule     string
		targetBucket string
		prefix       string
		format       string
	}
	var configs []invConfig
	for rows.Next() {
		var c invConfig
		if err := rows.Scan(&c.tenantID, &c.bucket, &c.schedule, &c.targetBucket, &c.prefix, &c.format); err != nil {
			ir.logger.Error("scan inventory config", zap.Error(err))
			continue
		}
		configs = append(configs, c)
	}

	for _, c := range configs {
		// Weekly runs only on Sunday.
		if c.schedule == "weekly" && now.Weekday() != time.Sunday {
			continue
		}
		ir.generateReport(runCtx, c.tenantID, c.bucket, c.targetBucket, c.prefix, c.format)
	}
}

func (ir *InventoryRunner) generateReport(ctx context.Context, tenantID, bucket, targetBucket, prefix, format string) {
	rows, err := ir.db.QueryContext(ctx, `
		SELECT object_key, size_bytes, etag, content_type, updated_at,
			COALESCE(encryption_algorithm, ''), COALESCE(backend_name, '')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2
		ORDER BY object_key ASC
	`, tenantID, bucket)
	if err != nil {
		ir.logger.Error("query objects for inventory",
			zap.String("bucket", bucket), zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	_ = w.Write([]string{"Key", "SizeBytes", "ETag", "ContentType", "LastModified", "EncryptionAlgorithm", "BackendName"})

	var count int
	for rows.Next() {
		var key, etag, contentType, encAlgo, backendName string
		var sizeBytes int64
		var updatedAt time.Time
		if err := rows.Scan(&key, &sizeBytes, &etag, &contentType, &updatedAt, &encAlgo, &backendName); err != nil {
			ir.logger.Error("scan inventory row", zap.Error(err))
			continue
		}
		_ = w.Write([]string{
			key,
			fmt.Sprintf("%d", sizeBytes),
			etag,
			contentType,
			updatedAt.UTC().Format(time.RFC3339),
			encAlgo,
			backendName,
		})
		count++
	}
	w.Flush()

	if count == 0 {
		return
	}

	now := time.Now().UTC()
	objectKey := fmt.Sprintf("%s%s/%sT00-00Z/manifest.csv",
		prefix, bucket, now.Format("2006-01-02"))

	container := fmt.Sprintf("tenant/%s/%s", tenantID, targetBucket)

	if _, err := ir.eng.Put(ctx, container, objectKey, strings.NewReader(buf.String())); err != nil {
		ir.logger.Error("write inventory report",
			zap.String("tenant_id", tenantID),
			zap.String("target", targetBucket+"/"+objectKey),
			zap.Error(err))
		return
	}

	ir.logger.Info("inventory report generated",
		zap.String("tenant_id", tenantID),
		zap.String("bucket", bucket),
		zap.String("target", targetBucket+"/"+objectKey),
		zap.Int("objects", count))
}

// GenerateReportNow is exposed for testing — generates a report immediately.
func (ir *InventoryRunner) GenerateReportNow(ctx context.Context, tenantID, bucket, targetBucket, prefix, format string) {
	if ir == nil {
		return
	}
	ir.generateReport(ctx, tenantID, bucket, targetBucket, prefix, format)
}
