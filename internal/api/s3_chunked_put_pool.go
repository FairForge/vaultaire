package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// defaultChunkStoreConcurrency bounds how many chunk stores a single chunked
// PUT runs at once. The store step is a full HTTPS round-trip to the backend
// (~100 ms to iDrive), so the old one-at-a-time loop capped engine-path
// uploads at chunk_size ÷ round-trip (~19 MB/s); 8 workers lift that ceiling
// while keeping peak in-flight memory bounded (workers + chunker buffer,
// ≤ ~19 chunks of ≤ 16 MB). Override via CHUNK_PUT_CONCURRENCY.
const defaultChunkStoreConcurrency = 8

// chunkPutBoundDefault bounds ONE chunk blob's backend PUT, failover included
// (WP-R8-2): a chunk is ≤ 16 MiB, its per-attempt deadline is
// s3PutDeadline's 15 s (10 s floor, 60 s per 64 MiB) with one retry on a
// fresh connection = 30 s per backend, and the engine fails over to at most
// two more general-purpose backends: 90 s.
const chunkPutBoundDefault = 90 * time.Second

// chunkPendingTakeoverDefault is how old a pending claim must be before
// another writer of the chunk takes it over: twice chunkPutBoundDefault. The
// claimer's PUT ran under chunkPutBound from after its claim — detached from
// its request since RP14-1, so a request that ends does not end it — so by
// then it has ended, and a backend that committed a PUT nobody waited for
// (RP8-11) did so within the second bound: the taker's bytes are never
// overwritten by the claimer's late ones.
const chunkPendingTakeoverDefault = 2 * chunkPutBoundDefault

// defaultChunkGetPrefetch caps how many chunks a chunked GET holds at once
// (in flight + fetched + being written); the byte window
// (CHUNK_GET_WINDOW_BYTES, 64 MiB) is the other bound, so at the real ~2 MiB
// average chunk the window is ~32 chunks. It was 4 until 2026-10-10: 8 MiB in
// flight ÷ iDrive's 100–600 ms per chunk = 14–50 MB/s (s3_large_get.go).
// Override via CHUNK_GET_PREFETCH (1 = sequential fetches).
const defaultChunkGetPrefetch = 32

// takenChunkRef identifies one GCI reference taken during a chunked PUT so
// an aborted upload can compensate it (F10).
type takenChunkRef struct{ scope, hash string }

// dupChunkWaiter is a later occurrence of a hash whose first occurrence is
// still being stored by a worker; it takes its reference when that store
// commits instead of racing it into a duplicate store.
type dupChunkWaiter struct {
	index  int
	offset int64
}

type chunkStoreJob struct {
	chunk crypto.Chunk
	held  int64 // bytes of the write's budget hold assigned to this chunk
}

type chunkStoreOutcome struct {
	chunk          crypto.Chunk
	backend        string
	storedBytes    int64
	ciphertextHash string
	keyVersion     *int // the tenant key version the stored blob is sealed under (nil: unencrypted)
	err            error
}

// chunkStorePool fans new-chunk stores out to a bounded worker set. The
// dispatcher (the request goroutine in handleChunkedPut) keeps every dedup
// decision sequential; workers only compress, encrypt, and run
// storeChunkLocked. Same-hash races within one upload are prevented by the
// inFlight map: the dispatcher registers later occurrences as waiters, and
// the collector grants their references after the store commits. Cross-
// request races are already serialized by storeChunkLocked's advisory lock.
type chunkStorePool struct {
	a           *S3ToEngine
	ctx         context.Context
	cancel      context.CancelFunc
	refTenantID string // tenant ID recorded on manifest rows
	encTenantID string // tenant ID used for convergent keys + storage keys (t.ID)
	bucket      string
	artifact    string
	scope       string
	contentType string
	encrypting  bool

	jobs          chan chunkStoreJob
	results       chan chunkStoreOutcome
	wg            sync.WaitGroup
	collectorDone chan struct{}

	mu           sync.Mutex
	firstErr     error
	physicalSize int64
	backendName  string
	takenRefs    []takenChunkRef
	newRefs      []crypto.TenantChunkRef
	inFlight     map[string][]dupChunkWaiter

	// hold is the write's share of the process budget (WP-R2-6); nil =
	// unaccounted (a pool built without one).
	hold *chunkPutHold
}

