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
			e.HintBackend("c", "k", "sync") // the head row's backend_name

			// Act
			err := e.Delete(context.Background(), "c", "k")

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
	for i := 0; i < 5; i++ {
		e.HintBackend("c", "k", "sync")
		_ = e.Delete(context.Background(), "c", "k")
	}
	require.Equal(t, "open", e.GetFailoverStatus()["sync"])

	e.HintBackend("c", "k", "sync")
	err := e.Delete(context.Background(), "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Zero(t, primary.deletes)
}

// A miss on the recorded backend is still a miss (an idempotent delete),
// and a row naming an unregistered backend keeps today's fallback until
// WP-R6-1.
func TestDelete_RecordedMissAndUnregisteredNames(t *testing.T) {
	e := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "primary"})
	primary := newDeleteRecorder("primary", nil)
	e.AddDriver("primary", primary)
	e.AddDriver("sync", missDeleter{newDeleteRecorder("sync", nil)})

	e.HintBackend("c", "gone", "sync")
	err := e.Delete(context.Background(), "c", "gone")
	var nf NotFoundError
	assert.ErrorAs(t, err, &nf, "the recorded backend's miss is the verdict")
	assert.Zero(t, primary.deletes)

	e.HintBackend("c", "old", "onedrive") // a name no driver has
	require.NoError(t, e.Delete(context.Background(), "c", "old"))
	assert.Equal(t, 1, primary.deletes, "unhinted / unregistered: the primary, as before")
}

type missDeleter struct{ *deleteRecorder }

func (m missDeleter) Delete(_ context.Context, c, a string) error { return ErrNotFound(c, a) }
