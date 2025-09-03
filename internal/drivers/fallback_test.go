// internal/drivers/fallback_test.go
package drivers

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// MockDriver for testing fallback behavior
type MockDriver struct {
	name       string
	shouldFail bool
	failCount  int
	calls      int
}

func (m *MockDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	m.calls++
	if m.shouldFail || m.calls <= m.failCount {
		return nil, errors.New("mock driver failed")
	}
	return io.NopCloser(strings.NewReader("data from " + m.name)), nil
}

func (m *MockDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	m.calls++
	if m.shouldFail || m.calls <= m.failCount {
		return errors.New("mock driver failed")
	}
	return nil
}

func (m *MockDriver) Delete(ctx context.Context, container, artifact string) error {
	m.calls++
	if m.shouldFail || m.calls <= m.failCount {
		return errors.New("mock driver failed")
	}
	return nil
}

func (m *MockDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	m.calls++
	if m.shouldFail || m.calls <= m.failCount {
		return nil, errors.New("mock driver failed")
	}
	return []string{"file1", "file2"}, nil
}

func (m *MockDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	m.calls++
	if m.shouldFail || m.calls <= m.failCount {
		return false, errors.New("mock driver failed")
	}
	return true, nil
}

func TestFallbackDriver(t *testing.T) {
	t.Run("uses primary when available", func(t *testing.T) {
		// Arrange
		primary := &MockDriver{name: "primary", shouldFail: false}
		secondary := &MockDriver{name: "secondary", shouldFail: false}

		fallback := NewFallbackDriver(primary, secondary, zap.NewNop())

		// Act
		reader, err := fallback.Get(context.Background(), "test", "file")

		// Assert
		require.NoError(t, err)
		data, _ := io.ReadAll(reader)
		assert.Equal(t, "data from primary", string(data))
		assert.Equal(t, 1, primary.calls)
		assert.Equal(t, 0, secondary.calls)
	})

	t.Run("falls back to secondary on primary failure", func(t *testing.T) {
		// Arrange
		primary := &MockDriver{name: "primary", shouldFail: true}
		secondary := &MockDriver{name: "secondary", shouldFail: false}

		fallback := NewFallbackDriver(primary, secondary, zap.NewNop())

		// Act
		reader, err := fallback.Get(context.Background(), "test", "file")

		// Assert
		require.NoError(t, err)
		data, _ := io.ReadAll(reader)
		assert.Equal(t, "data from secondary", string(data))
		assert.Equal(t, 1, primary.calls)
		assert.Equal(t, 1, secondary.calls)
	})

	t.Run("returns error when all backends fail", func(t *testing.T) {
		// Arrange
		primary := &MockDriver{name: "primary", shouldFail: true}
		secondary := &MockDriver{name: "secondary", shouldFail: true}

		fallback := NewFallbackDriver(primary, secondary, zap.NewNop())

		// Act
		_, err := fallback.Get(context.Background(), "test", "file")

		// Assert
		require.Error(t, err)
		assert.Contains(t, err.Error(), "all backends failed")
		assert.Equal(t, 1, primary.calls)
		assert.Equal(t, 1, secondary.calls)
	})
}
