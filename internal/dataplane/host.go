package dataplane

// The pipeline a Go data-plane host runs around one S3 request — the
// harness's (internal/api/edge_codec_harness_test.go, WP-EDGE-2 Part A)
// lifted into a package, and the Worker's (edge/s3-ingest) in Go:
//
//   - a write: admit; a refusal relayed; a pass-through handed back to the
//     caller untouched; a codec admit chunked with the frozen chunker
//     (crypto.DefaultChunker), each chunk's sha256 asked of the index per
//     IndexBatch, every miss sealed (crypto.SealWrappedChunk, the 0x06
//     blob, under the admit's PUBLIC key) and PUT on its link, the chunk
//     list committed with the body's MD5 — an aws-chunked body decoded and
//     its trailer verified first (D-EDGE-31); else the move: the body PUT
//     to the presigned link with its signed headers, or — aws-chunked —
//     under the admit's header-signed request (D-EDGE-30/33), and the
//     backend's answer committed.
//   - a read: admit; the presigned backend GET; the backend's body under
//     the ORIGIN's headers at the client's status, committed at the
//     backend's headers (served) — or handed back as a pass-through when
//     the backend did not answer as admitted (fallback).
//
// The 503 rule (the Worker's): once a write's body has been read from, the
// request can never be passed on — any failure is the client's 503
// SlowDown + Retry-After 30 (the commit's edge_error gives everything back).

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag of the body
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/crypto"
)

const (
	// commitBound is the commit's own clock, detached from the client's
	// request (RP64-1's rule: a client gone is not a cancel).
	commitBound = 60 * time.Second
	// commitRetry is the wait before the one retry of a lost commit answer
	// (idempotent on the admit id).
	commitRetry = 2 * time.Second
	// codecBatchBytes bounds the chunk plaintext one codec write holds
	// between its cut and its seal (the Worker's CODEC_IN_FLIGHT_BYTES).
	codecBatchBytes = 48 << 20
	// codecPutConcurrency is how many sealed chunks a write PUTs at once.
	codecPutConcurrency = 4
	// retryAfter is the SlowDown's Retry-After.
	retryAfter = "30"
)

// hopByHop are never described to the control plane (the Worker's list).
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

// Host is a Go data-plane host: the pipeline over one ControlPlane and one
// HTTP client to the backends.
type Host struct {
	CP ControlPlane
	// Client reaches the backends' presigned links (nil = a client with no
	// overall timeout: a body may take long; each call has its context).
	Client *http.Client
	// ClientIP is the client's address as this host trusts it (a node: its
	// direct peer; the origin: internal/clientip).
	ClientIP func(*http.Request) string
	// Features this host reads (a node reads no window: none).
	Features []string
	Logger   *zap.Logger
}

// Result is a write's outcome: the S3 answer to relay, or a pass-through
// (the body untouched — the caller takes the request as it would without
// a data plane). Admit and Index are what the control plane answered
// (Index merged over the batches) — for the logs and the tests.
type Result struct {
	Relay       *Relay
	PassThrough bool
	Reason      string
	Admit       AdmitResponse
	Index       IndexResponse
}

// ReadResult is a read's outcome: a pass-through, a relayed refusal, or the
// response to write — Status, Headers (the origin's) and Body (the
// backend's; the caller closes it).
type ReadResult struct {
	PassThrough bool
	Reason      string
	Relay       *Relay
	Status      int
	Headers     map[string]string
	Body        io.ReadCloser
	Admit       AdmitResponse
}

func (h *Host) log() *zap.Logger {
	if h.Logger == nil {
		return zap.NewNop()
	}
	return h.Logger
}

func (h *Host) client() *http.Client {
	if h.Client == nil {
		return NewBackendClient(BackendAnswerTimeout)
	}
	return h.Client
}

// BackendAnswerTimeout is how long a backend may take to answer once the
// host has sent its request (the body included): the origin driver's
// per-PUT deadline (FIXED_BUCKET_PUT_TIMEOUT's 60 s for 64 MiB — a host's
// largest single request is a 64 MiB part or a 16 MiB sealed chunk). A
// backend that takes the body and never answers (Lyve us-east-1, 1–3 % of
// PUTs on 2026-10-17) ends the request as the host's edge_error — the
// client's 503 — never a request that hangs. A GET's body has no
// deadline: it streams as long as the client reads.
const BackendAnswerTimeout = 60 * time.Second

