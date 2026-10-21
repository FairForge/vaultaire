package api

// WP-R3-1: ONE placement decision for every write that stores an object's
// bytes from a stream it has not read yet — a single PUT (HandlePut) and a
// CompleteMultipartUpload. The rule: a Complete is placed exactly as a single
// PUT of the same bytes and class would be. Before, Complete always wrote one
// whole PLAINTEXT blob — into an sse_enabled bucket too, with or without a
// master key (R3-07), never deduplicated, never compressed — while a PUT of
// the same bytes was chunked, chunk-encrypted, sealed or refused.
//
// The decision, in order (the PUT's, unchanged):
//
//   - chunked when the content index exists, the size is over the chunking
//     threshold (64 MiB), the bucket is not versioned (Enabled or Suspended:
//     a manifest carries no version id), the class is chunkable
//     (storageClassDisablesChunking), the bucket is not pinned to a region,
//     the `chunking` flag is on and the request is not SSE-C. The chunks are
//     encrypted (AES256-CE) whenever the process has the chunk encryptor —
//     whatever the bucket says;
//   - refused 503 + Retry-After when encryption is required (the bucket is
//     sse_enabled, or the request asks x-amz-server-side-encryption:
//     AES256) and this process has no SSE service (no master key), SSE-C
//     excepted (the customer's own key) — R8-04's rule for the read, on the
//     write side; a request that asked for encryption is never stored
//     plaintext with a 200 (WP-R8-5 Part 0, W-4);
//   - whole and SSE-S3 sealed when encryption is required and the object is
//     not chunk-encrypted, at any size: the streaming codec (WP-R8-5,
//     internal/crypto/stream.go) holds one segment, never the object, so
//     no size is refused for encryption (the 256 MiB cap and its 413 are
//     gone);
//   - whole plaintext otherwise.
//
// The bucket's encryption setting is read strictly: a read that fails is a
// 500, never "not encrypted".

import (
	"context"
	"net/http"

	"go.uber.org/zap"
)

// placementKind is where a write's bytes go.
type placementKind int

const (
	placeWhole       placementKind = iota // one blob, as received
	placeWholeSealed                      // one blob, SSE-S3 sealed as it streams (format 0x03)
	placeChunked                          // the chunk pipeline: content-defined chunks, dedup, per-chunk encryption
)

func (k placementKind) String() string {
	switch k {
	case placeWholeSealed:
		return "whole_sealed"
	case placeChunked:
		return "chunked"
	}
	return "whole"
}

// placementRequest is what the decision is made from.
type placementRequest struct {
	TenantID, Bucket string
	// Class is the RESOLVED storage class (resolvePutStorageClass).
	Class string
	// Size is the object's logical size: the PUT's declared length, the sum
	// of a Complete's parts.
	Size int64
	// SSEC: the request carries SSE-C headers (a PUT; Create refuses them).
	SSEC bool
	// SSERequested: x-amz-server-side-encryption: AES256 on the request.
	SSERequested bool
}

// writePlacement is the decision.
type writePlacement struct {
	Kind placementKind
	// ChunkEncrypted: placeChunked with per-chunk encryption.
	ChunkEncrypted bool
	// Refusal, when set, is the answer instead of a write.
	Refusal *placementRefusal
}

// placementRefusal is a write the placement refuses, before any byte.
type placementRefusal struct {
	Status     int
	Code       string
	Message    string // AWS's own sentence, when the code's default is not it
	Suggestion string
	RetryAfter string
}

// write answers the refusal.
func (p *placementRefusal) write(w http.ResponseWriter, r *http.Request) {
	if p.RetryAfter != "" {
		w.Header().Set("Retry-After", p.RetryAfter)
	}
	var opts []ErrorOption
	if p.Message != "" {
		opts = append(opts, WithMessage(p.Message))
	}
	if p.Suggestion != "" {
		opts = append(opts, WithSuggestion(p.Suggestion))
	}
	if len(opts) > 0 {
		WriteS3ErrorWithContext(w, p.Code, r.URL.Path, generateRequestID(), opts...)
		return
	}
	WriteS3Error(w, p.Code, r.URL.Path, generateRequestID())
}

// ssecRequiredRefusal is AWS's wording for a write without a customer key
// into a bucket whose policy demands one (the bucket-policy
// s3:x-amz-server-side-encryption-customer-algorithm condition): what a
// ssec_required bucket answers (WP-SSEC-1 B.5).
const ssecRequiredRefusal = "Requests specifying Server Side Encryption with Customer provided keys must provide an appropriate secret key."

// refuseSSECRequired: the bucket requires a customer key and the write
// carries none — 400, the client's, before any byte.
var refuseSSECRequired = &placementRefusal{Status: http.StatusBadRequest, Code: ErrInvalidRequest, Message: ssecRequiredRefusal,
	Suggestion: "This bucket requires SSE-C: send x-amz-server-side-encryption-customer-algorithm: AES256, -key and -key-MD5 on every write."}

