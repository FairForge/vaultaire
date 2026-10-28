// Package dataplane is the data plane of plan 40.10 as a Go package
// (WP-EDGE-3, Stage 1a Prompt 47): the pipeline a data-plane host runs
// around one S3 write or read — admit, the move or the codec, commit — and
// the ControlPlane it asks. The Cloudflare Worker (edge/s3-ingest) is the
// same pipeline in JavaScript; this package is it in Go, for the two other
// kinds of host: a data node (cmd/vaultaire with VAULTAIRE_ROLE=node, over
// HTTPS — httpcp.go) and the origin itself (host=slc, the control plane's
// functions called in-process — internal/api's Server.DataPlane).
//
// A host holds no key of the hierarchy (contract line 4): the admit hands
// it presigned or header-signed backend requests and, for a codec write,
// the tenant's codec PUBLIC key; the per-chunk keys it makes die with the
// request. The control plane verifies every client signature, decides,
// reserves and commits; the host never decides anything the control plane
// did not answer.
//
// The wire types below were internal/api's (edgeAdmitRequest, …); internal/api
// aliases them, so the JSON is the one the Worker already speaks.
package dataplane

import (
	"context"
	"time"
)

// ControlPlane is what a data-plane host asks: the three calls of
// /api/v1/edge/*. Admit decides one S3 request; Index answers a codec
// write's chunk hashes (hit, or a link to store the miss); Commit settles
// what the host did. Each returns the call's own error (a transport
// failure, a non-200 of the control plane); a decision — a pass-through,
// a relayed S3 refusal — is a value, never an error.
type ControlPlane interface {
	Admit(ctx context.Context, req AdmitRequest) (AdmitResponse, error)
	Index(ctx context.Context, req IndexRequest) (IndexResponse, error)
	Commit(ctx context.Context, req CommitRequest) (CommitResponse, error)
}

// CommitResponse is the commit's answer: the S3 answer the host relays to
// the client (a write), or an acknowledgement (a read — the client was
// answered at the backend's headers).
type CommitResponse = Relay

// Op values of an admit.
const (
	OpPut  = "put"
	OpPart = "part"
	OpGet  = "get"
)

// FeatureWindow: the host reads a windowed answer (D-EDGE-32).
const FeatureWindow = "window"

// FeatureRS: the host stripes an RS write itself (WP-EDGE-4 PR B) — a Go
// host (a node, host=slc); the Worker never names it.
const FeatureRS = "rs"

// AdmitRequest is what the host describes of the client's request.
type AdmitRequest struct {
	Op        string            `json:"op"`
	Method    string            `json:"method"`
	Host      string            `json:"host"`
	Path      string            `json:"path"`
	Query     string            `json:"query"`
	Headers   map[string]string `json:"headers"`
	ClientIP  string            `json:"client_ip"`
	RequestID string            `json:"request_id"`
	// Features the host reads (0.4.0: "window"): an answer a host does not
	// know is never given — a 0.3.0 Worker would GET a windowed link whole
	// and fall back, charging the backend's breaker.
	Features []string `json:"features,omitempty"`
}

