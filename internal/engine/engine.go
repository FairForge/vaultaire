// internal/engine/engine.go
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"go.uber.org/zap"
)

// CoreEngine implements the Engine interface: named drivers, a configured
// primary, breaker-based failover and the object→backend routing cache. The
// read cache, access-pattern tracker, cost optimizer, health-score selector,
// backup replication and age-based tiering that used to hang off this struct
// were inert in production and were removed in Review R15 (WP-R6-4/5/6).
type CoreEngine struct {
	drivers map[string]Driver
	primary string

	logger   *zap.Logger
	db       *sql.DB
	failover *FailoverManager

	// Where an object is, is not the engine's to remember (WP-R6-1): every
	// call names the backend the object's row records on its context
	// (WithRecordedBackend). The in-memory route map (objectBackends, R6-09:
	// unbounded) and the object_locations second truth (R6-14) are gone.

	// writeFailures counts PUTs that failed on every eligible durable
	// backend (WP-F fail-loudly). Exposed via GetMetrics for alerting.
	writeFailures atomic.Int64

	mu     sync.RWMutex
	config *Config
}

// Config holds engine configuration.
type Config struct {
	DefaultBackend string
}

// NewEngine creates a new engine. The database is optional (dev mode): with
// nil the location store degrades to a no-op.
func NewEngine(db *sql.DB, logger *zap.Logger, config *Config) *CoreEngine {
	if config == nil {
		config = &Config{DefaultBackend: "local"}
	}

	e := &CoreEngine{
		drivers:  make(map[string]Driver),
		primary:  config.DefaultBackend,
		db:       db,
		logger:   logger,
		config:   config,
		failover: NewFailoverManager(logger),
	}
	return e
}

// AddDriver adds a storage driver
func (e *CoreEngine) AddDriver(name string, driver Driver) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.drivers[name] = driver
	if e.primary == "" {
		e.primary = name
	}
	e.failover.Register(name)
	e.logger.Info("driver added",
		zap.String("name", name),
		zap.Bool("is_primary", e.primary == name))
}

// SetPrimary sets the primary driver
func (e *CoreEngine) SetPrimary(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.primary = name
}

// GetDriverNames returns a sorted list of registered driver names.
func (e *CoreEngine) GetDriverNames() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.drivers))
	for name := range e.drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetPrimary returns the current primary backend name.
func (e *CoreEngine) GetPrimary() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.primary
}

// CheckDriver runs HealthCheck on a single named driver.
func (e *CoreEngine) CheckDriver(ctx context.Context, name string) error {
	e.mu.RLock()
	driver, exists := e.drivers[name]
	e.mu.RUnlock()
	if !exists {
		return fmt.Errorf("driver %q not found", name)
	}
	return driver.HealthCheck(ctx)
}

// Get retrieves an artifact from the backend its row records — the
// context's WithRecordedBackend, else the primary (a row with no backend on
// record) — and from no other (WP-R6-1, recorded_backend.go): a miss there
// is a miss, a backend that cannot be asked is ErrAllBackendsUnavailable.
func (e *CoreEngine) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if err := e.requireTenant(ctx, "get", container, artifact); err != nil {
		return nil, err
	}
	candidates, err := e.readCandidates(e.preferredBackend(ctx, container, artifact), "get")
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", container, artifact, err)
	}

	var reader io.ReadCloser
	usedBackend, err := e.failover.ExecuteOp(ctx, "get", candidates, func(driverName string) error {
		d, ok := e.drivers[driverName]
		if !ok {
			return fmt.Errorf("driver %s not found", driverName)
		}
		var getErr error
		reader, getErr = d.Get(ctx, container, artifact)
		return getErr
	})

	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", container, artifact, err)
	}
	common.SetBackendUsed(ctx, usedBackend)

	return reader, nil
}

