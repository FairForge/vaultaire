package drivers

// The permafrost fleet's transfers (WP-FLEET-4, docs/reviews/WP-FLEET-NEXT-
// PROMPT.md Part 2; measured 2026-11-05, docs/FLEET.md):
//
//   - D-F4-1 a large Get reads 32 MiB ranges (64 MiB from 1 GiB): the
//     per-user request budget is what one drive meets first, and a 256 MB
//     object cost 32 requests at 8 MiB (now 8). A Get holds at most
//     odRangeWindowMax of ranges, and every fleet Get shares odRangeBudget
//     past its first range — under load a Get degrades to fewer ranges,
//     never more memory.
//   - D-F4-2 a body the driver can READ AGAIN (an io.ReaderAt: the
//     materialize spool, a bytes.Reader) of up to 200 MiB is ONE simple PUT,
//     retried with the burst attempts — each attempt reads its own section,
//     so a lingering earlier attempt cannot disturb it. A STREAMED body (the
//     parity shards' pipes, a client body) keeps the upload session: one
//     request it could not re-send would lose the shard to a 429 burst,
//     which WP-FLEET-3 exists to ride. A streamed body of at most 4 MiB is
//     read into memory and sent as a simple PUT, as before.
//   - D-F4-3 a session's fragments are 180 × 320 KiB (odChunkSize) and
//     DOUBLE-BUFFERED: the next fragment is read from the source while one
//     is in flight (a pipelined session from a client body wrote 24.6 MB/s
//     against 7.6 read-then-send). Two pooled buffers per upload.
//   - D-F4-4 every write is checked by quickXorHash: the body is hashed as
//     it streams (quickxor.go, #1169) and compared with the item Graph
//     answers (the simple PUT's, or the final fragment's). A mismatch, or a
//     size other than the one sent, deletes the item for good and fails the
//     Put with engine.ErrObjectChanged; counted in
//     vaultaire_permafrost_hash_mismatch_total{account}. An answer that
//     names no hash (OneDrive for Business always sends one) is accepted.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// odSimplePutMax is the largest re-readable body sent as one simple PUT
	// (Microsoft allows 250 MB).
	odSimplePutMax = 200 << 20
	// odRangeWindowMax bounds the ranges one Get holds (8 × 32 MiB, 4 × 64).
	odRangeWindowMax = 256 << 20
	// odRangeBudgetBytes is what every fleet Get together may hold past
	// each one's first range.
	odRangeBudgetBytes = 1 << 30
)

// odRangeBudget is the fleet Gets' shared read-ahead budget.
var odRangeBudget = parfetch.NewBudget(odRangeBudgetBytes)

// odRangePartFor is a large Get's range size (D-F4-1).
func odRangePartFor(size int64) int64 {
	switch {
	case size >= 1<<30:
		return odRangePartHuge
	case size >= 2*odRangePartLarge:
		return odRangePartLarge
	}
	return odRangePart
}

var permafrostHashMismatch = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_permafrost_hash_mismatch_total",
	Help: "Permafrost writes whose item, as Graph answered it, did not match what was sent (quickXorHash or size); the item is deleted for good and the Put fails (WP-FLEET-4 D-F4-4).",
}, []string{"account"})

// odItem is the part of a driveItem a write is checked against.
type odItem struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
	File struct {
		Hashes struct {
			QuickXorHash string `json:"quickXorHash"`
		} `json:"hashes"`
	} `json:"file"`
}

// decodeODItem reads an item from an answer (at most 1 MiB of JSON).
func decodeODItem(r io.Reader) (*odItem, error) {
	var it odItem
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&it); err != nil {
		return nil, fmt.Errorf("decode the item: %w", err)
	}
	return &it, nil
}