// NewBackendClient is the HTTP client a host reaches the backends' links
// with: the default transport's dialing and pooling, and a deadline on the
// backend's answer after the request was sent.
func NewBackendClient(answer time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = answer
	t.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: t}
}

// newRequestID is a request id for the admit, the logs and the client.
func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Describe builds the admit request of the client's req: every header
// lower-cased minus the hop-by-hop ones (the client's Authorization inside
// the description, never on the call itself), the host, the raw path and
// query, the client's IP, a request id.
func (h *Host) Describe(req *http.Request, op string) AdmitRequest {
	headers := map[string]string{}
	for name, vals := range req.Header {
		n := strings.ToLower(name)
		if hopByHop[n] {
			continue
		}
		headers[n] = strings.Join(vals, ", ")
	}
	headers["host"] = req.Host
	if _, ok := headers["content-length"]; !ok && req.ContentLength >= 0 && len(req.TransferEncoding) == 0 && op != OpGet {
		headers["content-length"] = strconv.FormatInt(req.ContentLength, 10)
	}
	ip := ""
	if h.ClientIP != nil {
		ip = h.ClientIP(req)
	}
	return AdmitRequest{Op: op, Method: req.Method, Host: req.Host, Path: req.URL.EscapedPath(), Query: req.URL.RawQuery,
		Headers: headers, ClientIP: ip, RequestID: newRequestID(), Features: h.Features}
}

// WriteOp is the admit op of a PUT: an UploadPart names exactly
// partNumber and uploadId (aws-sdk-go-v2's x-id aside).
func WriteOp(req *http.Request) string {
	q := req.URL.Query()
	if q.Get("uploadId") != "" && q.Get("partNumber") != "" {
		return OpPart
	}
	return OpPut
}

// Write runs a PutObject or UploadPart: body is the request's body (size
// its Content-Length, -1 unknown — an aws-chunked body sent chunked).
func (h *Host) Write(ctx context.Context, req *http.Request, body io.Reader, size int64) (*Result, error) {
	desc := h.Describe(req, WriteOp(req))
	admit, err := h.CP.Admit(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("admit: %w", err)
	}
	res := &Result{Admit: admit}
	switch {
	case admit.Refusal != nil:
		res.Relay = admit.Refusal
		return res, nil
	case !admit.Edge:
		res.PassThrough, res.Reason = true, admit.Reason
		return res, nil
	case admit.Codec != nil:
		h.codec(ctx, req, body, size, desc, res)
		return res, nil
	}
	h.move(ctx, req, body, size, desc, res)
	return res, nil
}

// move hands the client's body to the backend as it is and commits the
// backend's answer.
func (h *Host) move(ctx context.Context, req *http.Request, body io.Reader, size int64, desc AdmitRequest, res *Result) {
	admit := res.Admit
	started := time.Now()
	commit := CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeEdgeError}
	method := admit.Method
	if method == "" {
		method = http.MethodPut
	}
	breq, err := http.NewRequestWithContext(ctx, method, admit.URL, body)
	if err == nil {
		if admit.Headers != nil {
			// aws-chunked: the header-signed request, the framing untouched
			// (sent chunked when the client sent no length).
			for k, v := range admit.Headers {
				if k != "host" {
					breq.Header.Set(k, v)
				}
			}
			breq.ContentLength = size
		} else {
			for k, v := range admit.SendHeaders {
				if k != "content-length" {
					breq.Header.Set(k, v)
				}
			}
			breq.ContentLength = admit.ContentLengthOf(size)
		}
		if breq.ContentLength == 0 {
			breq.Body = http.NoBody
		}
		var resp *http.Response
		resp, err = h.client().Do(breq)
		if err == nil {
			commit.BackendStatus = resp.StatusCode
			commit.BackendETag = resp.Header.Get("ETag")
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				commit.Outcome = OutcomeOK
			} else {
				commit.Outcome = OutcomeBackendError
				commit.BackendCode = backendErrorCode(b)
			}
		}
	}
	if err != nil {
		commit.BackendCode = truncate(err.Error(), 120)
	}
	commit.DurationMS = time.Since(started).Milliseconds()
	res.Relay = h.commitWrite(ctx, commit, desc)
	h.log().Info("data plane: move", zap.String("op", desc.Op), zap.String("admit_id", admit.AdmitID), zap.Int64("size", size),
		zap.String("outcome", commit.Outcome), zap.Int("backend_status", commit.BackendStatus), zap.Int64("ms", commit.DurationMS))
}