// GetRange reads a byte range directly from the backend without downloading
// the full object — the backend Get would ask, and no other. Falls back to
// full Get + discard if the driver doesn't implement RangeGetter.
func (e *CoreEngine) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	if err := e.requireTenant(ctx, "get_range", container, artifact); err != nil {
		return nil, err
	}
	candidates, err := e.readCandidates(e.preferredBackend(ctx, container, artifact), "get_range")
	if err != nil {
		return nil, fmt.Errorf("get range %s/%s: %w", container, artifact, err)
	}

	var reader io.ReadCloser
	_, err = e.failover.ExecuteOp(ctx, "get_range", candidates, func(driverName string) error {
		d, ok := e.drivers[driverName]
		if !ok {
			return fmt.Errorf("driver %s not found", driverName)
		}
		if rg, ok := d.(RangeGetter); ok {
			var getErr error
			reader, getErr = rg.GetRange(ctx, container, artifact, offset, length)
			return getErr
		}
		// Fallback: full GET + seek/discard (existing slow path)
		full, getErr := d.Get(ctx, container, artifact)
		if getErr != nil {
			return getErr
		}
		if offset > 0 {
			if _, discardErr := io.CopyN(io.Discard, full, offset); discardErr != nil {
				_ = full.Close()
				return discardErr
			}
		}
		reader = full
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get range %s/%s [%d-%d]: %w", container, artifact, offset, offset+length-1, err)
	}
	return reader, nil
}

// Put stores an artifact and returns the name of the backend it was written to.
//
// The returned backend name must be persisted by the caller (the S3 adapter
// writes it to object_head_cache.backend_name): it is the one place every
// later read and delete learns where the object is (WP-R6-1). The engine
// records nothing itself — no in-memory route, no object_locations row
// (R6-09, R6-14, R8-19: one row per chunk per store, never read for them).
func (e *CoreEngine) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) (string, error) {
	if err := e.requireTenant(ctx, "put", container, artifact); err != nil {
		return "", err
	}
	tenantID := common.GetTenantID(ctx)
	// A chunk blob has one address (WP-R8-7): a write into the chunk
	// container under any other tenant's context would store it where no
	// other reader, and no collector, will ever look.
	if container == ChunkContainer && tenantID != ChunkAddressTenant {
		return "", fmt.Errorf("put %s/%s: %w", container, artifact, ErrChunkAddress)
	}
	sizeReader := &sizeTrackingReader{Reader: data}

	// Quota accounting deliberately does NOT happen here (WP-1): the API
	// layer is the single reservation site — it knows the real logical size,
	// the overwrite delta, and the failure outcome. An engine-level estimate
	// double-counted every PUT and re-counted each deduplicated chunk store.

	// Resolve storage class from options to determine target backend.
	//
	// Placement is decided by the API layer (resolvePutStorageClass) and this
	// class → backend map, nothing else. The access tracker used to be
	// consulted here when no class was set and its answer APPLIED: with
	// `temperature` never written it named "lyve" for every previously-seen
	// object, so the second to fifth PUT of any key in an `auto` bucket left
	// the primary for the resilient tier's backend (R6-03; the 2026-07-31 fix
	// had only closed the "local" branch). The engine must never re-derive
	// placement.
	options := ApplyPutOptions(opts...)
	e.mu.RLock()
	targetBackend, _ := ResolveStorageClass(options.StorageClass, e.primary, e.drivers)
	_, targetRegistered := e.drivers[targetBackend]
	e.mu.RUnlock()

	// A strict target with no driver on this process (a redeploy without
	// SYNC_WEBDAV_URLS) is a 503 the client retries — never the primary in
	// its place, which stored the object off the bucket's tier and answered
	// 200 (Prompt 2b.6 G1). Before any byte is read: the API writes no head
	// row and releases its reservation.
	if strictTarget(targetBackend) && !targetRegistered {
		e.strictTargetUnregistered(targetBackend, "put")
		return "", fmt.Errorf("put %s/%s: %w: %w: %q", container, artifact, ErrAllBackendsUnavailable, ErrStrictTargetUnregistered, targetBackend)
	}

	// Build candidate list: target first, then primary, then the
	// general-purpose durable backends — target-only backends (local, r2,
	// geyser, permafrost, idrive-<region>) are excluded unless targeted or
	// primary (WP-F, R6-04).
	candidates := e.buildWriteCandidateList(targetBackend)

	// Failover body safety: retries share ONE reader, so an attempt that
	// consumed bytes leaves the next backend a drained stream. Seekable
	// bodies (chunk stores hand us a *bytes.Reader) are rewound to their
	// starting offset before each retry, which makes failover genuinely
	// work. Non-seekable bodies that have been partially consumed must NOT
	// be retried at all — a length-validating backend fails pointlessly and
	// gets breaker-charged for it, and a lax backend stores a truncated
	// object as success. ErrNoFailover stops the candidate walk.
	seeker, seekable := data.(io.Seeker)
	var bodyStart int64
	if seekable {
		if off, serr := seeker.Seek(0, io.SeekCurrent); serr == nil {
			bodyStart = off
		} else {
			seekable = false
		}
	}
	firstAttempt := true

	usedBackend, err := e.failover.ExecuteOp(ctx, "put", candidates, func(driverName string) error {
		d, ok := e.drivers[driverName]
		if !ok {
			return fmt.Errorf("driver %s not found", driverName)
		}
		if !firstAttempt && sizeReader.bytesRead > 0 {
			if !seekable {
				// Unreachable in practice — the previous attempt already
				// returned ErrNoFailover — but kept as a hard guard so no
				// future Execute change can replay a drained stream.
				return fmt.Errorf("%w: non-rewindable body", ErrNoFailover)
			}
			if _, serr := seeker.Seek(bodyStart, io.SeekStart); serr != nil {
				return fmt.Errorf("%w: rewind failed: %w", ErrNoFailover, serr)
			}
			sizeReader.bytesRead = 0
		}
		firstAttempt = false
		perr := d.Put(ctx, container, artifact, sizeReader, opts...)
		// The SOURCE failed (a client that went away mid-body: Go's server
		// hands the handler the read error before it cancels the context):
		// the caller's failure, whatever the driver made of it — an SDK
		// does not always wrap the body's error (Prompt 2b.2 C1).
		if srcErr := sizeReader.sourceErr(); perr != nil && srcErr != nil && !errors.Is(perr, ErrCallerAborted) {
			perr = fmt.Errorf("%w: %w (%w)", ErrCallerAborted, srcErr, perr)
		}
		if perr != nil && !seekable && sizeReader.bytesRead > 0 {
			return fmt.Errorf("%w: %w", ErrNoFailover, perr)
		}
		return perr
	})
	if err == nil {
		common.SetBackendUsed(ctx, usedBackend)
	}

	if err != nil {
		// WP-F fail-loudly: a genuine backend failure becomes a 5xx to the
		// client (the API layer maps ErrAllBackendsUnavailable to 503 with
		// Retry-After) — clients retry; data is never silently stranded.
		// Client-level outcomes (quota, invalid input) keep their identity.
		if isBackendFailure(err) {
			e.writeFailures.Add(1)
			e.logger.Error("durable backend write failed — rejecting request, no local fallback",
				zap.String("container", container),
				zap.String("artifact", artifact),
				zap.String("target_backend", targetBackend),
				zap.Strings("candidates", candidates),
				zap.Error(err))
			if !errors.Is(err, ErrAllBackendsUnavailable) {
				err = fmt.Errorf("%w: %w", ErrAllBackendsUnavailable, err)
			}
		}
		return "", fmt.Errorf("put %s/%s: %w", container, artifact, err)
	}

	return usedBackend, nil
}