func newChunkStorePool(
	ctx context.Context, a *S3ToEngine, cancel context.CancelFunc,
	refTenantID, encTenantID, bucket, artifact, scope, contentType string,
	encrypting bool,
) *chunkStorePool {
	workers := a.chunkStoreConcurrency
	if workers < 1 {
		workers = 1
	}
	p := &chunkStorePool{
		a:             a,
		ctx:           ctx,
		cancel:        cancel,
		refTenantID:   refTenantID,
		encTenantID:   encTenantID,
		bucket:        bucket,
		artifact:      artifact,
		scope:         scope,
		contentType:   contentType,
		encrypting:    encrypting,
		jobs:          make(chan chunkStoreJob),
		results:       make(chan chunkStoreOutcome),
		collectorDone: make(chan struct{}),
		newRefs:       make([]crypto.TenantChunkRef, 0, 16),
		inFlight:      make(map[string][]dupChunkWaiter),
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	go p.collect()
	return p
}

// fail records the first real error and cancels in-flight work. Later errors
// are cascades of that cancellation and are dropped — but a cancelled
// sibling can report before the cause does (its fall-over to the next
// backend dies with context.Canceled while the cause is still on its way to
// the collector), so a cancellation is replaced by the first error that is
// not one: the handler maps the cause (a backend that could not be asked →
// 503 + Retry-After), never the sibling's "context canceled" → 500 (prod
// 2026-10-11 03:51, six 500s on a 1 GiB chunked PUT; Prompt 2b.5 F1.3).
func (p *chunkStorePool) fail(err error) {
	p.mu.Lock()
	if p.firstErr == nil || (errors.Is(p.firstErr, context.Canceled) && !errors.Is(err, context.Canceled)) {
		p.firstErr = err
	}
	p.mu.Unlock()
	p.cancel()
}

func (p *chunkStorePool) failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstErr != nil
}

// noteDuplicate registers the chunk as a waiter when its hash is already
// being stored by a worker. Returns true if registered — the dispatcher
// then skips normal processing; the collector grants the reference once
// the in-flight store commits.
func (p *chunkStorePool) noteDuplicate(c *crypto.Chunk) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	waiters, ok := p.inFlight[c.Hash]
	if !ok {
		return false
	}
	p.inFlight[c.Hash] = append(waiters, dupChunkWaiter{index: c.Index, offset: c.Offset})
	return true
}

// addRef records one taken GCI reference and its manifest row. keyVersion
// is the version of the BLOB the reference points at — the row's, copied
// exactly like its ciphertext hash, never the tenant's current version
// (WP-R8-1); nil (an unencrypted chunk) records the column's default.
func (p *chunkStorePool) addRef(index int, offset int64, hash string, ciphertextHash *string, keyVersion *int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.takenRefs = append(p.takenRefs, takenChunkRef{scope: p.scope, hash: hash})
	v := 1
	if keyVersion != nil {
		v = *keyVersion
	}
	p.newRefs = append(p.newRefs, crypto.TenantChunkRef{
		TenantID:             p.refTenantID,
		BucketName:           p.bucket,
		ObjectKey:            p.artifact,
		ChunkIndex:           index,
		ChunkOffset:          offset,
		PlaintextHash:        hash,
		DedupScope:           p.scope,
		EncryptionKeyVersion: v,
		CiphertextHash:       ciphertextHash,
	})
}

