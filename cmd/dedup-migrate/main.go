package main

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G401 — S3 spec requires MD5
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/database"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

// Result holds the outcome of migrating a single object.
type Result struct {
	TenantID    string
	Bucket      string
	Key         string
	SizeBytes   int64
	ChunkCount  int
	PhysicalNew int64 // bytes of newly-stored chunks
	Saved       int64 // logical − physicalNew (dedup savings)
	Skipped     bool
	Failed      bool
	FailReason  string
}

// deps bundles the dependencies migrateObject needs.
type deps struct {
	eng *engine.CoreEngine
	gci *crypto.GlobalContentIndex
	db  *sql.DB
	log *zap.Logger
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report savings without writing")
	minSize := flag.Int64("min-size", 64<<20, "minimum object size in bytes")
	tenantFilter := flag.String("tenant", "", "filter by tenant_id (optional)")
	bucketFilter := flag.String("bucket", "", "filter by bucket (optional)")
	limit := flag.Int("limit", 0, "max objects to process (0=all)")
	keepOriginal := flag.Bool("keep-original", false, "do not delete the monolithic copy after migration")
	flag.Parse()

	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, eng := bootstrap(logger)
	defer func() { _ = db.Close() }()

	gci := crypto.NewGlobalContentIndex(db)

	d := &deps{eng: eng, gci: gci, db: db, log: logger}

	candidates, err := selectCandidates(ctx, db, *minSize, *tenantFilter, *bucketFilter, *limit)
	if err != nil {
		logger.Fatal("select candidates", zap.Error(err))
	}
	logger.Info("candidates selected", zap.Int("count", len(candidates)))

	var totalLogical, totalPhysical, totalSaved int64
	var migrated, skipped, failed int

	for i, c := range candidates {
		res, migErr := migrateObject(ctx, d, c, *dryRun, *keepOriginal)
		if migErr != nil {
			logger.Error("migrate object", zap.String("key", c.key), zap.Error(migErr))
			failed++
			continue
		}
		if res.Skipped {
			skipped++
			continue
		}
		if res.Failed {
			logger.Warn("object failed verification",
				zap.String("key", c.key),
				zap.String("reason", res.FailReason))
			failed++
			continue
		}

		migrated++
		totalLogical += res.SizeBytes
		totalPhysical += res.PhysicalNew
		totalSaved += res.Saved

		mode := "LIVE"
		if *dryRun {
			mode = "DRY-RUN"
		}
		logger.Info(fmt.Sprintf("[%s] %d/%d", mode, i+1, len(candidates)),
			zap.String("tenant", res.TenantID),
			zap.String("bucket", res.Bucket),
			zap.String("key", res.Key),
			zap.Int64("size", res.SizeBytes),
			zap.Int("chunks", res.ChunkCount),
			zap.Int64("new_bytes", res.PhysicalNew),
			zap.Int64("saved", res.Saved))
	}

	logger.Info("migration complete",
		zap.Int("migrated", migrated),
		zap.Int("skipped", skipped),
		zap.Int("failed", failed),
		zap.Int64("logical_bytes", totalLogical),
		zap.Int64("physical_bytes", totalPhysical),
		zap.Int64("saved_bytes", totalSaved))
}

type candidate struct {
	tenantID string
	bucket   string
	key      string
	size     int64
	etag     string
}