// Delete removes an artifact from every backend it is known to be on: the
// one its row records (resolved as Get resolves it) and each backend the
// context names as holding another copy (WithKnownCopies — the demotion
// ledger's other copy, a retired_blobs row's backend). Every candidate is
// attempted (R6-24: it used to stop at the first success and keep the
// second copy). A miss on one is fine — that copy is gone; a failure on any
// is the delete's failure (ErrAllBackendsUnavailable for a backend that
// could not be asked, unregistered included: the API answers 503 and keeps
// the head row, the quota and the aftermath, so the client retries) — never
// "deleted" while a copy may remain. Misses everywhere return the miss: the
// API's idempotent delete.
//
// No candidate is ever the primary in place of the recorded backend: the
// primary's DELETE of an absent key succeeds on every S3-class store, and
// DeleteObject answered 204 and dropped the head row while the bytes stayed
// where the row said (Prompt 2b.2 C2; WP-R7-5's fallback for a row on an
// unregistered name is gone with WP-R6-1 — prod had 0 such rows).
func (e *CoreEngine) Delete(ctx context.Context, container, artifact string) error {
	if err := e.requireTenant(ctx, "delete", container, artifact); err != nil {
		return err
	}
	targets := []string{e.preferredBackend(ctx, container, artifact)}
	for _, b := range KnownCopies(ctx) {
		if !slices.Contains(targets, b) {
			targets = append(targets, b)
		}
	}

	var failure, miss error
	deleted := false
	for _, name := range targets {
		err := e.deleteOn(ctx, name, container, artifact)
		switch {
		case err == nil:
			deleted = true
		case IsObjectMiss(err):
			if miss == nil {
				miss = err
			}
		case failure == nil:
			failure = err
		}
	}

	switch {
	case failure != nil:
		return failure
	case deleted:
		return nil
	default:
		return miss
	}
}