// upload writes size bytes of data to path: one simple PUT when the body can
// be read again (or is small), else a session (D-F4-2).
func (t *odTenant) upload(ctx context.Context, driveID, parentID, path string, data io.Reader, size int64) error {
	if ra, ok := data.(io.ReaderAt); ok && size <= odSimplePutMax {
		start := int64(0)
		if sk, ok := data.(io.Seeker); ok {
			if off, err := sk.Seek(0, io.SeekCurrent); err == nil {
				start = off
			}
		}
		return t.simpleUploadAt(ctx, driveID, parentID, path, ra, start, size, true)
	}
	if size <= simpleUploadMax {
		buf := make([]byte, size)
		if _, err := io.ReadFull(t.pacedSource(ctx, data), buf); err != nil {
			// A body shorter than its length is an error, never a shorter file.
			return fmt.Errorf("onedrive read body: %w", err)
		}
		return t.simpleUploadAt(ctx, driveID, parentID, path, bytes.NewReader(buf), 0, size, false)
	}
	return t.uploadResumable(ctx, driveID, parentID, path, data, size)
}

// simpleUploadAt is one simple PUT of [start, start+size) of ra, with the
// burst attempts; each attempt reads its own section and hashes it, and the
// item answered is checked against the hash of the attempt that produced it.
// paced: the bytes are taken from the account's and tenant's ingress as they
// are read (a retry takes them again — they are sent again).
func (t *odTenant) simpleUploadAt(ctx context.Context, driveID, parentID, path string, ra io.ReaderAt, start, size int64, paced bool) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/content",
		graphBase, driveID, parentID, odEscapePath(path))
	var (
		mu  sync.Mutex
		cur hash.Hash
	)
	body := func() io.ReadCloser {
		h := NewQuickXorHash()
		mu.Lock()
		cur = h
		mu.Unlock()
		var r io.Reader = io.NewSectionReader(ra, start, size)
		if paced {
			r = t.pacedSource(ctx, r)
		}
		return io.NopCloser(io.TeeReader(r, h))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body())
	if err != nil {
		return fmt.Errorf("onedrive simple upload: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.GetBody = func() (io.ReadCloser, error) { return body(), nil }
	// A body past simpleUploadMax goes over the upload transport: HTTP/1.1,
	// the fragments' — Go's HTTP/2 flow control held a 200 MiB simple PUT to
	// ~10 MB/s per object (2026-11-07), and its answer bound (2 min after the
	// body) fits a large body where Graph's 15 s did not.
	client := t.graphHTTP
	if size > simpleUploadMax && t.uploadHTTP != nil {
		client = t.uploadHTTP
	}
	resp, err := t.graphDoWith(ctx, client, req, odBurstAttempts(ctx), odBurstMaxWait)
	if err != nil {
		return fmt.Errorf("onedrive simple upload: %w", err)
	}
	item, derr := decodeODItem(resp.Body)
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	if derr != nil {
		item = nil // read back by its path instead
	}
	mu.Lock()
	sum := cur.Sum(nil)
	mu.Unlock()
	return t.verifyWrite(ctx, driveID, path, item, size, sum)
}

// verifyWrite checks the written item against what was sent (D-F4-4); item
// nil = read it by its path. A mismatch deletes the item for good.
func (t *odTenant) verifyWrite(ctx context.Context, driveID, path string, item *odItem, size int64, sum []byte) error {
	if item == nil {
		got, err := t.getItem(ctx, driveID, path)
		if err != nil {
			return fmt.Errorf("onedrive verify %s: %w", path, err)
		}
		item = got
	}
	want := base64.StdEncoding.EncodeToString(sum)
	got := item.File.Hashes.QuickXorHash
	if got == "" || (got == want && item.Size == size) {
		return nil // no hash in the answer: nothing to compare against
	}
	permafrostHashMismatch.WithLabelValues(t.name).Inc()
	note := "deleted"
	if err := t.deleteItem(context.WithoutCancel(ctx), driveID, item.ID); err != nil {
		note = "NOT deleted: " + err.Error()
	}
	return fmt.Errorf("%w: onedrive %s: the drive holds quickXorHash %q, %d bytes; %q, %d bytes were sent (the item %s)",
		engine.ErrObjectChanged, path, got, item.Size, want, size, note)
}

// getItem reads an item's id, size and hash by its path.
func (t *odTenant) getItem(ctx context.Context, driveID, path string) (*odItem, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s?$select=id,size,file",
		graphBase, driveID, odRootFolder, odEscapePath(path))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeODItem(resp.Body)
}

