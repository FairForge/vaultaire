// internal/logging/logger_test.go
package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoggerConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &LoggerConfig{
			Level:  LevelInfo,
			Format: FormatJSON,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects invalid level", func(t *testing.T) {
		config := &LoggerConfig{Level: "invalid"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "level")
	})

	t.Run("applies defaults", func(t *testing.T) {
		config := &LoggerConfig{}
		config.ApplyDefaults()
		assert.Equal(t, LevelInfo, config.Level)
		assert.Equal(t, FormatJSON, config.Format)
	})
}

func TestNewLogger(t *testing.T) {
	t.Run("creates logger", func(t *testing.T) {
		logger := NewLogger(nil)
		assert.NotNil(t, logger)
	})

	t.Run("creates logger with config", func(t *testing.T) {
		config := &LoggerConfig{Level: LevelDebug}
		logger := NewLogger(config)
		assert.NotNil(t, logger)
	})
}

func TestLogger_Levels(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&LoggerConfig{
		Level:  LevelDebug,
		Output: &buf,
	})

	t.Run("logs debug", func(t *testing.T) {
		buf.Reset()
		logger.Debug("debug message")
		assert.Contains(t, buf.String(), "debug")
		assert.Contains(t, buf.String(), "debug message")
	})

	t.Run("logs info", func(t *testing.T) {
		buf.Reset()
		logger.Info("info message")
		assert.Contains(t, buf.String(), "info")
		assert.Contains(t, buf.String(), "info message")
	})

	t.Run("logs warn", func(t *testing.T) {
		buf.Reset()
		logger.Warn("warn message")
		assert.Contains(t, buf.String(), "warn")
		assert.Contains(t, buf.String(), "warn message")
	})

	t.Run("logs error", func(t *testing.T) {
		buf.Reset()
		logger.Error("error message")
		assert.Contains(t, buf.String(), "error")
		assert.Contains(t, buf.String(), "error message")
	})
}

func TestLogger_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&LoggerConfig{
		Level:  LevelWarn,
		Output: &buf,
	})

	t.Run("filters below threshold", func(t *testing.T) {
		buf.Reset()
		logger.Debug("should not appear")
		logger.Info("should not appear")
		assert.Empty(t, buf.String())
	})

	t.Run("logs at and above threshold", func(t *testing.T) {
		buf.Reset()
		logger.Warn("warning")
		logger.Error("error")
		assert.Contains(t, buf.String(), "warning")
		assert.Contains(t, buf.String(), "error")
	})
}

func TestLogger_StructuredFields(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&LoggerConfig{
		Level:  LevelInfo,
		Format: FormatJSON,
		Output: &buf,
	})

	t.Run("includes fields", func(t *testing.T) {
		logger.With("user_id", "123", "action", "login").Info("user logged in")

		var entry map[string]interface{}
		err := json.Unmarshal(buf.Bytes(), &entry)
		require.NoError(t, err)
		assert.Equal(t, "123", entry["user_id"])
		assert.Equal(t, "login", entry["action"])
	})
}

func TestLogger_WithContext(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&LoggerConfig{
		Level:  LevelInfo,
		Format: FormatJSON,
		Output: &buf,
	})

	t.Run("extracts context fields", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), ContextKeyRequestID, "req-123")
		ctx = context.WithValue(ctx, ContextKeyTenantID, "tenant-456")

		logger.WithContext(ctx).Info("request processed")

		var entry map[string]interface{}
		err := json.Unmarshal(buf.Bytes(), &entry)
		require.NoError(t, err)
		assert.Equal(t, "req-123", entry["request_id"])
		assert.Equal(t, "tenant-456", entry["tenant_id"])
	})
}

func TestLogger_Formats(t *testing.T) {
	t.Run("JSON format", func(t *testing.T) {
		var buf bytes.Buffer
		logger := NewLogger(&LoggerConfig{
			Level:  LevelInfo,
			Format: FormatJSON,
			Output: &buf,
		})

		logger.Info("test message")

		var entry map[string]interface{}
		err := json.Unmarshal(buf.Bytes(), &entry)
		require.NoError(t, err)
		assert.Equal(t, "test message", entry["message"])
		assert.Equal(t, "info", entry["level"])
	})

	t.Run("text format", func(t *testing.T) {
		var buf bytes.Buffer
		logger := NewLogger(&LoggerConfig{
			Level:  LevelInfo,
			Format: FormatText,
			Output: &buf,
		})

		logger.Info("test message")
		output := buf.String()
		assert.Contains(t, output, "INFO")
		assert.Contains(t, output, "test message")
	})
}

func TestLogger_ChildLogger(t *testing.T) {
	var buf bytes.Buffer
	parent := NewLogger(&LoggerConfig{
		Level:  LevelInfo,
		Format: FormatJSON,
		Output: &buf,
	})

	t.Run("inherits parent fields", func(t *testing.T) {
		child := parent.With("service", "api").Named("http")
		child.Info("request")

		var entry map[string]interface{}
		err := json.Unmarshal(buf.Bytes(), &entry)
		require.NoError(t, err)
		assert.Equal(t, "api", entry["service"])
		assert.Contains(t, entry["logger"], "http")
	})
}