// deleteOn deletes the artifact on one named backend, through its breaker.
func (e *CoreEngine) deleteOn(ctx context.Context, name, container, artifact string) error {
	e.mu.RLock()
	d, registered := e.drivers[name]
	e.mu.RUnlock()
	if !registered {
		return fmt.Errorf("delete %s/%s on %s: %w", container, artifact, name, e.unregisteredBackend(name, "delete"))
	}
	if _, err := e.failover.ExecuteOp(ctx, "delete", []string{name}, func(string) error {
		return d.Delete(ctx, container, artifact)
	}); err != nil {
		return fmt.Errorf("delete %s/%s on %s: %w", container, artifact, name, err)
	}
	return nil
}

// List returns artifacts in a container
func (e *CoreEngine) List(ctx context.Context, container, prefix string) ([]Artifact, error) {
	driver, ok := e.drivers[e.primary]
	if !ok {
		return nil, fmt.Errorf("no driver available")
	}

	keys, err := driver.List(ctx, container, prefix)
	if err != nil {
		return nil, err
	}

	artifacts := make([]Artifact, len(keys))
	for i, key := range keys {
		artifacts[i] = Artifact{
			Key:       key,
			Container: container,
			Type:      "blob",
		}
	}

	return artifacts, nil
}

// HealthCheck verifies all drivers and systems are healthy
func (e *CoreEngine) HealthCheck(ctx context.Context) error {
	for name, driver := range e.drivers {
		if err := driver.HealthCheck(ctx); err != nil {
			return fmt.Errorf("driver %s unhealthy: %w", name, err)
		}
	}
	if e.db != nil {
		if err := e.db.PingContext(ctx); err != nil {
			return fmt.Errorf("database unhealthy: %w", err)
		}
	}
	return nil
}

// WriteFailures returns the number of PUTs rejected because every eligible
// durable backend failed (the fail-loudly path). Exported to Prometheus as
// vaultaire_backend_write_failures_total.
func (e *CoreEngine) WriteFailures() int64 { return e.writeFailures.Load() }

// GetMetrics returns comprehensive metrics
func (e *CoreEngine) GetMetrics(ctx context.Context) (map[string]interface{}, error) {
	metrics := map[string]interface{}{
		"drivers":        len(e.drivers),
		"primary":        e.primary,
		"write_failures": e.writeFailures.Load(),
	}
	return metrics, nil
}

type sizeTrackingReader struct {
	io.Reader
	bytesRead int64

	mu     sync.Mutex
	srcErr error // the source's own read failure (never io.EOF)
}

func (r *sizeTrackingReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.bytesRead += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		r.mu.Lock()
		if r.srcErr == nil {
			r.srcErr = err
		}
		r.mu.Unlock()
	}
	return
}

// sourceErr is the first read error of the source itself, if any.
func (r *sizeTrackingReader) sourceErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.srcErr
}

// buildCandidateList returns an ordered list of backends to try: preferred
// first, then primary (if different), then any remaining registered backends.
func (e *CoreEngine) buildCandidateList(preferred string) []string {
	seen := make(map[string]bool)
	var candidates []string

	add := func(name string) {
		if name != "" && !seen[name] {
			if _, ok := e.drivers[name]; ok {
				candidates = append(candidates, name)
				seen[name] = true
			}
		}
	}

	add(preferred)
	add(e.primary)
	for name := range e.drivers {
		add(name)
	}
	return candidates
}