// uploadResumable is streamingChunkedUpload with the one restart D-F3-3
// allows: a session that is gone (the fragment's attempts exhausted and the
// session answering 404) starts over in a new session only when the body can
// be rewound (an io.Seeker); a streamed body fails as it did.
func (t *odTenant) uploadResumable(ctx context.Context, driveID, parentID, path string, data io.Reader, totalSize int64) error {
	err := t.streamingChunkedUpload(ctx, driveID, parentID, path, data, totalSize)
	if !errors.Is(err, errODSessionGone) {
		return err
	}
	seeker, ok := data.(io.Seeker)
	if !ok {
		permafrostFragmentResumes.WithLabelValues("failed").Inc()
		return err
	}
	if _, serr := seeker.Seek(0, io.SeekStart); serr != nil {
		permafrostFragmentResumes.WithLabelValues("failed").Inc()
		return fmt.Errorf("%w (rewind for a new session: %w)", err, serr)
	}
	permafrostFragmentResumes.WithLabelValues("restarted").Inc()
	if rerr := t.streamingChunkedUpload(ctx, driveID, parentID, path, data, totalSize); rerr != nil {
		return fmt.Errorf("onedrive upload restarted after its session was gone: %w", rerr)
	}
	return nil
}

// odFrag is one fragment handed from the reader to the sender.
type odFrag struct {
	bp  *[]byte
	n   int
	off int64
}

// streamingChunkedUpload uploads totalSize bytes of data in one session,
// double-buffered (D-F4-3): the reader fills one pooled fragment buffer from
// the source while the sender has the other in flight. The source is read
// through the account's and its tenant's ingress (back-pressure on the
// source) and hashed as it is read; a fragment that exhausts its attempts is
// resumed from the session (resumeFragment, D-F3-3); the final answer's
// item is checked (verifyWrite, D-F4-4).
func (t *odTenant) streamingChunkedUpload(ctx context.Context, driveID, parentID, path string, data io.Reader, totalSize int64) error {
	sessionURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/createUploadSession",
		graphBase, driveID, parentID, odEscapePath(path))
	sb, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(sb))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sb)), nil }
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive create upload session: %w", err)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&session)
	_ = resp.Body.Close()
	if session.UploadURL == "" {
		return fmt.Errorf("onedrive: empty upload session URL")
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := NewQuickXorHash()
	src := io.TeeReader(t.pacedSource(sctx, data), h)

	free := make(chan *[]byte, 2)
	for i := 0; i < 2; i++ {
		free <- odChunkPool.Get().(*[]byte)
	}
	work := make(chan odFrag) // unbuffered: one fragment in flight, one being read
	var item *odItem
	sendErr := make(chan error, 1)
	go func() {
		var err error
		for f := range work {
			if err == nil {
				frag := (*f.bp)[:f.n]
				it, perr := t.putChunk(sctx, session.UploadURL, frag, int(f.off), int(f.off)+f.n-1, int(totalSize))
				if perr != nil {
					it, perr = t.resumeFragment(sctx, session.UploadURL, frag, f.off, totalSize, perr)
				}
				switch {
				case perr != nil:
					err = fmt.Errorf("onedrive streaming chunk at offset %d: %w", f.off, perr)
					cancel()
				case it != nil:
					item = it
				}
			}
			free <- f.bp
		}
		sendErr <- err
	}()

	var readErr error
	offset := int64(0)
read:
	for offset < totalSize {
		var bp *[]byte
		select {
		case bp = <-free:
		case <-sctx.Done():
			break read
		}
		toRead := min(int64(odChunkSize), totalSize-offset)
		n, err := io.ReadFull(src, (*bp)[:toRead])
		if err != nil {
			free <- bp
			// A body shorter than its length is an error, never a shorter file.
			readErr = fmt.Errorf("onedrive read chunk at offset %d: %w", offset, err)
			cancel()
			break
		}
		select {
		case work <- odFrag{bp: bp, n: n, off: offset}:
		case <-sctx.Done():
			free <- bp
			break read
		}
		offset += int64(n)
	}
	close(work)
	err = <-sendErr
	for i := 0; i < 2; i++ {
		odChunkPool.Put(<-free)
	}
	switch {
	case err != nil:
		return err
	case readErr != nil:
		return readErr
	case ctx.Err() != nil:
		return fmt.Errorf("onedrive upload: %w", ctx.Err())
	}
	return t.verifyWrite(ctx, driveID, path, item, totalSize, h.Sum(nil))
}

