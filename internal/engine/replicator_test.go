package engine

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDriver tracks calls for testing
type TestDriver struct {
	putCalls int32
}

func (t *TestDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("test data")), nil
}

func (t *TestDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error {
	atomic.AddInt32(&t.putCalls, 1)
	return nil
}

func (t *TestDriver) Delete(ctx context.Context, container, artifact string) error {
	return nil
}

func (t *TestDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	return []string{"obj1", "obj2", "obj3"}, nil
}

func (t *TestDriver) HealthCheck(ctx context.Context) error {
	return nil
}

func (t *TestDriver) Name() string {
	return "test"
}

func TestReplicator_ReplicateSync(t *testing.T) {
	replicator := NewReplicator(SyncReplication)

	primary := &TestDriver{}
	secondary := &TestDriver{}

	data := strings.NewReader("test data")

	err := replicator.Replicate(context.Background(), []Driver{primary, secondary},
		"container", "artifact", data)

	assert.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&primary.putCalls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&secondary.putCalls))
}

func TestReplicator_ReplicateAsync(t *testing.T) {
	replicator := NewReplicator(AsyncReplication)

	primary := &TestDriver{}
	secondary := &TestDriver{}

	data := strings.NewReader("test data")

	err := replicator.Replicate(context.Background(), []Driver{primary, secondary},
		"container", "artifact", data)

	assert.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&primary.putCalls))
	// Secondary will be called asynchronously, may not be done yet
}