// targetOnlyBackends receive a write only when they are the resolved target
// of the storage class (or the configured primary) — never as a silent
// failover destination for someone else's object:
//
//   - local: the hub's disk does not survive loss of the machine (WP-F, 1.14).
//   - r2: the PUBLIC store / CDN origin; a private object must not land there.
//   - geyser: tape — evicted after the staging window, then 403 on GET; a
//     STANDARD customer never asked for a restore workflow.
//   - permafrost: the OneDrive fleet, an async second-copy role at ~10 MB/s.
//   - idrive-<region>: region-pinned buckets; writing a US object to an EU
//     endpoint (or the reverse) is a data-residency breach.
//   - sync: the Sync.com WebDAV bridge. Sync's terms forbid reselling the
//     service without its written consent: customer data lands there only
//     for a flagged tenant's `sync` bucket (api.resolvePutStorageClass).
//
// Failover for a general-purpose write is therefore between the target, the
// primary and the remaining general-purpose durable backends (idrive, lyve,
// s3, quotaless). (R6-04)
var targetOnlyBackends = map[string]bool{
	"local":      true,
	"r2":         true,
	"geyser":     true,
	"permafrost": true,
	"sync":       true,
	"onedrive":   true, // legacy registration key (tools now use "permafrost", R7-10); keep for old head rows
}

// dormantBackends are general-purpose stores kept registered only so the
// rows on them stay readable: never a write fall-over destination unless
// they are the target or the primary, and — like every backend since
// WP-R6-1 — never read unless a row names them. Unlike targetOnlyBackends
// they may still be made the primary (STORAGE_MODE=wasabi, the admin
// switch): then every write is theirs again.
//
//   - wasabi: the interim primary for one day (2026-10-03 → 10-04) while an
//     iDrive key answered 403; a partner account kept dormant since. A 90-day
//     minimum per object makes a stray fall-over write a 90-day charge.
var dormantBackends = map[string]bool{
	"wasabi": true,
}

// writeOnlyWhenTargeted reports whether name may receive a write only as the
// explicit target or the configured primary.
func writeOnlyWhenTargeted(name string) bool {
	return targetOnlyBackends[name] || strings.HasPrefix(name, "idrive-")
}

// strictTargetBackends never have another backend asked after their own
// failure — for writes nor reads (Prompt 2b.4 E1.1). For `sync` a fall-over
// is never right: a write landed on the primary is off the tier the bucket
// promises (and answered 200), and a read of a key whose older copy sits on
// the primary (written before the tier flag, or by an earlier fall-over;
// WP-R6-1 never removes it) served the OLD bytes under the new head row's
// ETag. Once three bridges were failing, or the breaker was open, both
// happened. A strict backend that fails answers ErrAllBackendsUnavailable
// (503 + Retry-After) — the region-pinned precedent (errRegionDriverUnavailable).
//
// This departs from R6-04 (a down backend falls to the primary) for these
// backends only. The other target-only backends keep the fall-over: tape,
// the second-copy fleet and region pins are decisions not taken here.
var strictTargetBackends = map[string]bool{
	"sync": true,
}

// ErrStrictTargetUnregistered is the cause, wrapped in
// ErrAllBackendsUnavailable (503 + Retry-After), of a write placed on a
// strict target that no driver is registered under on this process: never
// another backend in its place (Prompt 2b.6 G1).
var ErrStrictTargetUnregistered = errors.New("strict target backend is not registered")

// strictTarget reports whether name is asked alone when it is the target of
// a write or the recorded backend of a read.
func strictTarget(name string) bool { return strictTargetBackends[name] }

// IsStrictTarget is strictTarget for the API layer.
func IsStrictTarget(name string) bool { return strictTarget(name) }

// readCandidates is the Get/GetRange candidate list for an object recorded
// on target: target alone (WP-R6-1; a strict target was already asked alone,
// Prompt 2b.4 E1.1). A target no driver is registered under is
// ErrAllBackendsUnavailable (503) — never the primary or any other backend in
// its place (Prompt 2b.5 F3.1: a redeploy without SYNC_WEBDAV_URLS fanned
// every sync-row read out to the primary, which served whatever older copy
// of the key it held, 200).
func (e *CoreEngine) readCandidates(target, op string) ([]string, error) {
	e.mu.RLock()
	_, ok := e.drivers[target]
	e.mu.RUnlock()
	if !ok {
		return nil, e.unregisteredBackend(target, op)
	}
	return []string{target}, nil
}