// Has says whether the host named the feature.
func (r AdmitRequest) Has(feature string) bool {
	for _, f := range r.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// Expect tells the host what the backend will enforce.
type Expect struct {
	MD5 bool `json:"md5"` // the link signs Content-MD5: the backend enforces it
}

// AdmitResponse is the decision.
type AdmitResponse struct {
	Edge    bool   `json:"edge"`
	Reason  string `json:"reason,omitempty"` // why not (a pass-through), for the host's log line
	Refusal *Relay `json:"refusal,omitempty"`
	AdmitID string `json:"admit_id,omitempty"`
	URL     string `json:"url,omitempty"`
	// URLs (WP-EDGE-4, edge_mirror): a mirrored write's links — [the
	// placement's, the mirror leg's], the same signed headers on both; URL
	// stays URLs[0] for a host that knows one link. A read: [the primary's,
	// the mirror's] — the second tried when the first does not answer as
	// admitted, before the origin fallback (MirrorSendHeaders its headers).
	URLs              []string          `json:"urls,omitempty"`
	MirrorSendHeaders map[string]string `json:"mirror_send_headers,omitempty"`
	// RS (WP-EDGE-4 PR B, edge_rs): the write is striped K+M by the host
	// (internal/ec) — one presigned PUT per shard, no body link. Answered
	// only to a host naming FeatureRS (a Go host; never the Worker).
	RS          *RSGrant          `json:"rs,omitempty"`
	Method      string            `json:"method,omitempty"`
	SendHeaders map[string]string `json:"send_headers,omitempty"`
	Backend     string            `json:"backend,omitempty"`
	StorageKey  string            `json:"storage_key,omitempty"`
	Expect      *Expect           `json:"expect,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	// A GET (Part B): the status the backend must answer (206 — every link
	// names a Range, the whole object's for a whole read), the status the
	// CLIENT gets (200, or 206 for the client's own Range), the ORIGIN's
	// response headers the host serves in place of the backend's
	// (object_headers.go's set), and the admitted span in bytes — the
	// backend's Content-Length must equal it or the host falls back to the
	// origin, whose integrity path decides (RP66-2).
	ExpectStatus    int               `json:"expect_status,omitempty"`
	Status          int               `json:"status,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	ContentLength   int64             `json:"content_length,omitempty"`
	// BodyMD5 (a GET, RP74-4): the whole object's MD5 (hex) the row
	// records — "" when it records none a reader can check (a -N ETag with
	// no body_md5, a chunked row). A host that verifies (host=slc) checks a
	// whole read against it; the Worker and a node ignore it.
	BodyMD5 string `json:"body_md5,omitempty"`
	// Window (WP-EDGE-2 Part C, D-EDGE-32): a read of at least 64 MiB is
	// the windowed reader's — 16 MiB ranges, at most 6 in flight, from
	// Start over ContentLength bytes, each written in order; the link signs
	// no range. Answered only to a host naming FeatureWindow.
	Window *Window `json:"window,omitempty"`
	// An aws-chunked write (WP-EDGE-2 Part C): StreamingTrailer is the
	// trailer the body's framing ends in (the codec decodes the framing and
	// VERIFIES it — D-EDGE-31), ContentLength the DECODED length. The mover
	// gets Headers instead of a presigned link: a HEADER-signed backend
	// request (lower-cased, authorization included) to send to URL with the
	// client's body untouched (D-EDGE-30, D-EDGE-33).
	StreamingTrailer string            `json:"streaming_trailer,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	// Unverified: an admitted write whose bytes nothing checks — an
	// aws-chunked write to a backend that does not verify the trailer, with
	// no Content-MD5 the commit could compare (the metric's
	// admitted_unverified; never on the wire).
	Unverified bool `json:"-"`
	// Codec (WP-EDGE-2): the write is the HOST's to chunk, hash and seal —
	// no presigned PUT for the body; the host asks Index per batch of chunk
	// hashes, uploads the misses as 0x06 blobs and commits the chunk list.
	Codec *CodecGrant `json:"codec,omitempty"`
}

// Window is a windowed read's shape: the range size, the most ranges in
// flight (a Worker's connection limit), the span's first byte.
type Window struct {
	RangeBytes int64 `json:"range_bytes"`
	InFlight   int   `json:"in_flight"`
	Start      int64 `json:"start"`
}

// CodecGrant is what a codec admit hands the data-plane host: the tenant's
// codec PUBLIC key (X-Wing, base64 in JSON) and its key version, and the
// two numbers the pipeline needs. Nothing here is a secret (contract line
// 4: nothing of the hierarchy travels but a public key).
type CodecGrant struct {
	Version        int    `json:"version"`
	KEMPublicKey   []byte `json:"kem_public_key"`
	ChunkThreshold int64  `json:"chunk_threshold"`
	SegmentSize    int    `json:"segment_size"`
}

// RSGrant is an RS write's shard links (WP-EDGE-4 PR B): the layout and,
// per shard, its presigned PUT (signing ShardBytes) on its leg.
type RSGrant struct {
	K          int         `json:"k"`
	M          int         `json:"m"`
	Stripe     int         `json:"stripe"`
	ShardBytes int64       `json:"shard_bytes"`
	Shards     []ShardLink `json:"shards"`
}

// ShardLink is one shard's PUT.
type ShardLink struct {
	Index       int               `json:"index"`
	Leg         string            `json:"leg"`
	URL         string            `json:"url"`
	SendHeaders map[string]string `json:"send_headers"`
}

// Relay is one S3 answer, as JSON for the host: the exact status, headers
// and XML body the origin would have answered.
type Relay struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
}

// CommitRequest is what the host reports: the backend's answer. No digest
// and no byte count for a moved write — the host is not in the byte path
// there. A CODEC write reverses that on purpose (WP-EDGE-2): the host read
// every byte to chunk it, so it reports the chunk list and the whole body's
// MD5 (the digest is free once the bytes were read).
type CommitRequest struct {
	AdmitID       string `json:"admit_id"`
	Outcome       string `json:"outcome"` // a write: ok | backend_error | edge_error | checksum_mismatch; a GET: served | fallback | error
	BackendStatus int    `json:"backend_status"`
	BackendCode   string `json:"backend_code"` // the backend's XML <Code>, when it answered an error; the subrequest's error otherwise
	BackendETag   string `json:"backend_etag"`
	DurationMS    int64  `json:"duration_ms"`
	// A GET: the Content-Length the backend answered at its headers — what
	// is metered (the host cannot see the end of a stream it does not touch).
	BackendContentLength int64 `json:"backend_content_length,omitempty"`
	// A windowed GET (D-EDGE-32): the bytes the host wrote to the client,
	// reported when its stream ended.
	BytesSent int64 `json:"bytes_sent,omitempty"`
	// A codec write (WP-EDGE-2): the ordered chunk list and the body's MD5
	// (hex) — a PUT's ETag, a part's part digest.
	Chunks  []CommitChunk `json:"chunks,omitempty"`
	BodyMD5 string        `json:"body_md5,omitempty"`
	// BackendETags (WP-EDGE-4): a mirrored write's ETag per link, in the
	// admit's urls order; a failed mirror PUT's is omitted (the copy is the
	// blob_copies job's). ServedURL: the read link that served (0 the
	// primary, 1 the mirror).
	BackendETags []string `json:"backend_etags,omitempty"`
	ServedURL    int      `json:"served_url,omitempty"`
	// ShardETags (an RS write): each shard's backend ETag, in shard order;
	// "" for a shard that did not land. BodyMD5 is the body's.
	ShardETags []string `json:"shard_etags,omitempty"`
	// Verified (a GET, RP74-4): the host checks the whole body against the
	// admit's body_md5 as it streams — counted verified only for host=slc,
	// the one host the control plane trusts to say so.
	Verified bool `json:"verified,omitempty"`
}

// The outcomes a host reports.
const (
	OutcomeOK               = "ok"
	OutcomeBackendError     = "backend_error"
	OutcomeEdgeError        = "edge_error"
	OutcomeChecksumMismatch = "checksum_mismatch"
	OutcomeServed           = "served"
	OutcomeFallback         = "fallback"
	OutcomeError            = "error"
)

// CommitChunk is one chunk of a codec commit, in body order: the PLAIN
// sha256 (hex; over the service channel for the duration of the call,
// never stored or logged — D-EDGE-16), its plaintext and sealed sizes, the
// backend's MD5 ETag of the sealed bytes for a chunk this host stored, and
// whether it was stored or a dedup hit.
type CommitChunk struct {
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	SealedSize  int64  `json:"sealed_size,omitempty"`
	BackendETag string `json:"backend_etag,omitempty"`
	// BackendETags (WP-EDGE-4): one per link of a mirrored miss, in the
	// links' order; a failed mirror PUT's is omitted.
	BackendETags []string `json:"backend_etags,omitempty"`
	Status       string   `json:"status"` // stored | hit
}

// IndexRequest is one batch of the host's chunk hashes.
type IndexRequest struct {
	AdmitID string       `json:"admit_id"`
	Chunks  []IndexChunk `json:"chunks"`
}

// IndexChunk is one chunk hash asked of the index.
type IndexChunk struct {
	Hash string `json:"hash"` // plain sha256, hex — for the duration of this call
	Size int64  `json:"size"`
	// Public (WP-EDGE-4 PR D): the host's catalogue filter holds the hash —
	// the control plane checks public_chunks and answers a hit on the one
	// `_public` copy, or a miss (a false positive) as for any chunk.
	Public bool `json:"public,omitempty"`
}

// IndexMiss is a chunk the host must store: one presigned PUT at the
// chunk's one address, signing the SEALED length.
type IndexMiss struct {
	Hash         string            `json:"hash"`
	URL          string            `json:"url"`
	URLs         []string          `json:"urls,omitempty"` // [URL, the mirror leg's] (WP-EDGE-4)
	SendHeaders  map[string]string `json:"send_headers"`
	SealedLength int64             `json:"sealed_length"`
}

// IndexResponse answers an IndexRequest: the hashes the index holds (hits)
// and a link per hash the host must store (misses).
type IndexResponse struct {
	Hits   []string    `json:"hits"`
	Misses []IndexMiss `json:"misses"`
}

// IndexBatch is the most chunk hashes one Index call carries (the Worker's
// INDEX_BATCH).
const IndexBatch = 64