func TestLogAggregator(t *testing.T) {
	aggregator := NewLogAggregator(&AggregatorConfig{
		BufferSize:    100,
		FlushInterval: 100 * time.Millisecond,
	})

	t.Run("buffers entries", func(t *testing.T) {
		aggregator.Add(&LogEntry{
			Level:     LevelInfo,
			Message:   "test",
			Timestamp: time.Now(),
		})

		stats := aggregator.Stats()
		assert.Equal(t, int64(1), stats.Buffered)
	})

	t.Run("flushes on interval", func(t *testing.T) {
		flushed := make(chan bool, 1)
		aggregator.OnFlush(func(entries []*LogEntry) {
			flushed <- true
		})

		aggregator.Start()
		defer aggregator.Stop()

		aggregator.Add(&LogEntry{
			Level:   LevelInfo,
			Message: "flush test",
		})

		select {
		case <-flushed:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("flush not triggered")
		}
	})
}

func TestLogAggregator_Filtering(t *testing.T) {
	aggregator := NewLogAggregator(&AggregatorConfig{
		BufferSize: 100,
		MinLevel:   LevelWarn,
	})

	t.Run("filters by level", func(t *testing.T) {
		aggregator.Add(&LogEntry{Level: LevelDebug, Message: "debug"})
		aggregator.Add(&LogEntry{Level: LevelInfo, Message: "info"})
		aggregator.Add(&LogEntry{Level: LevelWarn, Message: "warn"})
		aggregator.Add(&LogEntry{Level: LevelError, Message: "error"})

		stats := aggregator.Stats()
		assert.Equal(t, int64(2), stats.Buffered) // Only warn and error
	})
}

func TestLogAggregator_Sampling(t *testing.T) {
	aggregator := NewLogAggregator(&AggregatorConfig{
		BufferSize: 100,
		SampleRate: 0.5, // 50% sampling
	})

	t.Run("samples entries", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			aggregator.Add(&LogEntry{Level: LevelInfo, Message: "sampled"})
		}

		stats := aggregator.Stats()
		// Should be roughly 50, allow variance
		assert.Greater(t, stats.Buffered, int64(20))
		assert.Less(t, stats.Buffered, int64(80))
	})
}

func TestLogAggregator_Destinations(t *testing.T) {
	var buf bytes.Buffer
	aggregator := NewLogAggregator(&AggregatorConfig{
		BufferSize: 10,
	})

	t.Run("writes to destination", func(t *testing.T) {
		aggregator.AddDestination(&WriterDestination{Writer: &buf})

		aggregator.Add(&LogEntry{
			Level:     LevelInfo,
			Message:   "destination test",
			Timestamp: time.Now(),
		})
		aggregator.Flush()

		assert.Contains(t, buf.String(), "destination test")
	})
}

func TestLogEntry(t *testing.T) {
	t.Run("creates entry", func(t *testing.T) {
		entry := &LogEntry{
			Level:     LevelInfo,
			Message:   "test",
			Timestamp: time.Now(),
			Fields: map[string]interface{}{
				"key": "value",
			},
		}
		assert.Equal(t, LevelInfo, entry.Level)
		assert.Equal(t, "test", entry.Message)
	})

	t.Run("serializes to JSON", func(t *testing.T) {
		entry := &LogEntry{
			Level:     LevelError,
			Message:   "error occurred",
			Timestamp: time.Now(),
		}

		data, err := json.Marshal(entry)
		require.NoError(t, err)
		assert.Contains(t, string(data), "error occurred")
	})
}

func TestLogLevels(t *testing.T) {
	t.Run("defines levels", func(t *testing.T) {
		assert.Equal(t, "debug", LevelDebug)
		assert.Equal(t, "info", LevelInfo)
		assert.Equal(t, "warn", LevelWarn)
		assert.Equal(t, "error", LevelError)
		assert.Equal(t, "fatal", LevelFatal)
	})

	t.Run("compares levels", func(t *testing.T) {
		assert.True(t, LevelValue(LevelError) > LevelValue(LevelInfo))
		assert.True(t, LevelValue(LevelDebug) < LevelValue(LevelWarn))
	})
}

func TestLogFormats(t *testing.T) {
	t.Run("defines formats", func(t *testing.T) {
		assert.Equal(t, "json", FormatJSON)
		assert.Equal(t, "text", FormatText)
		assert.Equal(t, "logfmt", FormatLogfmt)
	})
}

func TestContextKeys(t *testing.T) {
	t.Run("defines context keys", func(t *testing.T) {
		assert.NotNil(t, ContextKeyRequestID)
		assert.NotNil(t, ContextKeyTenantID)
		assert.NotNil(t, ContextKeyUserID)
	})
}

func TestLoggerErrorHandling(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&LoggerConfig{
		Level:  LevelInfo,
		Format: FormatJSON,
		Output: &buf,
	})

	t.Run("logs error with stack", func(t *testing.T) {
		err := assert.AnError
		logger.WithError(err).Error("operation failed")

		var entry map[string]interface{}
		_ = json.Unmarshal(buf.Bytes(), &entry)
		assert.Contains(t, entry, "error")
	})
}

func TestLoggerAsync(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&LoggerConfig{
		Level:  LevelInfo,
		Format: FormatJSON,
		Output: &buf,
		Async:  true,
	})

	t.Run("logs asynchronously", func(t *testing.T) {
		logger.Info("async message")

		// Give async logger time to flush
		time.Sleep(50 * time.Millisecond)
		logger.Sync()

		assert.Contains(t, buf.String(), "async message")
	})
}