// strictTargetUnregistered logs and counts a call routed to a strict target
// that no driver is registered under.
func (e *CoreEngine) strictTargetUnregistered(name, op string) {
	strictUnregistered.WithLabelValues(name, op).Inc()
	e.logger.Error("call routed to a strict backend that is not registered on this process — answered 503, never another backend",
		zap.String("backend", name), zap.String("op", op))
}

// buildWriteCandidateList is buildCandidateList restricted to backends that
// are safe write targets for THIS object. A failing durable backend must
// surface as a 5xx to the client, not as a silent write to the hub's local
// disk, to the public store, to tape or to the wrong jurisdiction (which lies
// about durability or placement, and bills the wrong tier).
func (e *CoreEngine) buildWriteCandidateList(target string) []string {
	if strictTarget(target) && target != e.primary {
		if _, ok := e.drivers[target]; ok {
			return []string{target}
		}
		e.strictTargetUnregistered(target, "put")
		return nil
	}
	all := e.buildCandidateList(target)
	writable := make([]string, 0, len(all))
	for _, name := range all {
		if (writeOnlyWhenTargeted(name) || dormantBackends[name]) && name != target && name != e.primary {
			continue
		}
		writable = append(writable, name)
	}
	return writable
}

// ErrNotPrimaryEligible is returned by CheckPrimaryEligible.
var ErrNotPrimaryEligible = errors.New("backend cannot be the primary")

// CheckPrimaryEligible reports whether name may become the write primary:
// it must be a registered driver and not a target-only backend. r2 is the
// PUBLIC store, geyser is tape, permafrost is the async second-copy fleet,
// sync is a third party's service we may not resell and idrive-<region> pins
// residency — none of them may receive every private
// object. local is allowed (it is the dev/hub primary). The admin dashboard
// used to hand SetPrimary any string, including drivers that do not exist
// (Review R12 — proven live: primary set to "r2" on a box without an r2
// driver, after which every write would have failed).
func (e *CoreEngine) CheckPrimaryEligible(name string) error {
	e.mu.RLock()
	_, registered := e.drivers[name]
	e.mu.RUnlock()
	if !registered {
		return fmt.Errorf("%w: %q is not a registered backend", ErrNotPrimaryEligible, name)
	}
	if name != "local" && writeOnlyWhenTargeted(name) {
		return fmt.Errorf("%w: %q is a target-only backend (public store, tape, second copy, third-party service or region pin)", ErrNotPrimaryEligible, name)
	}
	return nil
}

// GetDriver returns a named driver if it is registered.
func (e *CoreEngine) GetDriver(name string) (Driver, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.drivers[name]
	return d, ok
}

// GetFailoverStatus returns circuit breaker states for all backends.
func (e *CoreEngine) GetFailoverStatus() map[string]string {
	return e.failover.GetAllStatuses()
}

// Shutdown gracefully shuts down the engine
func (e *CoreEngine) Shutdown(ctx context.Context) error {
	e.logger.Info("shutting down engine")
	if e.db != nil {
		_ = e.db.Close()
	}
	return nil
}

func (e *CoreEngine) GetContainerMetadata(ctx context.Context, container string) (*Container, error) {
	return &Container{
		Name:     container,
		Type:     "storage",
		Created:  time.Now(),
		Metadata: make(map[string]interface{}),
	}, nil
}

func (e *CoreEngine) GetArtifactMetadata(ctx context.Context, container, artifact string) (*Artifact, error) {
	return &Artifact{
		Container: container,
		Key:       artifact,
		Type:      "blob",
		Modified:  time.Now(),
		Metadata:  make(map[string]interface{}),
	}, nil
}

func (e *CoreEngine) Execute(ctx context.Context, container string, wasm []byte, input io.Reader) (io.Reader, error) {
	return nil, fmt.Errorf("WASM execution not yet implemented")
}

func (e *CoreEngine) Query(ctx context.Context, sql string) (ResultSet, error) {
	return nil, fmt.Errorf("SQL queries not yet implemented")
}

func (e *CoreEngine) Train(ctx context.Context, model string, data []byte) error {
	return fmt.Errorf("ML training not yet implemented")
}

func (e *CoreEngine) Predict(ctx context.Context, model string, input []byte) ([]byte, error) {
	return nil, fmt.Errorf("ML prediction not yet implemented")
}
