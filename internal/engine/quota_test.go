// internal/engine/quota_test.go
package engine

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Simple manual mock for quota manager
type mockQuotaManager struct {
	shouldAllow bool
	returnError error
	calls       []quotaCall
}

type quotaCall struct {
	method   string
	tenantID string
	bytes    int64
}

func (m *mockQuotaManager) CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error) {
	m.calls = append(m.calls, quotaCall{"CheckAndReserve", tenantID, bytes})
	return m.shouldAllow, m.returnError
}

func (m *mockQuotaManager) ReleaseQuota(ctx context.Context, tenantID string, bytes int64) error {
	m.calls = append(m.calls, quotaCall{"ReleaseQuota", tenantID, bytes})
	return nil
}

// MockDriver for testing
type MockDriver struct {
	putCalled bool
}

func (m *MockDriver) Name() string {
	return "mock"
}

func (m *MockDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *MockDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error {
	m.putCalled = true
	return nil
}

func (m *MockDriver) Delete(ctx context.Context, container, artifact string) error {
	return nil
}

func (m *MockDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	return nil, nil
}

func (m *MockDriver) HealthCheck(ctx context.Context) error {
	return nil
}

// SINGLE test function declaration
func TestCoreEngine_PutWithQuotaEnforcement(t *testing.T) {
	t.Run("allows put within quota", func(t *testing.T) {
		// Setup
		mockQuota := &mockQuotaManager{shouldAllow: true}
		engine := NewEngine(nil)
		engine.SetQuotaManager(mockQuota)

		// Add a mock driver
		mockDriver := &MockDriver{}
		engine.AddDriver("local", mockDriver)

		ctx := context.WithValue(context.Background(), tenantIDKey, "tenant-123")
		data := strings.NewReader("test data")

		// Act
		err := engine.Put(ctx, "container", "key", data)

		// Assert
		assert.NoError(t, err)
		assert.Len(t, mockQuota.calls, 1)
		assert.Equal(t, "CheckAndReserve", mockQuota.calls[0].method)
	})

	t.Run("blocks put exceeding quota", func(t *testing.T) {
		// Setup
		mockQuota := &mockQuotaManager{shouldAllow: false}
		engine := NewEngine(nil)
		engine.SetQuotaManager(mockQuota)

		mockDriver := &MockDriver{}
		engine.AddDriver("local", mockDriver)

		ctx := context.WithValue(context.Background(), tenantIDKey, "tenant-123")
		data := strings.NewReader("test data")

		// Act
		err := engine.Put(ctx, "container", "key", data)

		// Assert
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrQuotaExceeded)
	})
}
