package api

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// defaultChunkStoreConcurrency bounds how many chunk stores a single chunked
// PUT runs at once. The store step is a full HTTPS round-trip to the backend
// (~100 ms to iDrive), so the old one-at-a-time loop capped engine-path
// uploads at chunk_size ÷ round-trip (~19 MB/s); 8 workers lift that ceiling
// while keeping peak in-flight memory bounded (workers + chunker buffer,
// ≤ ~19 chunks of ≤ 16 MB). Override via CHUNK_PUT_CONCURRENCY.
const defaultChunkStoreConcurrency = 8

// defaultChunkGetPrefetch bounds how many chunks a chunked GET fetches ahead
// of the write cursor. Each buffered chunk holds up to 16 MB and GETs are
// typically more concurrent than PUTs, so the default is smaller than the
// store pool's. Override via CHUNK_GET_PREFETCH.
const defaultChunkGetPrefetch = 4

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

type chunkStoreJob struct{ chunk crypto.Chunk }

type chunkStoreOutcome struct {
	chunk          crypto.Chunk
	backend        string
	storedBytes    int64
	ciphertextHash string
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
}

func newChunkStorePool(
	a *S3ToEngine, ctx context.Context, cancel context.CancelFunc,
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

// fail records the first error and cancels in-flight work. Later errors are
// dropped — they are almost always cascades of the first cancellation.
func (p *chunkStorePool) fail(err error) {
	p.mu.Lock()
	if p.firstErr == nil {
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

// addRef records one taken GCI reference and its manifest row.
func (p *chunkStorePool) addRef(index int, offset int64, hash string, ciphertextHash *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.takenRefs = append(p.takenRefs, takenChunkRef{scope: p.scope, hash: hash})
	p.newRefs = append(p.newRefs, crypto.TenantChunkRef{
		TenantID:             p.refTenantID,
		BucketName:           p.bucket,
		ObjectKey:            p.artifact,
		ChunkIndex:           index,
		ChunkOffset:          offset,
		PlaintextHash:        hash,
		DedupScope:           p.scope,
		EncryptionKeyVersion: 1,
		CiphertextHash:       ciphertextHash,
	})
}

// submit marks the hash in flight and hands the chunk to a worker. Blocks
// when all workers are busy — that bound, plus the chunker's own buffer, is
// what keeps a chunked PUT's peak memory independent of object size.
func (p *chunkStorePool) submit(c crypto.Chunk) {
	p.mu.Lock()
	p.inFlight[c.Hash] = []dupChunkWaiter{}
	p.mu.Unlock()
	p.jobs <- chunkStoreJob{chunk: c}
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
	storeData := chunk.Data
	var compressedSize *int64
	var compressionAlgo *string
	var encrypted bool
	var encryptionAlgo *string
	var ciphertextHash string

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
		ct, ctHash, encErr := p.a.chunkEncSvc.EncryptChunkData(p.encTenantID, chunk.Hash, storeData)
		if encErr != nil {
			return chunkStoreOutcome{chunk: job.chunk,
				err: fmt.Errorf("encrypt chunk %s: %w", chunk.Hash[:16], encErr)}
		}
		storeData = ct
		ciphertextHash = ctHash
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

	bn, storeErr := p.a.storeChunkLocked(p.ctx, p.scope, storageKey, storeData, &crypto.GCIEntry{
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
	})
	if storeErr != nil {
		return chunkStoreOutcome{chunk: job.chunk,
			err: fmt.Errorf("store chunk %s: %w", chunk.Hash[:16], storeErr)}
	}
	return chunkStoreOutcome{
		chunk:          job.chunk,
		backend:        bn,
		storedBytes:    int64(len(storeData)),
		ciphertextHash: ciphertextHash,
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
		p.addRef(out.chunk.Index, out.chunk.Offset, out.chunk.Hash, ctHash)
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
			p.addRef(wtr.index, wtr.offset, out.chunk.Hash, ctHash)
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
