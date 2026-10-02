package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/xml"
	"errors"
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
	if errors.Is(err, sql.ErrNoRows) {
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
		WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
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
	// writer delivers the report through the customer write path (Review
	// R13-02); nil = reports are not delivered.
	writer *generatedObjectWriter
	// ReportDeadline bounds ONE report (Review R13-23: every tenant's
	// reports used to share one 5-minute budget, so a large bucket early in
	// the list starved the rest).
	ReportDeadline time.Duration
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
	// onlyTenant is a test hook: when set, a run skips every other tenant's
	// configurations. Packages share one test database. Never set in
	// production.
	onlyTenant string
}

// inventoryJobName is the job's name in job_runs and on the metrics.
const inventoryJobName = "inventory"

// InventoryResult is one run's outcome.
type InventoryResult struct {
	// Reports written, reports that could not be written, configurations
	// not due today (weekly, and it is not Sunday).
	Written, Failed, Skipped int
	Errors                   []string
}

func NewInventoryRunner(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *InventoryRunner {
	if db == nil || eng == nil {
		return nil
	}
	return &InventoryRunner{db: db, eng: eng, logger: logger, ReportDeadline: 30 * time.Minute, JobName: inventoryJobName}
}

// SetWriter wires the delivery writer.
func (ir *InventoryRunner) SetWriter(w *generatedObjectWriter) {
	if ir != nil {
		ir.writer = w
	}
}

// spec is the job's schedule: daily at 00:30 UTC, catch-up four minutes after
// boot. It used to be an hourly tick that acted only when the wall clock's
// hour was 0 (Review R13-07/R13-23): a restart during hour 0 skipped the
// day's reports, or wrote them twice. The day is now decided by job_runs.
//
// What makes a run fail: the list of inventory configurations could not be
// read — retried at the next hourly check. A report that could not be
// written (target bucket gone, over quota, its deadline) does NOT: it is
// counted in the note and the other buckets' reports are written; the next
// day's run writes that bucket's next report.
func (ir *InventoryRunner) spec(sched *jobScheduler) jobSpec {
	sp := jobSpec{Name: ir.JobName, Hour: 0, Minute: 30,
		BootDelay: 4 * time.Minute, MaxRunTime: 6 * time.Hour}
	sp.Run = func(ctx context.Context) (jobReport, error) {
		// The report is dated on the day it was owed: a catch-up after a
		// restart writes the report of the scheduled day, and "weekly" means
		// the run scheduled on a Sunday.
		day := (&scheduledJob{spec: sp}).lastScheduled(sched.now())
		res, err := ir.RunOnce(ctx, day)
		rep := jobReport{Rows: int64(res.Written)}
		if res.Failed > 0 {
			rep.Note = fmt.Sprintf("%d report(s) not written, first: %s", res.Failed, res.Errors[0])
		}
		return rep, err
	}
	return sp
}

// RunOnce writes the reports owed for day (UTC): every daily configuration,
// and the weekly ones when day is a Sunday.
func (ir *InventoryRunner) RunOnce(ctx context.Context, day time.Time) (InventoryResult, error) {
	var res InventoryResult
	if ir == nil {
		return res, errors.New("inventory: runner not configured")
	}
	day = day.UTC()

	rows, err := ir.db.QueryContext(ctx, `
		SELECT tenant_id, name, inventory_schedule, inventory_target_bucket,
			inventory_prefix, inventory_format
		FROM buckets
		WHERE inventory_enabled = TRUE AND inventory_target_bucket IS NOT NULL
		ORDER BY tenant_id, name
	`)
	if err != nil {
		return res, fmt.Errorf("inventory: list configurations: %w", err)
	}

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
			_ = rows.Close()
			return res, fmt.Errorf("inventory: scan configuration: %w", err)
		}
		configs = append(configs, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("inventory: iterate configurations: %w", err)
	}

	for _, c := range configs {
		if ir.onlyTenant != "" && c.tenantID != ir.onlyTenant {
			continue
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		// Weekly reports belong to the run scheduled on a Sunday.
		if c.schedule == "weekly" && day.Weekday() != time.Sunday {
			res.Skipped++
			continue
		}
		rctx, cancel := ctx, context.CancelFunc(func() {})
		if ir.ReportDeadline > 0 {
			rctx, cancel = context.WithTimeout(ctx, ir.ReportDeadline)
		}
		err := ir.generateReport(rctx, c.tenantID, c.bucket, c.targetBucket, c.prefix, c.format, day)
		cancel()
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s/%s: %v", c.tenantID, c.bucket, err))
			ir.logger.Warn("inventory report not delivered",
				zap.String("tenant_id", c.tenantID), zap.String("bucket", c.bucket),
				zap.String("target", c.targetBucket), zap.Error(err))
			continue
		}
		res.Written++
	}
	return res, nil
}

