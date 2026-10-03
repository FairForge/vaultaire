package api

import (
	"context"
	"crypto/md5" // #nosec G501 -- S3 ETags are MD5 by specification
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// Background writers — access-log delivery and inventory reports — deliver
// into a tenant's bucket exactly as a customer PUT does (Review R13-02, the
// R6-21 / WP-R6-7 P1). Before this file both called
// eng.Put(ctx, "tenant/<id>/<bucket>", …) on a context with no tenant, wrote
// no head-cache row and reserved no quota. The S3 path addresses
// "<id>_<bucket>" (tenant.NamespaceContainer) and drivers key blobs by the
// tenant in ctx, so every delivered report landed under a container no read
// path addresses, billed to nobody: a customer who enabled logging or
// inventory never saw a single object.
//
// The body is spooled to a temp file first (bounded memory whatever the
// report size — R4-18 buffered the whole report), which also yields the
// exact length and MD5 the write path needs: quota is reserved for the real
// size, the backend PUT streams from disk, and the ETag is the MD5 stream.

var (
	// errTargetBucketGone: the configured target bucket no longer belongs to
	// the tenant (deleted since the config was written). Delivery is skipped
	// and the source rows are kept.
	errTargetBucketGone = errors.New("target bucket no longer exists for this tenant")
	// errTargetQuotaExceeded: the tenant is out of quota on the floor the
	// report would be billed on. Rows are kept for the next pass.
	errTargetQuotaExceeded = errors.New("target tenant over quota")
)

// generatedObjectWriter delivers system-generated objects through the
// customer write path.
type generatedObjectWriter struct {
	db     *sql.DB
	eng    engine.Engine
	quota  QuotaManager               // nil = no accounting (tests without a manager)
	gci    *crypto.GlobalContentIndex // nil = a displaced chunked row's manifest is not released
	logger *zap.Logger
}

func newGeneratedObjectWriter(db *sql.DB, eng engine.Engine, quota QuotaManager, gci *crypto.GlobalContentIndex, logger *zap.Logger) *generatedObjectWriter {
	if db == nil || eng == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &generatedObjectWriter{db: db, eng: eng, quota: quota, gci: gci, logger: logger}
}

// deliveredObject describes what write stored.
type deliveredObject struct {
	Size    int64
	ETag    string
	Backend string
	// OverQuota: the tenant was out of quota and the object was written on
	// the caller's explicit allowance (generatedWriteOptions.AllowOverQuota);
	// the bytes are accounted all the same.
	OverQuota bool
}

// generatedWriteOptions are the per-call choices of writeOpts.
type generatedWriteOptions struct {
	// AllowOverQuota writes the object even when the tenant is out of quota
	// on the floor it lands on. The bytes are force-accounted (never hidden)
	// and the result says OverQuota. For the one object a customer is owed
	// whatever their balance: the GDPR export (WP-R10-3b). Reports and
	// access logs never set it (errTargetQuotaExceeded keeps their rows).
	AllowOverQuota bool
}

// write renders body into key of the tenant's bucket. Ownership of the
// target bucket is re-checked now, not at config time; Object Lock on the
// key is honoured; placement, quota, head row and the version ledger follow
// plain PUT (placeObject, reserveQuota, upsertWholeObjectHeadRow,
// recordObjectVersion). A failure after the backend write leaves a durable
// blob under the key; the caller keeps its source rows and the next pass
// overwrites (inventory: same key per day) or writes a fresh key (access
// logs: timestamp + random suffix).
func (g *generatedObjectWriter) write(ctx context.Context, tenantID, bucket, key, contentType string, body func(io.Writer) error) (deliveredObject, error) {
	return g.writeOpts(ctx, tenantID, bucket, key, contentType, body, generatedWriteOptions{})
}

// writeOpts is write with per-call options.
func (g *generatedObjectWriter) writeOpts(ctx context.Context, tenantID, bucket, key, contentType string, body func(io.Writer) error, o generatedWriteOptions) (deliveredObject, error) {
	if g == nil {
		return deliveredObject{}, errors.New("generated object writer not configured")
	}
	if tenantID == "" || bucket == "" || key == "" {
		return deliveredObject{}, errors.New("generated object: tenant, bucket and key are required")
	}

	var exists bool
	if err := g.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`, tenantID, bucket).Scan(&exists); err != nil {
		return deliveredObject{}, fmt.Errorf("check target bucket %s: %w", bucket, err)
	}
	if !exists {
		return deliveredObject{}, errTargetBucketGone
	}
	if err := checkObjectLock(ctx, g.db, tenantID, bucket, key, false); err != nil {
		return deliveredObject{}, fmt.Errorf("target key %s/%s: %w", bucket, key, err)
	}

	// Spool: exact size + MD5 without holding the report in memory.
	spool, err := os.CreateTemp("", "vaultaire-report-*")
	if err != nil {
		return deliveredObject{}, fmt.Errorf("spool report: %w", err)
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()
	hasher := md5.New() // #nosec G401 -- S3 ETag
	counter := &countingWriter{w: io.MultiWriter(spool, hasher)}
	if err := body(counter); err != nil {
		return deliveredObject{}, fmt.Errorf("render report: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return deliveredObject{}, fmt.Errorf("rewind spool: %w", err)
	}
	size := counter.n
	etag := hex.EncodeToString(hasher.Sum(nil))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	t := &tenant.Tenant{ID: tenantID}
	container := t.NamespaceContainer(bucket)
	tctx := common.WithTenantID(ctx, tenantID)

	class := resolvePutStorageClass(tctx, g.db, g.eng, tenantID, bucket, "")
	floor := usage.FloorOf(class)
	overQuota := false
	if g.quota != nil {
		ok, qErr := reserveQuota(tctx, g.quota, tenantID, floor, size)
		if qErr != nil {
			return deliveredObject{}, fmt.Errorf("reserve quota: %w", qErr)
		}
		if !ok {
			if !o.AllowOverQuota {
				return deliveredObject{}, errTargetQuotaExceeded
			}
			// The allowance: account the bytes unconditionally (a negative
			// release adds), so the usage the customer sees is true.
			if err := releaseQuotaOn(tctx, g.quota, tenantID, floor, -size); err != nil {
				return deliveredObject{}, fmt.Errorf("account over-quota write: %w", err)
			}
			overQuota = true
			g.logger.Warn("generated object: written past the tenant's quota on an explicit allowance",
				zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", key), zap.Int64("bytes", size))
		}
	}
	release := func() {
		if g.quota != nil && size > 0 {
			if err := releaseQuotaOn(context.WithoutCancel(tctx), g.quota, tenantID, floor, size); err != nil {
				g.logger.Error("generated object: quota release failed", zap.Error(err), zap.String("tenant_id", tenantID))
			}
		}
	}

	opts := []engine.PutOption{engine.WithContentLength(size), engine.WithContentType(contentType)}
	if class != "" {
		opts = append(opts, engine.WithStorageClass(class))
	}
	backendName, err := placeObject(tctx, g.db, g.eng, tenantID, bucket, container, key, spool, opts...)
	if err != nil {
		release()
		return deliveredObject{}, fmt.Errorf("store %s/%s: %w", bucket, key, err)
	}

	attrs := objectAttrs{ContentType: contentType}
	displaced, dbErr := atomicHeadUpsertReleasing(tctx, g.db, manifestReleaser(g.gci), tenantID, bucket, key, func(tx *sql.Tx) error {
		return upsertWholeObjectHeadRow(tctx, tx, tenantID, bucket, key, size, etag, backendName, floor, attrs)
	})
	if dbErr != nil {
		// The blob is durable but invisible and unbilled (HEAD/GET/LIST read
		// the head row only); the next pass writes it again. Same rule as
		// plain PUT (R3-10): never report success without the row.
		release()
		return deliveredObject{}, fmt.Errorf("head row for %s/%s: %w", bucket, key, dbErr)
	}
	if g.quota != nil && displaced.Size > 0 {
		if err := releaseQuotaOn(context.WithoutCancel(tctx), g.quota, tenantID, displaced.Floor, displaced.Size); err != nil {
			g.logger.Error("generated object: release overwritten object failed", zap.Error(err), zap.String("tenant_id", tenantID))
		}
	}
	// A report that replaces one stored on another backend (the target
	// bucket changed tier or visibility between two runs): as plain PUT.
	dropDisplacedBlob(tctx, g.db, g.eng, g.logger, lostWriteOverwrite, tenantID, bucket, container, key, displaced, backendName)
	recordObjectVersion(tctx, g.db, tenantID, bucket, key, size, etag, contentType, backendName)

	return deliveredObject{Size: size, ETag: etag, Backend: backendName, OverQuota: overQuota}, nil
}

// countingWriter counts bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
