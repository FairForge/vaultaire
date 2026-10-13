package engine

// WP-R6-1 Part A: a read asks the backend the object's row records — and
// only it; a delete attempts every backend the object is known to have a
// copy on; a call whose context names no tenant is refused.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// countingStore is an in-memory backend that counts every call.
type countingStore struct {
	name    string
	fail    error // every call fails with it when set
	mu      sync.Mutex
	objects map[string][]byte
	calls   atomic.Int32
	deletes atomic.Int32
}

func newCountingStore(name string) *countingStore {
	return &countingStore{name: name, objects: map[string][]byte{}}
}

func (d *countingStore) Name() string { return d.name }
func (d *countingStore) Get(_ context.Context, c, a string) (io.ReadCloser, error) {
	d.calls.Add(1)
	if d.fail != nil {
		return nil, d.fail
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.objects[c+"/"+a]
	if !ok {
		return nil, ErrNotFound(c, a)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (d *countingStore) GetRange(ctx context.Context, c, a string, off, n int64) (io.ReadCloser, error) {
	rc, err := d.Get(ctx, c, a)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(rc)
	if off > int64(len(b)) {
		off = int64(len(b))
	}
	end := min(off+n, int64(len(b)))
	return io.NopCloser(bytes.NewReader(b[off:end])), nil
}
func (d *countingStore) Put(_ context.Context, c, a string, r io.Reader, _ ...PutOption) error {
	d.calls.Add(1)
	if d.fail != nil {
		return d.fail
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects[c+"/"+a] = b
	return nil
}
func (d *countingStore) Delete(_ context.Context, c, a string) error {
	d.calls.Add(1)
	d.deletes.Add(1)
	if d.fail != nil {
		return d.fail
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.objects[c+"/"+a]; !ok {
		return ErrNotFound(c, a)
	}
	delete(d.objects, c+"/"+a)
	return nil
}
func (d *countingStore) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *countingStore) Exists(_ context.Context, c, a string) (bool, error) {
	d.calls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.objects[c+"/"+a]
	return ok, nil
}
func (d *countingStore) HealthCheck(context.Context) error { return nil }

func (d *countingStore) holds(c, a string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.objects[c+"/"+a]
	return ok
}

func (d *countingStore) plant(c, a string, b []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects[c+"/"+a] = b
}

// eighteen is prod's shape: a primary and 17 other registered backends.
func eighteen(t *testing.T) (*CoreEngine, map[string]*countingStore) {
	t.Helper()
	e := NewEngine(nil, zap.NewNop(), nil)
	stores := map[string]*countingStore{}
	for i := 0; i < 18; i++ {
		name := fmt.Sprintf("b%02d", i)
		stores[name] = newCountingStore(name)
		e.AddDriver(name, stores[name])
	}
	e.SetPrimary("b00")
	return e, stores
}

func totalCalls(stores map[string]*countingStore) int32 {
	var n int32
	for _, s := range stores {
		n += s.calls.Load()
	}
	return n
}

func tenantCtx() context.Context { return common.WithTenantID(context.Background(), "tenant-a") }

func TestGet_RecordedBackendMiss_IsOneCallNot18(t *testing.T) {
	// Before (origin/main 9f3d0c1, HintBackend to b07): 18 backend calls —
	// the recorded backend, the primary, then every other registered one.
	e, stores := eighteen(t)
	ctx := WithRecordedBackend(tenantCtx(), "b07")

	_, err := e.Get(ctx, "c", "gone")
	require.Error(t, err)
	assert.True(t, IsObjectMiss(err), "a miss on the recorded backend is a miss: %v", err)
	assert.Equal(t, int32(1), totalCalls(stores), "one backend call")
	assert.Equal(t, int32(1), stores["b07"].calls.Load())

	_, err = e.GetRange(ctx, "c", "gone", 0, 10)
	assert.True(t, IsObjectMiss(err), "%v", err)
	assert.Equal(t, int32(2), totalCalls(stores), "a ranged miss is one call too")
}

func TestGet_NoRecordedBackend_AsksThePrimaryOnly(t *testing.T) {
	e, stores := eighteen(t)
	_, err := e.Get(tenantCtx(), "c", "gone")
	assert.True(t, IsObjectMiss(err), "%v", err)
	assert.Equal(t, int32(1), totalCalls(stores))
	assert.Equal(t, int32(1), stores["b00"].calls.Load(), "a row with no backend on record is the primary's")
}

func TestGet_RecordedBackendBreakerOpen_503NoFanOut(t *testing.T) {
	// Before: the open breaker was skipped and the 17 others were asked.
	e, stores := eighteen(t)
	stores["b03"].plant("c", "k", []byte("stale copy on the primary's neighbour"))
	stores["b00"].plant("c", "k", []byte("stale copy on the primary"))
	for i := 0; i < failureThreshold; i++ {
		e.failover.breakers["b05"].RecordFailure()
	}
	ctx := WithRecordedBackend(tenantCtx(), "b05")

	_, err := e.Get(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "503, never a guess elsewhere")
	assert.False(t, IsObjectMiss(err))
	_, err = e.GetRange(ctx, "c", "k", 0, 4)
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Zero(t, totalCalls(stores), "no backend was asked")
}

func TestGet_RecordedBackendFails_503NoFanOut(t *testing.T) {
	e, stores := eighteen(t)
	stores["b05"].fail = errors.New("dial tcp: connection refused")
	stores["b00"].plant("c", "k", []byte("stale"))
	_, err := e.Get(WithRecordedBackend(tenantCtx(), "b05"), "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Equal(t, int32(1), totalCalls(stores))
}

func TestGet_StaleCopyOnAnotherBackend_NeverServed(t *testing.T) {
	// Before: the recorded backend's miss walked on to the primary, which
	// served the stale copy (the delete-resurrection class, R6-07).
	e, stores := eighteen(t)
	stores["b00"].plant("c", "k", []byte("a stale copy on the primary"))
	stores["b11"].plant("c", "k", []byte("another stale copy"))
	ctx := WithRecordedBackend(tenantCtx(), "b04")

	rc, err := e.Get(ctx, "c", "k")
	if rc != nil {
		b, _ := io.ReadAll(rc)
		t.Fatalf("served %q from a backend the row does not name", b)
	}
	assert.True(t, IsObjectMiss(err), "%v", err)
	rc, err = e.GetRange(ctx, "c", "k", 0, 5)
	assert.Nil(t, rc)
	assert.True(t, IsObjectMiss(err), "%v", err)
}

func TestGet_UnregisteredRecordedBackend_503NoFallback(t *testing.T) {
	// WP-R7-5's fallback goes (prod: 0 head rows on an unregistered name,
	// 2026-10-13): a row naming a backend no driver is registered under is
	// 503 — the bytes may well exist there.
	e, stores := eighteen(t)
	stores["b00"].plant("c", "k", []byte("a copy on the primary"))
	ctx := WithRecordedBackend(tenantCtx(), "onedrive")

	_, err := e.Get(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.ErrorIs(t, err, ErrBackendNotRegistered)
	_, err = e.GetRange(ctx, "c", "k", 0, 1)
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.ErrorIs(t, e.Delete(ctx, "c", "k"), ErrAllBackendsUnavailable)
	assert.Zero(t, totalCalls(stores))
	assert.True(t, stores["b00"].holds("c", "k"))
}

func TestDelete_RemovesEveryKnownCopy(t *testing.T) {
	// Before: Delete stopped at the first backend that succeeded (R6-24):
	// the second copy stayed.
	e, stores := eighteen(t)
	stores["b02"].plant("c", "k", []byte("the copy the row names"))
	stores["b09"].plant("c", "k", []byte("a known second copy"))
	stores["b00"].plant("c", "k", []byte("the primary's copy is not known — left alone"))
	ctx := WithKnownCopies(WithRecordedBackend(tenantCtx(), "b02"), "b09", "b02")

	require.NoError(t, e.Delete(ctx, "c", "k"))
	assert.False(t, stores["b02"].holds("c", "k"))
	assert.False(t, stores["b09"].holds("c", "k"))
	assert.True(t, stores["b00"].holds("c", "k"), "only the copies the caller knows")
	assert.Equal(t, int32(1), stores["b02"].deletes.Load(), "each candidate once")

	// A miss on one copy is fine: the copy is gone.
	stores["b02"].plant("c", "k2", []byte("x"))
	require.NoError(t, e.Delete(WithKnownCopies(WithRecordedBackend(tenantCtx(), "b02"), "b09"), "c", "k2"))
	assert.False(t, stores["b02"].holds("c", "k2"))
	// Misses everywhere: a miss (the API's idempotent delete).
	err := e.Delete(WithKnownCopies(WithRecordedBackend(tenantCtx(), "b02"), "b09"), "c", "nothing")
	assert.True(t, IsObjectMiss(err), "%v", err)
}

func TestDelete_OneCopyFails_IsAFailure(t *testing.T) {
	e, stores := eighteen(t)
	stores["b02"].plant("c", "k", []byte("the copy the row names"))
	stores["b09"].plant("c", "k", []byte("a second copy on a failing backend"))
	stores["b09"].fail = errors.New("503 Service Unavailable")
	ctx := WithKnownCopies(WithRecordedBackend(tenantCtx(), "b02"), "b09")

	err := e.Delete(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "never 'deleted' while a copy may remain")
	assert.False(t, IsObjectMiss(err))
	assert.False(t, stores["b02"].holds("c", "k"), "every candidate is attempted")
	assert.Equal(t, int32(1), stores["b09"].deletes.Load())
	// An unregistered known copy is a failure too: it cannot be asked.
	stores["b02"].plant("c", "k3", []byte("x"))
	err = e.Delete(WithKnownCopies(WithRecordedBackend(tenantCtx(), "b02"), "gone-backend"), "c", "k3")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
}

func TestEngine_NoTenantContext_Refused(t *testing.T) {
	// Before: GetTenantID answered "default" and the call went through —
	// to t-default/ on a fixed-bucket backend (refused there since WP-R8-7),
	// to a shared namespace on the others (R6-22).
	e, stores := eighteen(t)
	noTenant := context.Background()
	before := map[string]float64{}
	for _, op := range noTenantOps {
		before[op] = promtest.ToFloat64(noTenantRefusals.WithLabelValues(op))
	}

	_, err := e.Put(noTenant, "c", "k", bytes.NewReader([]byte("x")))
	assert.ErrorIs(t, err, ErrNoTenant)
	_, err = e.Get(noTenant, "c", "k")
	assert.ErrorIs(t, err, ErrNoTenant)
	_, err = e.GetRange(noTenant, "c", "k", 0, 1)
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.ErrorIs(t, e.Delete(noTenant, "c", "k"), ErrNoTenant)
	_, err = e.GetOn(noTenant, "b01", "c", "k")
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.ErrorIs(t, e.PutOn(noTenant, "b01", "c", "k", bytes.NewReader(nil)), ErrNoTenant)
	assert.ErrorIs(t, e.DeleteOn(noTenant, "b01", "c", "k"), ErrNoTenant)
	_, err = e.ExistsOn(noTenant, "b01", "c", "k")
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.ErrorIs(t, err, ErrInvalidInput, "a caller's bug: never a miss, never a breaker charge")
	assert.Zero(t, totalCalls(stores), "no backend was asked")
	for op, want := range map[string]float64{"put": 2, "get": 2, "get_range": 1, "delete": 2, "exists": 1} {
		assert.Equal(t, before[op]+want, promtest.ToFloat64(noTenantRefusals.WithLabelValues(op)), op)
	}

	// The shared chunk container and the probe tenant name their tenant.
	_, err = e.Put(ChunkContext(context.Background()), ChunkContainer, "_chunks/h", bytes.NewReader([]byte("x")))
	assert.NoError(t, err)
	_, err = e.Get(common.WithTenantID(context.Background(), ProbeTenant), "c", "k")
	assert.True(t, IsObjectMiss(err), "%v", err)
	// The context's tenant is never "default".
	assert.Equal(t, "", common.GetTenantID(context.Background()))
}