// submit marks the hash in flight and hands the chunk to a worker. Blocks
// when all workers are busy — that bound, plus the chunker's own buffer, is
// what keeps a chunked PUT's peak memory independent of object size.
func (p *chunkStorePool) submit(c crypto.Chunk, held int64) {
	p.mu.Lock()
	p.inFlight[c.Hash] = []dupChunkWaiter{}
	p.mu.Unlock()
	select {
	case p.jobs <- chunkStoreJob{chunk: c, held: held}:
	case <-p.ctx.Done():
		// The upload is over: the workers may be finishing PUTs that
		// outlived it (RP14-1) — never wait on them for a chunk nobody
		// will store.
		p.mu.Lock()
		delete(p.inFlight, c.Hash)
		p.mu.Unlock()
		p.hold.release(held)
	}
}

func (p *chunkStorePool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		p.results <- p.storeOne(job)
	}
}

// storeOne compresses, encrypts, and stores one new chunk — the body of the
// old sequential loop's mustStore branch, unchanged in order and semantics.
func (p *chunkStorePool) storeOne(job chunkStoreJob) chunkStoreOutcome {
	chunk := &job.chunk
	// The chunk's share of the write's budget hold (WP-R2-6): shrunk to its
	// buffers' real size once compressed and sealed, given back when the
	// store ends — or, for a PUT that outlives the request, when it ends.
	var lease *chunkLease
	if p.hold != nil {
		lease = &chunkLease{h: p.hold, left: job.held}
	}
	defer lease.releaseAll()
	// A chunk not yet started when the upload ended is never attempted
	// (RP14-1): no claim, no PUT.
	if err := p.ctx.Err(); err != nil {
		return chunkStoreOutcome{chunk: job.chunk, err: fmt.Errorf("chunk %s not attempted, the upload ended: %w", chunk.Hash[:16], err)}
	}
	storeData := chunk.Data
	var compressedSize *int64
	var compressionAlgo *string
	var encrypted bool
	var encryptionAlgo *string
	var ciphertextHash string
	var keyVersion *int

	if crypto.ShouldCompress(chunk.Data, p.contentType) {
		compressed, compErr := crypto.CompressBuffer(chunk.Data)
		if compErr == nil && len(compressed) < chunk.Size {
			storeData = compressed
			cs := int64(len(compressed))
			compressedSize = &cs
			algo := "zstd"
			compressionAlgo = &algo
		}
	}

	if p.a.chunkEncSvc != nil {
		ct, ctHash, kv, encErr := p.a.chunkEncSvc.EncryptChunkData(p.ctx, p.encTenantID, chunk.Hash, storeData)
		if encErr != nil {
			return chunkStoreOutcome{chunk: job.chunk,
				err: fmt.Errorf("encrypt chunk %s: %w", chunk.Hash[:16], encErr)}
		}
		storeData = ct
		ciphertextHash = ctHash
		keyVersion = &kv
		encrypted = true
		algo := "AES256-CE"
		encryptionAlgo = &algo
	}

	storageKey := "_chunks/" + chunk.Hash
	if p.encrypting {
		storageKey = "_chunks/" + p.encTenantID + "/" + chunk.Hash
	}
	var entryCiphertextHash *string
	if ciphertextHash != "" {
		entryCiphertextHash = &ciphertextHash
	}
	// What this chunk really holds now: the chunk, its compressed form when
	// one was kept, its ciphertext when sealed.
	actual := int64(len(chunk.Data))
	if compressedSize != nil {
		actual += *compressedSize
	}
	if encrypted {
		actual += int64(len(storeData))
	}
	if lease != nil {
		lease.release(job.held - actual)
	}

	res, storeErr := p.a.storeChunkLocked(p.ctx, p.scope, storageKey, storeData, lease, &crypto.GCIEntry{
		DedupScope:      p.scope,
		PlaintextHash:   chunk.Hash,
		StorageKey:      storageKey,
		SizeBytes:       int64(chunk.Size),
		CompressedSize:  compressedSize,
		CompressionAlgo: compressionAlgo,
		Encrypted:       encrypted,
		EncryptionAlgo:  encryptionAlgo,
		CiphertextHash:  entryCiphertextHash,
		RefCount:        1,
		// The version is a property of the blob, recorded with it (084).
		EncryptionKeyVersion: keyVersion,
	})
	if storeErr != nil {
		return chunkStoreOutcome{chunk: job.chunk,
			err: fmt.Errorf("store chunk %s: %w", chunk.Hash[:16], storeErr)}
	}
	// A concurrent first-store won the lock: the blob on disk is THEIRS, so
	// the manifest carries the row's ciphertext hash (not the one computed
	// here for bytes that were never stored) and no physical bytes are
	// attributed to this upload (R8-02).
	storedBytes := int64(len(storeData))
	if res.reused {
		storedBytes = 0
		ciphertextHash = ""
		if res.ciphertextHash != nil {
			ciphertextHash = *res.ciphertextHash
		}
		keyVersion = res.keyVersion // the stored blob's, not the one sealed here
	}
	return chunkStoreOutcome{
		chunk:          job.chunk,
		backend:        res.backend,
		storedBytes:    storedBytes,
		ciphertextHash: ciphertextHash,
		keyVersion:     keyVersion,
	}
}

