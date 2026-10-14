package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Prompt 2b.2 C2: a delete of an object whose head row names a registered
// backend fell back to the primary when that backend failed; an S3-like
// primary answers a DELETE of an absent key with success, so DeleteObject
// answered 204 and dropped the head row while the bytes stayed on the
// recorded backend. Before (2b8adc3): Delete returned nil, the primary was
// asked.

// deleteRecorder is an S3-like store: a DELETE of an absent key succeeds.
type deleteRecorder struct {
	name    string
	fail    error
	mu      sync.Mutex
	objects map[string][]byte
	deletes int
}

func newDeleteRecorder(name string, fail error) *deleteRecorder {
	return &deleteRecorder{name: name, fail: fail, objects: map[string][]byte{}}
}

func (d *deleteRecorder) Name() string { return d.name }
func (d *deleteRecorder) Get(_ context.Context, c, a string) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.objects[c+"/"+a]
	if !ok {
		return nil, ErrNotFound(c, a)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (d *deleteRecorder) Put(_ context.Context, c, a string, r io.Reader, _ ...PutOption) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects[c+"/"+a] = b
	return nil
}
func (d *deleteRecorder) Delete(_ context.Context, c, a string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes++
	if d.fail != nil {
		return d.fail
	}
	delete(d.objects, c+"/"+a)
	return nil
}
func (d *deleteRecorder) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *deleteRecorder) Exists(context.Context, string, string) (bool, error)   { return false, nil }
func (d *deleteRecorder) HealthCheck(context.Context) error                      { return nil }

func TestDelete_AHintedDeleteNeverFallsBackToThePrimary(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail error
		want error
	}{
		{"backend failure", errors.New("dial tcp 127.0.0.1:4922: connect: connection refused"), ErrAllBackendsUnavailable},
		{"part of the backend out", ErrPartiallyUnavailable, ErrPartiallyUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "primary"})
			primary := newDeleteRecorder("primary", nil)
			recorded := newDeleteRecorder("sync", tc.fail)
			e.AddDriver("primary", primary)
			e.AddDriver("sync", recorded)
			// Act: the head row's backend_name, on the context
			err := e.Delete(WithRecordedBackend(tenantCtx(), "sync"), "c", "k")

			// Assert: the client gets a retryable 503, nothing else is asked.
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
			assert.Zero(t, primary.deletes, "the primary's idempotent success is no verdict about the recorded backend")
		})
	}
}

func TestDelete_AnOpenBreakerOnTheRecordedBackendIsA503(t *testing.T) {
	e := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "primary"})
	primary := newDeleteRecorder("primary", nil)
	recorded := newDeleteRecorder("sync", errors.New("connection refused"))
	e.AddDriver("primary", primary)
	e.AddDriver("sync", recorded)
	ctx := WithRecordedBackend(tenantCtx(), "sync")
	for i := 0; i < 5; i++ {
		_ = e.Delete(ctx, "c", "k")
	}
	require.Equal(t, "open", e.GetFailoverStatus()["sync"])

	err := e.Delete(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Zero(t, primary.deletes)
}

// A miss on the recorded backend is still a miss (an idempotent delete),
// and a row naming an unregistered backend is a 503 (WP-R6-1).
func TestDelete_RecordedMissAndUnregisteredNames(t *testing.T) {
	e := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "primary"})
	primary := newDeleteRecorder("primary", nil)
	e.AddDriver("primary", primary)
	e.AddDriver("sync", missDeleter{newDeleteRecorder("sync", nil)})

	err := e.Delete(WithRecordedBackend(tenantCtx(), "sync"), "c", "gone")
	var nf NotFoundError
	assert.ErrorAs(t, err, &nf, "the recorded backend's miss is the verdict")
	assert.Zero(t, primary.deletes)

	// WP-R6-1: a name no driver has is a 503 — the primary in its place
	// (WP-R7-5's fallback, until WP-R6-1) is gone.
	err = e.Delete(WithRecordedBackend(tenantCtx(), "onedrive"), "c", "old")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.ErrorIs(t, err, ErrBackendNotRegistered)
	assert.Zero(t, primary.deletes, "never the primary in its place")
}

type missDeleter struct{ *deleteRecorder }

func (m missDeleter) Delete(_ context.Context, c, a string) error { return ErrNotFound(c, a) }

// Prompt 2b.3 D1.5: a FAILED delete kept the head row but dropped the
// engine's own routing record, so the retry went to the primary. Since
// WP-R6-1 the engine keeps no record: every call — the retry too — names
// the head row's backend, and the primary is never asked.
func TestDelete_TheRetryOfAFailedDeleteAsksTheRecordedBackendAgain(t *testing.T) {
	// Arrange
	e := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "primary"})
	primary := newDeleteRecorder("primary", nil)
	recorded := newDeleteRecorder("sync", errors.New("connection refused"))
	e.AddDriver("primary", primary)
	e.AddDriver("sync", recorded)
	ctx := WithRecordedBackend(tenantCtx(), "sync")
	require.Error(t, e.Delete(ctx, "c", "k"))

	// Act: the retry.
	err := e.Delete(ctx, "c", "k")

	// Assert
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Equal(t, 2, recorded.deletes, "the retry asks the recorded backend again")
	assert.Zero(t, primary.deletes)
	recorded.mu.Lock()
	recorded.fail = nil
	recorded.mu.Unlock()
	require.NoError(t, e.Delete(ctx, "c", "k"))
}