// ContentLengthOf is the body length the presigned link signs (its
// send_headers' content-length), else the request's.
func (a AdmitResponse) ContentLengthOf(size int64) int64 {
	if v, ok := a.SendHeaders["content-length"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return size
}

// codecItem is one chunk waiting for its batch's index answer.
type codecItem struct {
	idx   int
	chunk crypto.Chunk
}

// codec chunks, hashes, indexes, seals and stores the body, then commits
// the chunk list.
func (h *Host) codec(ctx context.Context, req *http.Request, body io.Reader, size int64, desc AdmitRequest, res *Result) {
	admit := res.Admit
	started := time.Now()
	declared := size
	src := body
	if admit.StreamingTrailer != "" {
		declared = admit.ContentLength
		dec, err := NewAWSChunkedDecoder(body, admit.StreamingTrailer, declared)
		if err != nil {
			res.Relay = h.commitWrite(ctx, CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeEdgeError}, desc)
			return
		}
		src = dec
	}
	sum := md5.New() // #nosec G401
	src = io.TeeReader(src, sum)

	entries, idx, n, err := h.codecStore(ctx, admit, src)
	res.Index = idx
	switch {
	case errors.Is(err, ErrChecksumMismatch):
		// The client's body is not what its trailer says: the client's
		// 400, relayed from the control plane, which gives everything back.
		res.Relay = h.commitWrite(ctx, CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeChecksumMismatch}, desc)
		return
	case err == nil && declared >= 0 && n != declared:
		err = fmt.Errorf("the body is %d bytes, %d declared", n, declared)
	}
	if err != nil {
		h.log().Warn("data plane: codec failed — edge_error", zap.Error(err), zap.String("admit_id", admit.AdmitID))
		h.commitWrite(ctx, CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeEdgeError}, desc)
		res.Relay = slowDown(req.URL.Path, desc.RequestID, "the data plane could not store your upload. Retry.")
		return
	}
	commit := CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum.Sum(nil)),
		DurationMS: time.Since(started).Milliseconds()}
	res.Relay = h.commitWrite(ctx, commit, desc)
	h.log().Info("data plane: codec", zap.String("op", desc.Op), zap.String("admit_id", admit.AdmitID), zap.Int64("bytes", n),
		zap.Int("chunks", len(entries)), zap.Int("misses", len(idx.Misses)), zap.Int("hits", len(idx.Hits)), zap.Int64("ms", commit.DurationMS))
}