// collect accounts every store outcome. Successful stores are recorded even
// after a failure elsewhere — their references were taken and the abort
// compensator must see them. Waiters on a committed hash take their
// references here, after the store's transaction is durable.
func (p *chunkStorePool) collect() {
	defer close(p.collectorDone)
	for out := range p.results {
		if out.err != nil {
			p.mu.Lock()
			delete(p.inFlight, out.chunk.Hash)
			p.mu.Unlock()
			p.fail(out.err)
			continue
		}

		p.mu.Lock()
		p.physicalSize += out.storedBytes
		if p.backendName == "" {
			p.backendName = out.backend
		}
		waiters := p.inFlight[out.chunk.Hash]
		delete(p.inFlight, out.chunk.Hash)
		p.mu.Unlock()

		var ctHash *string
		if out.ciphertextHash != "" {
			h := out.ciphertextHash
			ctHash = &h
		}
		p.addRef(out.chunk.Index, out.chunk.Offset, out.chunk.Hash, ctHash, out.keyVersion)
		for _, wtr := range waiters {
			rows, incErr := p.a.gci.IncrementRef(p.ctx, p.scope, out.chunk.Hash)
			if incErr != nil {
				p.fail(fmt.Errorf("increment ref %s: %w", out.chunk.Hash[:16], incErr))
				break
			}
			if rows == 0 {
				// Cannot happen while this upload holds a reference (GC only
				// sweeps ref_count 0) — treat as corruption, not dedup.
				p.fail(fmt.Errorf("chunk %s vanished while upload held a reference", out.chunk.Hash[:16]))
				break
			}
			p.addRef(wtr.index, wtr.offset, out.chunk.Hash, ctHash, out.keyVersion)
		}
	}
}

// join closes the job feed, waits for workers and the collector, and returns
// the first error. After join returns, all pool state is quiescent.
func (p *chunkStorePool) join() error {
	close(p.jobs)
	p.wg.Wait()
	close(p.results)
	<-p.collectorDone
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstErr
}

// sortedRefs returns the manifest rows in chunk-index order — workers finish
// out of order, but the manifest contract is index-ascending.
func (p *chunkStorePool) sortedRefs() []crypto.TenantChunkRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	refs := append([]crypto.TenantChunkRef(nil), p.newRefs...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].ChunkIndex < refs[j].ChunkIndex })
	return refs
}

// compensationRefs snapshots every reference taken so far, for the aborted-
// PUT compensator (F10).
func (p *chunkStorePool) compensationRefs() []takenChunkRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]takenChunkRef(nil), p.takenRefs...)
}