func selectCandidates(ctx context.Context, db *sql.DB, minSize int64, tenantFilter, bucketFilter string, limit int) ([]candidate, error) {
	query := `
		SELECT tenant_id, bucket, object_key, size_bytes, etag
		FROM object_head_cache
		WHERE is_chunked = FALSE
		  AND encryption_algorithm = ''
		  AND size_bytes >= $1`
	args := []interface{}{minSize}
	argN := 2

	if tenantFilter != "" {
		query += fmt.Sprintf(" AND tenant_id = $%d", argN)
		args = append(args, tenantFilter)
		argN++
	}
	if bucketFilter != "" {
		query += fmt.Sprintf(" AND bucket = $%d", argN)
		args = append(args, bucketFilter)
		argN++
	}

	query += " ORDER BY size_bytes DESC"

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argN)
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.tenantID, &c.bucket, &c.key, &c.size, &c.etag); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func migrateObject(ctx context.Context, d *deps, c candidate, dryRun, keepOriginal bool) (*Result, error) {
	tenantUUID, err := uuid.Parse(c.tenantID)
	if err != nil {
		return &Result{Skipped: true, FailReason: "non-UUID tenant"}, nil
	}

	t := &tenant.Tenant{ID: c.tenantID}
	container := t.NamespaceContainer(c.bucket)

	reader, err := d.eng.Get(ctx, container, c.key)
	if err != nil {
		return nil, fmt.Errorf("get object %s/%s: %w", c.bucket, c.key, err)
	}
	defer func() { _ = reader.Close() }()

	hasher := md5.New() // #nosec G401
	hashingReader := io.TeeReader(reader, hasher)

	chunker, err := crypto.DefaultFastCDCChunker()
	if err != nil {
		return nil, fmt.Errorf("create chunker: %w", err)
	}
	chunkCh, err := chunker.ChunkContext(ctx, hashingReader)
	if err != nil {
		return nil, fmt.Errorf("start chunker: %w", err)
	}

	var measuredSize int64
	var physicalNew int64
	var chunkCount int
	newRefs := make([]crypto.TenantChunkRef, 0, 16)

	for result := range chunkCh {
		if result.Err != nil {
			return nil, fmt.Errorf("chunking: %w", result.Err)
		}
		chunk := &result.Chunk
		measuredSize += int64(chunk.Size)
		chunkCount++

		lookup, lookupErr := d.gci.LookupChunk(ctx, chunk.Hash)
		if lookupErr != nil {
			return nil, fmt.Errorf("lookup chunk: %w", lookupErr)
		}

		storageKey := "_chunks/" + chunk.Hash

		if !dryRun {
			if lookup.IsNewChunk {
				opts := []engine.PutOption{engine.WithContentLength(int64(chunk.Size))}
				// "_global" — shared chunk container (see internal/api/s3_engine_adapter.go:39)
				bn, putErr := d.eng.Put(ctx, "_global", storageKey, bytes.NewReader(chunk.Data), opts...)
				if putErr != nil {
					return nil, fmt.Errorf("store chunk %s: %w", chunk.Hash[:16], putErr)
				}
				if insertErr := d.gci.InsertChunk(ctx, &crypto.GCIEntry{
					PlaintextHash: chunk.Hash,
					BackendID:     bn,
					StorageKey:    storageKey,
					SizeBytes:     int64(chunk.Size),
					RefCount:      1,
				}); insertErr != nil {
					return nil, fmt.Errorf("insert chunk: %w", insertErr)
				}
				physicalNew += int64(chunk.Size)
			} else {
				if incErr := d.gci.IncrementRef(ctx, chunk.Hash); incErr != nil {
					return nil, fmt.Errorf("increment ref: %w", incErr)
				}
			}
		} else {
			if lookup.IsNewChunk {
				physicalNew += int64(chunk.Size)
			}
		}

		newRefs = append(newRefs, crypto.TenantChunkRef{
			TenantID:             tenantUUID,
			BucketName:           c.bucket,
			ObjectKey:            c.key,
			ChunkIndex:           chunk.Index,
			ChunkOffset:          chunk.Offset,
			PlaintextHash:        chunk.Hash,
			EncryptionKeyVersion: 1,
		})
	}

	computedEtag := fmt.Sprintf("%x", hasher.Sum(nil))
	if computedEtag != c.etag {
		return &Result{
			TenantID:   c.tenantID,
			Bucket:     c.bucket,
			Key:        c.key,
			Failed:     true,
			FailReason: fmt.Sprintf("etag mismatch: computed=%s stored=%s", computedEtag, c.etag),
		}, nil
	}

	if dryRun {
		return &Result{
			TenantID:    c.tenantID,
			Bucket:      c.bucket,
			Key:         c.key,
			SizeBytes:   measuredSize,
			ChunkCount:  chunkCount,
			PhysicalNew: physicalNew,
			Saved:       measuredSize - physicalNew,
		}, nil
	}

	// Step 1: write manifest (atomic swap)
	contentType := "application/octet-stream"
	dedupRatio := float32(1.0)
	if physicalNew > 0 {
		dedupRatio = float32(measuredSize) / float32(physicalNew)
	}
	if err := d.gci.ReplaceObjectManifest(ctx, tenantUUID, c.bucket, c.key, newRefs, &crypto.ObjectMeta{
		TenantID:     tenantUUID,
		BucketName:   c.bucket,
		ObjectKey:    c.key,
		TotalSize:    measuredSize,
		ChunkCount:   chunkCount,
		ContentType:  &contentType,
		LogicalSize:  measuredSize,
		PhysicalSize: &physicalNew,
		DedupRatio:   &dedupRatio,
	}); err != nil {
		return nil, fmt.Errorf("replace manifest: %w", err)
	}

	// Step 2: flip is_chunked flag
	if _, err := d.db.ExecContext(ctx, `
		UPDATE object_head_cache
		SET is_chunked = TRUE, size_bytes = $4, updated_at = NOW()
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, c.tenantID, c.bucket, c.key, measuredSize); err != nil {
		return nil, fmt.Errorf("flip is_chunked: %w", err)
	}

	// Step 3: delete the monolithic copy (only after flag flip)
	if !keepOriginal {
		if err := d.eng.Delete(ctx, container, c.key); err != nil {
			d.log.Warn("delete original failed (object is migrated, original leaked)",
				zap.String("key", c.key), zap.Error(err))
		}
	}

	return &Result{
		TenantID:    c.tenantID,
		Bucket:      c.bucket,
		Key:         c.key,
		SizeBytes:   measuredSize,
		ChunkCount:  chunkCount,
		PhysicalNew: physicalNew,
		Saved:       measuredSize - physicalNew,
	}, nil
}

func bootstrap(logger *zap.Logger) (*sql.DB, *engine.CoreEngine) {
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	dbPort := 5432
	if p := os.Getenv("DB_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			dbPort = v
		}
	}
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "vaultaire"
	}
	dbUser := os.Getenv("DB_USER")
	if dbUser == "" {
		dbUser = "viera"
	}
	dbPassword := os.Getenv("DB_PASSWORD")

	dbConn, err := database.NewPostgres(database.Config{
		Host:     dbHost,
		Port:     dbPort,
		Database: dbName,
		User:     dbUser,
		Password: dbPassword,
		SSLMode:  "disable",
	}, logger)
	if err != nil {
		logger.Fatal("connect to database", zap.Error(err))
	}
	db := dbConn.DB()

	eng := engine.NewEngine(db, logger, &engine.Config{
		EnableCaching:  false,
		EnableML:       false,
		DefaultBackend: "local",
	})

	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire-data"
	}
	if mkErr := os.MkdirAll(dataPath, 0750); mkErr != nil {
		logger.Fatal("create data dir", zap.Error(mkErr))
	}
	localDriver := drivers.NewLocalDriver(dataPath, logger)
	eng.AddDriver("local", localDriver)
	logger.Info("local driver added", zap.String("path", dataPath))

	if accessKey := os.Getenv("S3_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("S3_SECRET_KEY")
		if d, dErr := drivers.NewS3CompatDriver(accessKey, secretKey, logger); dErr == nil {
			eng.AddDriver("s3", d)
			logger.Info("S3 driver added")
		}
	}

	if accessKey := os.Getenv("LYVE_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("LYVE_SECRET_KEY")
		region := os.Getenv("LYVE_REGION")
		if region == "" {
			region = "us-east-1"
		}
		if d, dErr := drivers.NewLyveDriver(accessKey, secretKey, "", region, logger); dErr == nil {
			eng.AddDriver("lyve", d)
			logger.Info("Lyve driver added")
		}
	}

	if accessKey := os.Getenv("QUOTALESS_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("QUOTALESS_SECRET_KEY")
		endpoint := os.Getenv("QUOTALESS_ENDPOINT")
		if endpoint == "" {
			endpoint = "https://us.quotaless.cloud:8000"
		}
		if d, dErr := drivers.NewQuotalessDriver(accessKey, secretKey, endpoint, logger); dErr == nil {
			eng.AddDriver("quotaless", d)
			logger.Info("quotaless driver added")
		}
	}

	if accessKey := os.Getenv("GEYSER_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("GEYSER_SECRET_KEY")
		bucket := os.Getenv("GEYSER_BUCKET")
		if bucket == "" {
			bucket = "stored3lib-632df558-9627-427b-ab86-9f3ff1eaafe9"
		}
		var geyserOpts []drivers.GeyserOption
		if ep := os.Getenv("GEYSER_ENDPOINT"); ep != "" {
			geyserOpts = append(geyserOpts, drivers.WithGeyserEndpoint(ep))
		}
		if d, dErr := drivers.NewGeyserDriver(accessKey, secretKey, bucket, "vaultaire", logger, geyserOpts...); dErr == nil {
			eng.AddDriver("geyser", d)
			logger.Info("geyser driver added")
		}
	}

	if accessKey := os.Getenv("IDRIVE_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("IDRIVE_SECRET_KEY")
		defaultEndpoint := os.Getenv("IDRIVE_ENDPOINT")
		if defaultEndpoint == "" {
			defaultEndpoint = "https://e2-us-west-1.idrive.com"
		}
		defaultRegion := os.Getenv("IDRIVE_REGION")
		if defaultRegion == "" {
			defaultRegion = "us-west-1"
		}
		if d, dErr := drivers.NewIDriveDriver(accessKey, secretKey, defaultEndpoint, defaultRegion, logger); dErr == nil {
			eng.AddDriver("idrive", d)
			logger.Info("iDrive driver added")
		}
		for region, endpoint := range drivers.IDriveRegions {
			driverName := "idrive-" + region
			if d, dErr := drivers.NewIDriveDriver(accessKey, secretKey, endpoint, region, logger); dErr == nil {
				eng.AddDriver(driverName, d)
			}
		}
	}

	if os.Getenv("TENANT_1_ID") != "" {
		if d, dErr := drivers.NewOneDriveFleetDriver(logger); dErr == nil {
			eng.AddDriver("onedrive", d)
			logger.Info("OneDrive fleet driver added")
		}
	}

	storageMode := os.Getenv("STORAGE_MODE")
	if storageMode == "" {
		if os.Getenv("IDRIVE_ACCESS_KEY") != "" {
			storageMode = "idrive"
		} else if os.Getenv("QUOTALESS_ACCESS_KEY") != "" {
			storageMode = "quotaless"
		} else if os.Getenv("S3_ACCESS_KEY") != "" {
			storageMode = "s3"
		} else if os.Getenv("GEYSER_ACCESS_KEY") != "" {
			storageMode = "geyser"
		} else {
			storageMode = "local"
		}
	}
	eng.SetPrimary(storageMode)

	return db, eng
}