// codecStore runs the chunker over src and stores the misses; it returns
// the commit's chunk list in body order, the index's answers merged and
// the decoded bytes read.
func (h *Host) codecStore(ctx context.Context, admit AdmitResponse, src io.Reader) ([]CommitChunk, IndexResponse, int64, error) {
	idx := IndexResponse{Hits: []string{}, Misses: []IndexMiss{}}
	chunker, err := crypto.DefaultChunker()
	if err != nil {
		return nil, idx, 0, err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results, err := chunker.ChunkContextDepth(cctx, src, 1)
	if err != nil {
		return nil, idx, 0, err
	}
	var entries []CommitChunk
	seen := map[string]bool{}
	var batch []codecItem
	var batchBytes, total int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		b := batch
		batch, batchBytes = nil, 0
		return h.storeBatch(ctx, admit, b, entries, &idx)
	}
	for r := range results {
		if r.Err != nil {
			// The chunker read the body: a decoder's verdict (a checksum, a
			// framing error) or the client's body breaking off.
			return nil, idx, total, r.Err
		}
		c := r.Chunk
		total += int64(c.Size)
		e := CommitChunk{Hash: c.Hash, Size: int64(c.Size)}
		if seen[c.Hash] {
			// Asked already: the commit settles it as a hit after the first
			// occurrence (it runs in order).
			e.Status = "hit"
			entries = append(entries, e)
			continue
		}
		seen[c.Hash] = true
		entries = append(entries, e)
		if batchBytes+int64(c.Size) > codecBatchBytes {
			if err := flush(); err != nil {
				return nil, idx, total, err
			}
		}
		batch = append(batch, codecItem{idx: len(entries) - 1, chunk: c})
		batchBytes += int64(c.Size)
		if len(batch) >= IndexBatch {
			if err := flush(); err != nil {
				return nil, idx, total, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, idx, total, err
	}
	return entries, idx, total, nil
}

// storeBatch asks the index for one batch and stores its misses (at most
// codecPutConcurrency at once), filling their entries.
func (h *Host) storeBatch(ctx context.Context, admit AdmitResponse, batch []codecItem, entries []CommitChunk, idx *IndexResponse) error {
	ireq := IndexRequest{AdmitID: admit.AdmitID, Chunks: make([]IndexChunk, 0, len(batch))}
	for _, it := range batch {
		ireq.Chunks = append(ireq.Chunks, IndexChunk{Hash: it.chunk.Hash, Size: int64(it.chunk.Size)})
	}
	ans, err := h.CP.Index(ctx, ireq)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	idx.Hits = append(idx.Hits, ans.Hits...)
	idx.Misses = append(idx.Misses, ans.Misses...)
	misses := make(map[string]IndexMiss, len(ans.Misses))
	for _, m := range ans.Misses {
		misses[m.Hash] = m
	}
	sem := make(chan struct{}, codecPutConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, it := range batch {
		m, miss := misses[it.chunk.Hash]
		if !miss {
			entries[it.idx].Status = "hit"
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it codecItem, m IndexMiss) {
			defer wg.Done()
			defer func() { <-sem }()
			etag, sealed, err := h.SealAndStore(ctx, admit.Codec, it.chunk, m)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			entries[it.idx].Status, entries[it.idx].SealedSize, entries[it.idx].BackendETag = "stored", sealed, etag
		}(it, m)
	}
	wg.Wait()
	return firstErr
}

// SealAndStore seals one chunk as a 0x06 blob under the grant's public key
// and PUTs it on its miss link; it returns the backend's ETag and the
// sealed length.
func (h *Host) SealAndStore(ctx context.Context, grant *CodecGrant, c crypto.Chunk, m IndexMiss) (string, int64, error) {
	sealed, err := crypto.SealWrappedChunk(grant.KEMPublicKey, grant.Version, bytes.NewReader(c.Data), int64(c.Size))
	if err != nil {
		return "", 0, fmt.Errorf("seal: %w", err)
	}
	n := crypto.WrappedChunkSealedLength(int64(c.Size))
	if m.SealedLength != 0 && m.SealedLength != n {
		return "", 0, fmt.Errorf("sealed %d bytes, the link signs %d", n, m.SealedLength)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.URL, sealed)
	if err != nil {
		return "", 0, err
	}
	for k, v := range m.SendHeaders {
		if k != "content-length" {
			req.Header.Set(k, v)
		}
	}
	req.ContentLength = n
	resp, err := h.client().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("chunk PUT: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("chunk PUT answered %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag"), n, nil
}

// commitWrite commits on its own clock; a lost answer is retried once.
// A 200 is the client's answer whatever status it carries. An expired or
// unknown admit (404/410), the service channel's 401/403 and no answer at
// all are the client's 503 SlowDown (the body is gone, the request cannot
// be passed on); another 4xx of the control plane's own is the client's
// 400 InvalidRequest naming it — the same upload would fail the same way
// (RP70-7, RP72-3).
func (h *Host) commitWrite(ctx context.Context, req CommitRequest, desc AdmitRequest) *Relay {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitBound)
	defer cancel()
	path := desc.Path
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(commitRetry):
			case <-cctx.Done():
				return slowDown(path, desc.RequestID, "the data plane could not report your upload. Retry.")
			}
		}
		relay, err := h.CP.Commit(cctx, req)
		if err == nil {
			r := relay
			return &r
		}
		var se *StatusError
		if errors.As(err, &se) {
			switch {
			case se.Status == http.StatusNotFound || se.Status == http.StatusGone:
				return slowDown(path, desc.RequestID, "the admission of this upload expired before it was committed. Retry the upload.")
			case se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden:
				return slowDown(path, desc.RequestID, "the data plane could not report your upload. Retry.")
			case se.Status >= 400 && se.Status < 500:
				return &Relay{Status: http.StatusBadRequest, Headers: map[string]string{"Content-Type": "application/xml", "x-amz-request-id": desc.RequestID},
					Body: s3ErrorXML("InvalidRequest", fmt.Sprintf("Your request was invalid. The data plane could not commit this upload (%s); retrying it unchanged will fail the same way.", se.Msg), path, desc.RequestID)}
			}
		}
		h.log().Warn("data plane: commit failed", zap.Int("attempt", attempt), zap.Error(err), zap.String("admit_id", req.AdmitID))
	}
	return slowDown(path, desc.RequestID, "the data plane could not report your upload. Retry.")
}