// generateReport streams the bucket's head rows as CSV into
// {prefix}{bucket}/{date}T00-00Z/manifest.csv of the target bucket through
// the customer write path (Review R13-02). date is the day the report is
// for. An empty bucket writes nothing (and is not an error).
func (ir *InventoryRunner) generateReport(ctx context.Context, tenantID, bucket, targetBucket, prefix, format string, date time.Time) error {
	_ = format // ORC/Parquet are accepted at config time and written as CSV (R4-18)
	if ir.writer == nil {
		return errors.New("no delivery writer")
	}
	var count int64
	if err := ir.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, tenantID, bucket).Scan(&count); err != nil {
		return fmt.Errorf("count objects: %w", err)
	}
	if count == 0 {
		return nil
	}

	objectKey := fmt.Sprintf("%s%s/%sT00-00Z/manifest.csv", prefix, bucket, date.UTC().Format("2006-01-02"))

	var written int
	_, err := ir.writer.write(ctx, tenantID, targetBucket, objectKey, "text/csv", func(out io.Writer) error {
		rows, err := ir.db.QueryContext(ctx, `
			SELECT object_key, size_bytes, etag, content_type, updated_at,
				COALESCE(encryption_algorithm, ''), COALESCE(backend_name, ''), floor
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2
			ORDER BY object_key ASC
		`, tenantID, bucket)
		if err != nil {
			return fmt.Errorf("query objects for inventory: %w", err)
		}
		defer func() { _ = rows.Close() }()

		w := csv.NewWriter(out)
		// The last column is the class the customer sees — what ListObjects
		// and HEAD say (WP-R13-1). It used to be BackendName: our internal
		// driver name, and `geyser` for a Smart-demoted downstairs object.
		if err := w.Write([]string{"Key", "SizeBytes", "ETag", "ContentType", "LastModified", "EncryptionAlgorithm", "StorageClass"}); err != nil {
			return err
		}
		for rows.Next() {
			var key, etag, contentType, encAlgo, backendName, floor string
			var sizeBytes int64
			var updatedAt time.Time
			if err := rows.Scan(&key, &sizeBytes, &etag, &contentType, &updatedAt, &encAlgo, &backendName, &floor); err != nil {
				return fmt.Errorf("scan inventory row: %w", err)
			}
			if err := w.Write([]string{
				key, fmt.Sprintf("%d", sizeBytes), etag, contentType,
				updatedAt.UTC().Format(time.RFC3339), encAlgo, engine.CustomerStorageClass(floor, backendName),
			}); err != nil {
				return err
			}
			written++
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate inventory rows: %w", err)
		}
		w.Flush()
		return w.Error()
	})
	if err != nil {
		return fmt.Errorf("deliver %s/%s: %w", targetBucket, objectKey, err)
	}

	ir.logger.Info("inventory report generated",
		zap.String("tenant_id", tenantID),
		zap.String("bucket", bucket),
		zap.String("target", targetBucket+"/"+objectKey),
		zap.Int("objects", written))
	return nil
}

// GenerateReportNow is exposed for testing — generates today's report
// immediately.
func (ir *InventoryRunner) GenerateReportNow(ctx context.Context, tenantID, bucket, targetBucket, prefix, format string) {
	if ir == nil {
		return
	}
	if err := ir.generateReport(ctx, tenantID, bucket, targetBucket, prefix, format, time.Now()); err != nil {
		ir.logger.Warn("inventory report not delivered",
			zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.Error(err))
	}
}
