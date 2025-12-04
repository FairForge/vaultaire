// internal/apm/apm_test.go
package apm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPMConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &APMConfig{
			ServiceName: "vaultaire",
			Environment: "production",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty service name", func(t *testing.T) {
		config := &APMConfig{}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("applies defaults", func(t *testing.T) {
		config := &APMConfig{ServiceName: "test"}
		config.ApplyDefaults()
		assert.Equal(t, "development", config.Environment)
		assert.True(t, config.Enabled)
	})
}

func TestNewAgent(t *testing.T) {
	t.Run("creates agent", func(t *testing.T) {
		agent := NewAgent(&APMConfig{ServiceName: "test"})
		assert.NotNil(t, agent)
	})
}

func TestAgent_StartTransaction(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("starts transaction", func(t *testing.T) {
		tx := agent.StartTransaction("test-operation")
		assert.NotNil(t, tx)
		assert.NotEmpty(t, tx.ID())
		assert.Equal(t, "test-operation", tx.Name())
		tx.End()
	})

	t.Run("tracks transaction timing", func(t *testing.T) {
		tx := agent.StartTransaction("timed-op")
		time.Sleep(10 * time.Millisecond)
		tx.End()

		assert.Greater(t, tx.Duration(), 10*time.Millisecond)
	})
}

func TestTransaction_Context(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("stores transaction in context", func(t *testing.T) {
		tx := agent.StartTransaction("context-test")
		ctx := tx.Context()

		retrieved := TransactionFromContext(ctx)
		assert.Equal(t, tx.ID(), retrieved.ID())
		tx.End()
	})
}

func TestTransaction_Segments(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})
	tx := agent.StartTransaction("segmented-op")
	defer tx.End()

	t.Run("creates segment", func(t *testing.T) {
		seg := tx.StartSegment("database-query")
		time.Sleep(5 * time.Millisecond)
		seg.End()

		segments := tx.Segments()
		assert.Len(t, segments, 1)
		assert.Equal(t, "database-query", segments[0].Name)
	})

	t.Run("creates nested segments", func(t *testing.T) {
		outer := tx.StartSegment("outer")
		inner := tx.StartSegment("inner")
		inner.End()
		outer.End()

		segments := tx.Segments()
		assert.GreaterOrEqual(t, len(segments), 2)
	})
}

func TestTransaction_Attributes(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})
	tx := agent.StartTransaction("attr-test")
	defer tx.End()

	t.Run("sets attribute", func(t *testing.T) {
		tx.SetAttribute("user.id", "123")
		attrs := tx.Attributes()
		assert.Equal(t, "123", attrs["user.id"])
	})

	t.Run("sets custom parameters", func(t *testing.T) {
		tx.AddCustomAttribute("order_id", "ORD-456")
		attrs := tx.Attributes()
		assert.Equal(t, "ORD-456", attrs["order_id"])
	})
}

func TestTransaction_Error(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("records error", func(t *testing.T) {
		tx := agent.StartTransaction("error-test")
		tx.NoticeError(assert.AnError)
		tx.End()

		assert.True(t, tx.HasError())
	})
}

func TestAgent_HTTPMiddleware(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tx := TransactionFromContext(r.Context())
		assert.NotNil(t, tx)
		w.WriteHeader(http.StatusOK)
	})

	t.Run("instruments HTTP handler", func(t *testing.T) {
		wrapped := agent.WrapHandler(handler)
		req := httptest.NewRequest("GET", "/api/users", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestAgent_DatabaseSegment(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})
	tx := agent.StartTransaction("db-test")
	defer tx.End()

	t.Run("creates database segment", func(t *testing.T) {
		seg := tx.StartDatabaseSegment(&DatabaseSegmentConfig{
			Operation:  "SELECT",
			Collection: "users",
			Product:    "PostgreSQL",
		})
		seg.End()

		segments := tx.Segments()
		require.Len(t, segments, 1)
		assert.Equal(t, "database", segments[0].Type)
	})
}

func TestAgent_ExternalSegment(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})
	tx := agent.StartTransaction("ext-test")
	defer tx.End()

	t.Run("creates external segment", func(t *testing.T) {
		seg := tx.StartExternalSegment(&ExternalSegmentConfig{
			URL:    "https://api.example.com/users",
			Method: "GET",
		})
		seg.End()

		segments := tx.Segments()
		require.Len(t, segments, 1)
		assert.Equal(t, "external", segments[0].Type)
	})
}

func TestAgent_Metrics(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("records custom metric", func(t *testing.T) {
		agent.RecordMetric("custom.metric", 42.5)
		metrics := agent.Metrics()
		assert.Contains(t, metrics, "custom.metric")
	})

	t.Run("increments counter", func(t *testing.T) {
		agent.IncrementCounter("requests.count")
		agent.IncrementCounter("requests.count")
		metrics := agent.Metrics()
		assert.Equal(t, float64(2), metrics["requests.count"])
	})
}

func TestAgent_Events(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("records custom event", func(t *testing.T) {
		agent.RecordEvent("UserSignup", map[string]interface{}{
			"plan":   "pro",
			"source": "organic",
		})

		events := agent.Events()
		require.Len(t, events, 1)
		assert.Equal(t, "UserSignup", events[0].Type)
	})
}

func TestAgent_Health(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("returns health status", func(t *testing.T) {
		health := agent.Health()
		assert.Equal(t, "healthy", health.Status)
	})

	t.Run("includes runtime stats", func(t *testing.T) {
		health := agent.Health()
		assert.Greater(t, health.Goroutines, 0)
		assert.Greater(t, health.HeapAlloc, uint64(0))
	})
}

func TestAgent_Shutdown(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("shuts down gracefully", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := agent.Shutdown(ctx)
		assert.NoError(t, err)
	})
}

func TestTransactionType(t *testing.T) {
	agent := NewAgent(&APMConfig{ServiceName: "test"})

	t.Run("sets transaction type", func(t *testing.T) {
		tx := agent.StartTransaction("web-request")
		tx.SetType(TransactionTypeWeb)
		tx.End()

		assert.Equal(t, TransactionTypeWeb, tx.Type())
	})
}

func TestTransactionTypes(t *testing.T) {
	t.Run("defines transaction types", func(t *testing.T) {
		assert.Equal(t, "web", TransactionTypeWeb)
		assert.Equal(t, "background", TransactionTypeBackground)
		assert.Equal(t, "message", TransactionTypeMessage)
	})
}

func TestSegmentTypes(t *testing.T) {
	t.Run("defines segment types", func(t *testing.T) {
		assert.Equal(t, "custom", SegmentTypeCustom)
		assert.Equal(t, "database", SegmentTypeDatabase)
		assert.Equal(t, "external", SegmentTypeExternal)
	})
}

func TestAgent_Disabled(t *testing.T) {
	agent := NewAgent(&APMConfig{
		ServiceName: "test",
		Enabled:     false,
	})

	t.Run("no-ops when disabled", func(t *testing.T) {
		tx := agent.StartTransaction("disabled-test")
		assert.NotNil(t, tx) // Returns noop transaction
		tx.End()
	})
}
