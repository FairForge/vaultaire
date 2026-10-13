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
	"github.com/prometheus/client_golang/prometheus"
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
	// errGeneratedSSEUnavailable: the target bucket is sse_enabled and this
	// process has no master key (unset on a slot): refused, never plaintext
	// (R8-04's rule, on the write side).
	errGeneratedSSEUnavailable = errors.New("the target bucket requires encryption and this server has no encryption key configured; it was not written")
)

// The writers on vaultaire_generated_object_refused_total, and the reasons.
const (
	generatedKindExport    = "export"
	generatedKindAccessLog = "access_log"
	generatedKindInventory = "inventory"

	refusedSSEUnavailable = "sse_unavailable"
)

// generatedObjectRefused counts generated objects refused instead of being
// stored plaintext in an sse_enabled bucket.
var generatedObjectRefused = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_generated_object_refused_total",
	Help: "System-generated objects (GDPR export, access log, inventory report) not written because the target bucket is sse_enabled and the object could not be encrypted: sse_unavailable (no master key on this process). Never stored plaintext. (too_large_for_sse is gone: SSE-S3 seals any size since WP-R8-5.)",
}, []string{"kind", "reason"})

func init() {
	for _, k := range []string{generatedKindExport, generatedKindAccessLog, generatedKindInventory} {
		for _, r := range []string{refusedSSEUnavailable} {
			generatedObjectRefused.WithLabelValues(k, r)
		}
	}
}

// generatedObjectWriter delivers system-generated objects through the
// customer write path.
type generatedObjectWriter struct {
	db     *sql.DB
	eng    engine.Engine
	quota  QuotaManager               // nil = no accounting (tests without a manager)
	gci    *crypto.GlobalContentIndex // nil = a displaced chunked row's manifest is not released
	logger *zap.Logger
	// sse seals the object when the target bucket is sse_enabled, exactly as
	// HandlePut does (WP-R8-1 follow-up A1). nil = this process has no
	// master key: a write into an sse_enabled bucket is refused, never
	// stored plaintext.
	sse *crypto.SSEService
	// allowSync is the PUT's gate for a `sync`-tier bucket (the tenant's
	// sync_backend flag, syncPlacementGate): a generated object lands where
	// a PUT into the same bucket would (#677: it was nil, so a report into a
	// flagged tenant's sync bucket landed on the primary, off the bucket's
	// tier). nil = no tenant may use sync (the GDPR export's `_exports` is
	// never a sync bucket).
	allowSync func(tenantID string) bool
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
	// Kind names the writer on vaultaire_generated_object_refused_total:
	// generatedKindExport | generatedKindAccessLog | generatedKindInventory.
	Kind string
}

// write renders body into key of the tenant's bucket; kind names the writer
// (generatedKindAccessLog, generatedKindInventory). Ownership of the
// target bucket is re-checked now, not at config time; Object Lock on the
// key is honoured; placement, quota, head row and the version ledger follow
// plain PUT (placeObject at a new address, reserveQuota, the commitBlobWrite
// flip with the version row). A failure after the backend write leaves the
// key's previous object as it was and the new blob queued as an orphan; the
// caller keeps its source rows and the next pass writes again (inventory:
// same key per day) or a fresh key (access logs: timestamp + random suffix).
func (g *generatedObjectWriter) write(ctx context.Context, kind, tenantID, bucket, key, contentType string, body func(io.Writer) error) (deliveredObject, error) {
	return g.writeOpts(ctx, tenantID, bucket, key, contentType, body, generatedWriteOptions{Kind: kind})
}