// errODSessionGone is an upload session that answered 404 to its query.
var errODSessionGone = errors.New("onedrive: upload session is gone")

// resumeFragment is D-F3-3: the fragment at offset exhausted its attempts
// (cause) while the session may still be open. The session is asked what it
// holds (GET the upload URL: nextExpectedRanges) and, once, the rest of the
// fragment is sent from the buffer; the session's view wins — a fragment it
// already holds (the answer was lost) is skipped. The item is the final
// answer when this was the last fragment (nil: verifyWrite reads it). A
// session that is gone is errODSessionGone (the caller may restart a
// rewindable body); anything else is the cause.
func (t *odTenant) resumeFragment(ctx context.Context, uploadURL string, frag []byte, offset, total int64, cause error) (*odItem, error) {
	if ctx.Err() != nil {
		return nil, cause
	}
	next, err := t.querySession(ctx, uploadURL)
	end := offset + int64(len(frag))
	switch {
	case errors.Is(err, errODSessionGone):
		return nil, fmt.Errorf("%w (%w)", errODSessionGone, cause)
	case err != nil:
		permafrostFragmentResumes.WithLabelValues("failed").Inc()
		return nil, fmt.Errorf("%w (session query: %w)", cause, err)
	case next == end:
		permafrostFragmentResumes.WithLabelValues("skipped").Inc()
		return nil, nil
	case next < offset || next > end:
		permafrostFragmentResumes.WithLabelValues("failed").Inc()
		return nil, fmt.Errorf("%w (the session expects byte %d, the fragment is %d-%d)", cause, next, offset, end-1)
	}
	item, err := t.putChunk(ctx, uploadURL, frag[next-offset:], int(next), int(end)-1, int(total))
	if err != nil {
		permafrostFragmentResumes.WithLabelValues("failed").Inc()
		return nil, fmt.Errorf("%w (resumed at %d: %w)", cause, next, err)
	}
	permafrostFragmentResumes.WithLabelValues("resumed").Inc()
	return item, nil
}

// querySession asks an upload session for the first byte it expects
// (nextExpectedRanges[0]'s start). The URL is pre-authenticated; the Graph
// transport bounds the answer (15 s, a metadata call). A session that holds
// every byte answers no range: the total.
func (t *odTenant) querySession(ctx context.Context, uploadURL string) (int64, error) {
	if err := t.paceRequest(ctx); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uploadURL, nil)
	if err != nil {
		return 0, fmt.Errorf("onedrive session query: %w", err)
	}
	req.Header.Set("User-Agent", odUserAgent())
	resp, err := t.graphHTTP.Do(req)
	t.totalReqs.Add(1)
	if err != nil {
		return 0, fmt.Errorf("onedrive session query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return 0, errODSessionGone
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		t.hold("session", odRetryAfter(resp.Header))
		return 0, fmt.Errorf("onedrive session query: HTTP %d (throttled)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("onedrive session query: HTTP %d", resp.StatusCode)
	}
	var out struct {
		NextExpectedRanges []string `json:"nextExpectedRanges"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return 0, fmt.Errorf("onedrive session query: %w", err)
	}
	if len(out.NextExpectedRanges) == 0 {
		return 0, errors.New("onedrive session query: no nextExpectedRanges")
	}
	first, _, _ := strings.Cut(out.NextExpectedRanges[0], "-")
	n, err := strconv.Atoi(first)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("onedrive session query: nextExpectedRanges %q", out.NextExpectedRanges[0])
	}
	return int64(n), nil
}