var (
	refuseUnreadableEncryption = &placementRefusal{Status: http.StatusInternalServerError, Code: ErrInternalError}
	refuseNoMasterKey          = &placementRefusal{Status: http.StatusServiceUnavailable, Code: ErrServiceUnavailable, RetryAfter: "30",
		Suggestion: "This bucket encrypts objects at rest and the server's encryption key is not available right now. Nothing was stored; retry later."}
	refuseCleartextTierNoKey = &placementRefusal{Status: http.StatusServiceUnavailable, Code: ErrServiceUnavailable, RetryAfter: "30",
		Suggestion: "This bucket stores on a backend reached without TLS, so objects are sealed before they leave the server, and the server's encryption key is not available right now. Nothing was stored; retry later."}
	refuseNoMasterKeyRequested = &placementRefusal{Status: http.StatusServiceUnavailable, Code: ErrServiceUnavailable, RetryAfter: "30",
		Suggestion: "The request asks for server-side encryption (x-amz-server-side-encryption: AES256) and the server's encryption key is not available right now. Nothing was stored; retry later."}
)

// versionedChunkingEnabled reports the tenant's `versioned_chunking` flag;
// no flag service = off (dark).
func (a *S3ToEngine) versionedChunkingEnabled(tenantID string) bool {
	return a.flags != nil && a.flags.Enabled(flagVersionedChunking, tenantID)
}

// decidePlacement is the one placement decision (file comment). It reads the
// bucket row (versioning, region pin, sse_enabled) and the `chunking` flag
// once.
func (a *S3ToEngine) decidePlacement(ctx context.Context, req placementRequest) writePlacement {
	chunkThreshold := a.chunkThreshold()

	// A versioned bucket (Enabled or Suspended) chunks only with the
	// tenant's `versioned_chunking` flag (WP-VM-1 Part D, default off):
	// since 092 a manifest has an address and a version keeps it. Off, the
	// bucket stores whole objects as before.
	versioned := false
	if a.db != nil {
		vs := getBucketVersioningStatus(ctx, a.db, req.TenantID, req.Bucket)
		versioned = (vs == "Enabled" || vs == "Suspended") && !a.versionedChunkingEnabled(req.TenantID)
	}
	// A RESILIENT / archive / PUBLIC / SYNC class keeps its tier's placement
	// promise only whole: chunk blobs live on the engine primary.
	byTier := storageClassDisablesChunking(req.Class)
	// A bucket pinned to a region promises where its bytes live (WP-R7-1);
	// a chunk blob has one address on the primary. Asked only where it can
	// matter (one bucket read per large write).
	byRegion := false
	if a.gci != nil && a.db != nil && req.Size > chunkThreshold {
		if name, rErr := bucketRegionDriver(ctx, a.db, a.engine, req.TenantID, req.Bucket); name != "" || rErr != nil {
			byRegion = true
		}
	}
	// The flag is read ONCE per write: a refresh between two reads could
	// skip SSE here and then keep the object out of the chunk path (R8-03).
	chunkable := a.gci != nil && req.Size > chunkThreshold && !versioned && !byTier && !byRegion &&
		!req.SSEC && a.chunkingEnabled(req.TenantID)
	chunkEncrypt := chunkable && a.chunkEncSvc != nil

	benc, err := bucketEncryptionSettings(ctx, a.db, req.TenantID, req.Bucket)
	if err != nil {
		a.logger.Error("write: bucket encryption setting unreadable", zap.Error(err), zap.String("bucket", req.Bucket))
		return writePlacement{Refusal: refuseUnreadableEncryption}
	}
	bucketSSE := benc.SSE
	// A bucket that requires a customer key refuses an unkeyed write, before
	// anything else (WP-SSEC-1 B.5): the client's 400, never a plaintext
	// object in a bucket that promised keys.
	if benc.SSECRequired && !req.SSEC {
		markClientRefusal(ctx)
		return writePlacement{Refusal: refuseSSECRequired}
	}
	if bucketSSE && a.sseService == nil && !req.SSEC {
		a.logger.Error("write into an encrypted bucket refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", req.Bucket))
		return writePlacement{Refusal: refuseNoMasterKey}
	}
	if req.SSERequested && a.sseService == nil && !req.SSEC {
		// The client chose the header: Warn, and out of the 5xx count
		// (R85-9) — the bucket case above is the operator's, at Error.
		a.logger.Warn("write asking for SSE-S3 refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", req.Bucket))
		markClientRefusal(ctx)
		return writePlacement{Refusal: refuseNoMasterKeyRequested}
	}

	// A tier on a cleartext transport (BLOMP: Blomp's storage URL is plain
	// HTTP) takes only sealed bytes — SSE-S3 whatever the bucket says, or
	// the customer's own key; with no master key the write is refused.
	// BLOMP_ALLOW_UNSEALED lifts it (testing; the bucket's own rules stay).
	cleartextTier := req.Class == "BLOMP" && !req.SSEC && !a.allowUnsealedCleartext
	if cleartextTier && a.sseService == nil && req.Size > 0 {
		a.logger.Error("write onto a cleartext-transport tier refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", req.Bucket), zap.String("class", req.Class))
		return writePlacement{Refusal: refuseCleartextTierNoKey}
	}

	encryptionRequired := (req.SSERequested || bucketSSE || cleartextTier) && !req.SSEC
	if a.sseService != nil && encryptionRequired && !chunkEncrypt && req.Size > 0 {
		return writePlacement{Kind: placeWholeSealed}
	}
	if chunkable {
		return writePlacement{Kind: placeChunked, ChunkEncrypted: chunkEncrypt}
	}
	return writePlacement{Kind: placeWhole}
}