// writeOpts is write with per-call options.
func (g *generatedObjectWriter) writeOpts(ctx context.Context, tenantID, bucket, key, contentType string, body func(io.Writer) error, o generatedWriteOptions) (deliveredObject, error) {
	if g == nil {
		return deliveredObject{}, errors.New("generated object writer not configured")
	}
	if tenantID == "" || bucket == "" || key == "" {
		return deliveredObject{}, errors.New("generated object: tenant, bucket and key are required")
	}

	// One read: the bucket is still the tenant's, and whether it requires
	// encryption. An error is an error — never "not encrypted".
	var sseBucket bool
	err := g.db.QueryRowContext(ctx,
		`SELECT sse_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, bucket).Scan(&sseBucket)
	if errors.Is(err, sql.ErrNoRows) {
		return deliveredObject{}, errTargetBucketGone
	}
	if err != nil {
		return deliveredObject{}, fmt.Errorf("check target bucket %s: %w", bucket, err)
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

	// An sse_enabled bucket gets what HandlePut gives it (WP-R8-1 follow-up
	// A1): the body sealed with SSE-S3 under the tenant's current key as it
	// streams from the spool file (WP-R8-5: at any size, one segment in
	// memory), the head row recording the algorithm and the version, size
	// and ETag the plaintext's. What cannot be sealed — no master key on
	// this process — is refused before anything is reserved or written:
	// never stored plaintext. An empty body has nothing to seal (as on PUT).
	var putBody io.Reader = spool
	putSize := size
	var enc objectEncryption
	if sseBucket && size > 0 {
		if g.sse == nil {
			reason, refusal := refusedSSEUnavailable, errGeneratedSSEUnavailable
			kind := o.Kind
			if kind == "" {
				kind = "other"
			}
			generatedObjectRefused.WithLabelValues(kind, reason).Inc()
			g.logger.Warn("generated object refused: its bucket requires encryption and it cannot be encrypted — never stored plaintext",
				zap.String("kind", kind), zap.String("reason", reason), zap.String("tenant_id", tenantID),
				zap.String("bucket", bucket), zap.String("key", key), zap.Int64("bytes", size))
			return deliveredObject{}, refusal
		}
		sealer, encErr := newSSEWriteSealer(tctx, g.sse, tenantID, spool, size)
		if encErr != nil {
			return deliveredObject{}, fmt.Errorf("encrypt %s/%s: %w", bucket, key, encErr)
		}
		putBody = sealer
		putSize = g.sse.SealedLength(size)
		enc = sealer.encryption()
	}

	class := resolvePutStorageClass(tctx, g.db, g.eng, g.allowSync, tenantID, bucket, "")
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

	opts := []engine.PutOption{engine.WithContentLength(putSize), engine.WithContentType(contentType)}
	if class != "" {
		opts = append(opts, engine.WithStorageClass(class))
	}
	// A new address per write (WP-R2-1): the report this one replaces stays
	// readable until the flip, and a failure leaves it untouched.
	blob, err := newBlobAddress(key)
	if err != nil {
		release()
		return deliveredObject{}, err
	}
	backendName, err := placeObject(tctx, g.db, g.eng, tenantID, bucket, container, blob, putBody, opts...)
	if err != nil {
		release()
		return deliveredObject{}, fmt.Errorf("store %s/%s: %w", bucket, key, err)
	}

	attrs := objectAttrs{ContentType: contentType}
	flip, dbErr := commitBlobWrite(tctx, g.db, manifestReleaser(g.gci), blobWrite{TenantID: tenantID, Bucket: bucket, Key: key,
		Version: &versionWrite{Size: size, ETag: etag, ContentType: contentType, Backend: backendName,
			Blob: versionBlobRef{StorageKey: blob, Enc: enc}},
		Upsert: func(tx *sql.Tx) error {
			return upsertWholeObjectHeadRowEnc(tctx, tx, tenantID, bucket, key, blob, size, etag, backendName, floor, attrs, enc)
		}})
	if dbErr != nil {
		// The blob is an orphan no row names (queued for the collector); the
		// next pass writes afresh. Same rule as plain PUT (R3-10): never
		// report success without the row.
		release()
		retireOrphanWrite(tctx, g.db, g.logger, tenantID, bucket, blob, backendName)
		return deliveredObject{}, fmt.Errorf("head row for %s/%s: %w", bucket, key, dbErr)
	}
	displaced := flip.Displaced
	if g.quota != nil {
		if err := releaseDisplacedOn(context.WithoutCancel(tctx), g.quota, tenantID, displaced); err != nil {
			g.logger.Error("generated object: release overwritten object failed", zap.Error(err), zap.String("tenant_id", tenantID))
		}
	}

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