// Read runs a GetObject.
func (h *Host) Read(ctx context.Context, req *http.Request) (*ReadResult, error) {
	desc := h.Describe(req, OpGet)
	admit, err := h.CP.Admit(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("admit: %w", err)
	}
	res := &ReadResult{Admit: admit}
	switch {
	case admit.Refusal != nil:
		res.Relay = admit.Refusal
		return res, nil
	case !admit.Edge:
		res.PassThrough, res.Reason = true, admit.Reason
		return res, nil
	}
	started := time.Now()
	commit := CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeError}
	breq, err := http.NewRequestWithContext(ctx, http.MethodGet, admit.URL, nil)
	var resp *http.Response
	if err == nil {
		for k, v := range admit.SendHeaders {
			breq.Header.Set(k, v)
		}
		resp, err = h.client().Do(breq)
	}
	switch {
	case err != nil:
		commit.BackendCode = truncate(err.Error(), 120)
	default:
		commit.BackendStatus = resp.StatusCode
		commit.BackendContentLength = resp.ContentLength
		if commit.BackendContentLength < 0 {
			commit.BackendContentLength = 0
		}
		commit.Outcome = OutcomeFallback
		if resp.StatusCode == admit.ExpectStatus && (resp.ContentLength < 0 || admit.ContentLength <= 0 || resp.ContentLength == admit.ContentLength) {
			commit.Outcome = OutcomeServed
		}
	}
	commit.DurationMS = time.Since(started).Milliseconds()
	// The commit at the backend's headers (served meters the backend's
	// Content-Length; fallback and error meter nothing — the caller serves).
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitBound)
	if _, cErr := h.CP.Commit(cctx, commit); cErr != nil {
		h.log().Warn("data plane: read commit failed — the reaper meters the admitted size", zap.Error(cErr), zap.String("admit_id", admit.AdmitID))
	}
	cancel()
	h.log().Info("data plane: read", zap.String("admit_id", admit.AdmitID), zap.String("outcome", commit.Outcome),
		zap.Int("backend_status", commit.BackendStatus), zap.Int64("bytes", commit.BackendContentLength), zap.Int64("ms", commit.DurationMS))
	if commit.Outcome != OutcomeServed {
		if resp != nil {
			_ = resp.Body.Close()
		}
		res.PassThrough, res.Reason = true, "fallback"
		return res, nil
	}
	res.Status = admit.Status
	if res.Status == 0 {
		res.Status = admit.ExpectStatus
	}
	res.Headers = admit.ResponseHeaders
	res.Body = resp.Body
	return res, nil
}

// WriteRelay writes an S3 answer to w.
func WriteRelay(w http.ResponseWriter, r *Relay) {
	for k, v := range r.Headers {
		w.Header().Set(k, v)
	}
	if r.Body != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/xml")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if r.Body != "" {
		_, _ = io.WriteString(w, r.Body)
	}
}

func slowDown(path, requestID, why string) *Relay {
	return &Relay{Status: http.StatusServiceUnavailable,
		Headers: map[string]string{"Content-Type": "application/xml", "Retry-After": retryAfter, "x-amz-request-id": requestID},
		Body:    s3ErrorXML("SlowDown", "Please reduce your request rate: "+why, path, requestID)}
}

// s3ErrorXML renders the S3 error document the origin's writer would.
func s3ErrorXML(code, message, path, requestID string) string {
	esc := html.EscapeString // & < > " ' — valid XML escaping of the client's path
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n<Error><Code>" + esc(code) + "</Code><Message>" + esc(message) +
		"</Message><Resource>" + esc(path) + "</Resource><RequestId>" + esc(requestID) + "</RequestId></Error>"
}

// backendErrorCode reads <Code> off an S3 error body ("" when none).
func backendErrorCode(b []byte) string {
	s := string(b)
	i := strings.Index(s, "<Code>")
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], "</Code>")
	if j < 0 {
		return ""
	}
	return s[i+6 : i+j]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
