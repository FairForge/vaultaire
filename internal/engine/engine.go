// internal/engine/engine.go
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
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

	locations *LocationStore

	// objectBackends records which backend each object was written to.
	// Key: "container/artifact"  Value: backend name string
	// This guarantees Get always reads from the same backend Put used.
	// In-memory only; objects written before a restart fall back to
	// object_locations, then e.primary (safe — they were written there too).
	objectBackends sync.Map

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
	e.locations = NewLocationStore(db, logger)
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

// objectKey returns the sync.Map key for a container+artifact pair.
func objectKey(container, artifact string) string {
	return container + "/" + artifact
}

// Get retrieves an artifact.
//
// Backend selection consults objectBackends first — the map written by Put
// that records exactly which backend each object landed on. This is the
// source of truth.
//
// Fall-through to e.primary only occurs for objects written before this
// engine version was deployed (map is empty on restart).
func (e *CoreEngine) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	tenantID := common.GetTenantID(ctx)

	preferredBackend := e.primary
	if v, ok := e.objectBackends.Load(objectKey(container, artifact)); ok {
		if name, ok := v.(string); ok && name != "" {
			preferredBackend = name
		}
	} else if e.locations != nil {
		if name, err := e.locations.LookupBackend(ctx, tenantID, container, artifact); err == nil && name != "" {
			preferredBackend = name
			e.objectBackends.Store(objectKey(container, artifact), name)
		}
	}

	// Build candidate list: preferred backend first, then primary, then others.
	candidates := e.buildCandidateList(preferredBackend)

	var reader io.ReadCloser
	usedBackend, err := e.failover.Execute(ctx, candidates, func(driverName string) error {
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
// the full object. Falls back to full Get + discard if the driver doesn't
// implement RangeGetter.
func (e *CoreEngine) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	preferredBackend := e.primary
	if v, ok := e.objectBackends.Load(objectKey(container, artifact)); ok {
		if name, ok := v.(string); ok && name != "" {
			preferredBackend = name
		}
	} else if e.locations != nil {
		tenantID := common.GetTenantID(ctx)
		if name, err := e.locations.LookupBackend(ctx, tenantID, container, artifact); err == nil && name != "" {
			preferredBackend = name
			e.objectBackends.Store(objectKey(container, artifact), name)
		}
	}

	candidates := e.buildCandidateList(preferredBackend)

	var reader io.ReadCloser
	_, err := e.failover.Execute(ctx, candidates, func(driverName string) error {
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
// writes it to object_head_cache.backend_name). It is also stored in
// objectBackends so that Get can route correctly within the same process
// lifetime without a DB round-trip.
func (e *CoreEngine) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) (string, error) {
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
	targetBackend, _ := ResolveStorageClass(options.StorageClass, e.primary, e.drivers)

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

	usedBackend, err := e.failover.Execute(ctx, candidates, func(driverName string) error {
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

	e.objectBackends.Store(objectKey(container, artifact), usedBackend)

	if e.locations != nil {
		resolvedClass := options.StorageClass
		if resolvedClass == "" {
			resolvedClass = "STANDARD"
		}
		go func() { // #nosec G118 -- fire-and-forget location record; must outlive the request, request ctx would cancel it
			_ = e.locations.RecordLocation(context.Background(), tenantID, container, artifact, usedBackend, resolvedClass, sizeReader.bytesRead)
		}()
	}

	return usedBackend, nil
}

// Delete removes an artifact from the backend that holds it, falling back to
// the primary. It stops at the first backend that reports success (or a miss
// — the API layer treats a miss as an idempotent delete), so a second copy on
// another backend is NOT removed here; that is WP-R6-1.
//
// The backend is resolved like Get does: hint / in-memory map first, then the
// durable object_locations row. Resolving from the in-memory map alone meant
// that after every restart a DELETE of an object stored off-primary went to
// the primary, was answered "not found", and the head row was removed while
// the bytes stayed on the real backend forever (R6-05). Callers that know the
// head-cache backend_name must still HintBackend first — that column is the
// routing truth and object_locations can lag it.
func (e *CoreEngine) Delete(ctx context.Context, container, artifact string) error {
	tenantID := common.GetTenantID(ctx)

	key := objectKey(container, artifact)
	targetBackend := e.primary
	if stored, ok := e.objectBackends.Load(key); ok {
		if name, ok := stored.(string); ok && name != "" {
			targetBackend = name
		}
	} else if e.locations != nil {
		if name, err := e.locations.LookupBackend(ctx, tenantID, container, artifact); err == nil && name != "" {
			targetBackend = name
		}
	}

	candidates := []string{targetBackend}
	if targetBackend != e.primary {
		candidates = append(candidates, e.primary)
	}

	_, lastErr := e.failover.Execute(ctx, candidates, func(driverName string) error {
		d, ok := e.drivers[driverName]
		if !ok {
			return fmt.Errorf("driver %s not found", driverName)
		}
		return d.Delete(ctx, container, artifact)
	})

	e.objectBackends.Delete(key)

	if e.locations != nil {
		_ = e.locations.RemoveLocation(ctx, tenantID, container, artifact)
	}

	return lastErr
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
}

func (r *sizeTrackingReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.bytesRead += int64(n)
	return
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
//
// Failover for a general-purpose write is therefore between the target, the
// primary and the remaining general-purpose durable backends (idrive, lyve,
// s3, quotaless). (R6-04)
var targetOnlyBackends = map[string]bool{
	"local":      true,
	"r2":         true,
	"geyser":     true,
	"permafrost": true,
	"onedrive":   true, // legacy registration key (tools now use "permafrost", R7-10); keep for old head rows
}

// writeOnlyWhenTargeted reports whether name may receive a write only as the
// explicit target or the configured primary.
func writeOnlyWhenTargeted(name string) bool {
	return targetOnlyBackends[name] || strings.HasPrefix(name, "idrive-")
}

// buildWriteCandidateList is buildCandidateList restricted to backends that
// are safe write targets for THIS object. A failing durable backend must
// surface as a 5xx to the client, not as a silent write to the hub's local
// disk, to the public store, to tape or to the wrong jurisdiction (which lies
// about durability or placement, and bills the wrong tier).
func (e *CoreEngine) buildWriteCandidateList(target string) []string {
	all := e.buildCandidateList(target)
	writable := make([]string, 0, len(all))
	for _, name := range all {
		if writeOnlyWhenTargeted(name) && name != target && name != e.primary {
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
// PUBLIC store, geyser is tape, permafrost is the async second-copy fleet and
// idrive-<region> pins residency — none of them may receive every private
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
		return fmt.Errorf("%w: %q is a target-only backend (public store, tape, second copy or region pin)", ErrNotPrimaryEligible, name)
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

// HintBackend seeds the objectBackends map so Get routes to the correct
// backend without a failed attempt against the primary. The S3 adapter
// calls this with the backend_name read from object_head_cache on GET.
func (e *CoreEngine) HintBackend(container, artifact, backend string) {
	if backend != "" {
		e.objectBackends.Store(objectKey(container, artifact), backend)
	}
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
