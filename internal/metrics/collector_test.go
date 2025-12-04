// internal/metrics/collector_test.go
package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &MetricConfig{
			Name:        "http_requests_total",
			Type:        MetricTypeCounter,
			Description: "Total HTTP requests",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &MetricConfig{Type: MetricTypeCounter}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects invalid type", func(t *testing.T) {
		config := &MetricConfig{Name: "test", Type: "invalid"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type")
	})
}

func TestNewCollector(t *testing.T) {
	t.Run("creates collector", func(t *testing.T) {
		collector := NewCollector(nil)
		assert.NotNil(t, collector)
	})
}

func TestCollector_Counter(t *testing.T) {
	collector := NewCollector(nil)

	t.Run("registers counter", func(t *testing.T) {
		counter, err := collector.RegisterCounter(&MetricConfig{
			Name:        "requests_total",
			Description: "Total requests",
			Labels:      []string{"method", "path"},
		})
		require.NoError(t, err)
		assert.NotNil(t, counter)
	})

	t.Run("increments counter", func(t *testing.T) {
		counter, _ := collector.GetCounter("requests_total")
		counter.Inc(Labels{"method": "GET", "path": "/api"})

		value := counter.Value(Labels{"method": "GET", "path": "/api"})
		assert.Equal(t, float64(1), value)
	})

	t.Run("adds to counter", func(t *testing.T) {
		counter, _ := collector.GetCounter("requests_total")
		counter.Add(5, Labels{"method": "POST", "path": "/api"})

		value := counter.Value(Labels{"method": "POST", "path": "/api"})
		assert.Equal(t, float64(5), value)
	})
}

func TestCollector_Gauge(t *testing.T) {
	collector := NewCollector(nil)

	t.Run("registers gauge", func(t *testing.T) {
		gauge, err := collector.RegisterGauge(&MetricConfig{
			Name:        "connections_active",
			Description: "Active connections",
		})
		require.NoError(t, err)
		assert.NotNil(t, gauge)
	})

	t.Run("sets gauge value", func(t *testing.T) {
		gauge, _ := collector.GetGauge("connections_active")
		gauge.Set(42, nil)

		value := gauge.Value(nil)
		assert.Equal(t, float64(42), value)
	})

	t.Run("increments and decrements gauge", func(t *testing.T) {
		gauge, _ := collector.GetGauge("connections_active")
		gauge.Set(10, nil)
		gauge.Inc(nil)
		gauge.Dec(nil)

		value := gauge.Value(nil)
		assert.Equal(t, float64(10), value)
	})
}

func TestCollector_Histogram(t *testing.T) {
	collector := NewCollector(nil)

	t.Run("registers histogram", func(t *testing.T) {
		histogram, err := collector.RegisterHistogram(&MetricConfig{
			Name:        "request_duration_seconds",
			Description: "Request duration",
			Buckets:     []float64{0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
		})
		require.NoError(t, err)
		assert.NotNil(t, histogram)
	})

	t.Run("observes values", func(t *testing.T) {
		histogram, _ := collector.GetHistogram("request_duration_seconds")
		histogram.Observe(0.05, nil)
		histogram.Observe(0.15, nil)
		histogram.Observe(0.75, nil)

		stats := histogram.Stats(nil)
		assert.Equal(t, int64(3), stats.Count)
		assert.InDelta(t, 0.95, stats.Sum, 0.001)
	})

	t.Run("tracks bucket counts", func(t *testing.T) {
		histogram, _ := collector.GetHistogram("request_duration_seconds")
		buckets := histogram.Buckets(nil)
		assert.NotEmpty(t, buckets)
	})
}

func TestCollector_Summary(t *testing.T) {
	collector := NewCollector(nil)

	t.Run("registers summary", func(t *testing.T) {
		summary, err := collector.RegisterSummary(&MetricConfig{
			Name:        "request_size_bytes",
			Description: "Request size",
			Quantiles:   []float64{0.5, 0.9, 0.99},
		})
		require.NoError(t, err)
		assert.NotNil(t, summary)
	})

	t.Run("observes values", func(t *testing.T) {
		summary, _ := collector.GetSummary("request_size_bytes")
		for i := 0; i < 100; i++ {
			summary.Observe(float64(i*10), nil)
		}

		quantiles := summary.Quantiles(nil)
		assert.Contains(t, quantiles, 0.5)
		assert.Contains(t, quantiles, 0.9)
	})
}

func TestCollector_Timer(t *testing.T) {
	collector := NewCollector(nil)

	_, _ = collector.RegisterHistogram(&MetricConfig{
		Name:    "operation_duration",
		Buckets: []float64{0.001, 0.01, 0.1, 1.0},
	})

	t.Run("times operation", func(t *testing.T) {
		timer := collector.StartTimer("operation_duration", nil)
		time.Sleep(10 * time.Millisecond)
		duration := timer.Stop()

		assert.Greater(t, duration, 10*time.Millisecond)
	})
}

func TestCollector_Snapshot(t *testing.T) {
	collector := NewCollector(nil)

	_, _ = collector.RegisterCounter(&MetricConfig{Name: "counter1"})
	_, _ = collector.RegisterGauge(&MetricConfig{Name: "gauge1"})

	counter, _ := collector.GetCounter("counter1")
	counter.Inc(nil)

	gauge, _ := collector.GetGauge("gauge1")
	gauge.Set(42, nil)

	t.Run("returns snapshot", func(t *testing.T) {
		snapshot := collector.Snapshot()
		assert.NotEmpty(t, snapshot.Metrics)
		assert.Contains(t, snapshot.Metrics, "counter1")
		assert.Contains(t, snapshot.Metrics, "gauge1")
	})
}

func TestCollector_Export(t *testing.T) {
	collector := NewCollector(nil)

	_, _ = collector.RegisterCounter(&MetricConfig{
		Name:        "http_requests",
		Description: "HTTP requests",
	})
	counter, _ := collector.GetCounter("http_requests")
	counter.Add(100, nil)

	t.Run("exports Prometheus format", func(t *testing.T) {
		output, err := collector.Export(FormatPrometheus)
		require.NoError(t, err)
		assert.Contains(t, string(output), "http_requests")
		assert.Contains(t, string(output), "100")
	})

	t.Run("exports JSON format", func(t *testing.T) {
		output, err := collector.Export(FormatJSON)
		require.NoError(t, err)
		assert.Contains(t, string(output), "http_requests")
	})
}

func TestCollector_Registry(t *testing.T) {
	collector := NewCollector(nil)

	t.Run("lists registered metrics", func(t *testing.T) {
		_, _ = collector.RegisterCounter(&MetricConfig{Name: "metric1"})
		_, _ = collector.RegisterGauge(&MetricConfig{Name: "metric2"})

		metrics := collector.List()
		assert.Len(t, metrics, 2)
	})

	t.Run("unregisters metric", func(t *testing.T) {
		err := collector.Unregister("metric1")
		assert.NoError(t, err)

		metrics := collector.List()
		assert.Len(t, metrics, 1)
	})
}

func TestCollector_Namespace(t *testing.T) {
	collector := NewCollector(&CollectorConfig{
		Namespace: "vaultaire",
		Subsystem: "storage",
	})

	t.Run("prefixes metric names", func(t *testing.T) {
		_, _ = collector.RegisterCounter(&MetricConfig{Name: "operations"})

		metrics := collector.List()
		assert.Contains(t, metrics[0], "vaultaire_storage_operations")
	})
}

func TestLabels(t *testing.T) {
	t.Run("creates labels", func(t *testing.T) {
		labels := Labels{"method": "GET", "status": "200"}
		assert.Equal(t, "GET", labels["method"])
	})

	t.Run("generates key", func(t *testing.T) {
		labels := Labels{"b": "2", "a": "1"}
		key := labels.Key()
		// Should be sorted
		assert.Equal(t, "a=1,b=2", key)
	})
}

func TestMetricTypes(t *testing.T) {
	t.Run("defines metric types", func(t *testing.T) {
		assert.Equal(t, "counter", MetricTypeCounter)
		assert.Equal(t, "gauge", MetricTypeGauge)
		assert.Equal(t, "histogram", MetricTypeHistogram)
		assert.Equal(t, "summary", MetricTypeSummary)
	})
}

func TestExportFormats(t *testing.T) {
	t.Run("defines export formats", func(t *testing.T) {
		assert.Equal(t, "prometheus", FormatPrometheus)
		assert.Equal(t, "json", FormatJSON)
		assert.Equal(t, "statsd", FormatStatsD)
	})
}

func TestCollector_Reset(t *testing.T) {
	collector := NewCollector(nil)

	_, _ = collector.RegisterCounter(&MetricConfig{Name: "resettable"})
	counter, _ := collector.GetCounter("resettable")
	counter.Add(100, nil)

	t.Run("resets metrics", func(t *testing.T) {
		collector.Reset()

		value := counter.Value(nil)
		assert.Equal(t, float64(0), value)
	})
}

func TestCollector_Concurrent(t *testing.T) {
	collector := NewCollector(nil)
	_, _ = collector.RegisterCounter(&MetricConfig{Name: "concurrent"})

	t.Run("handles concurrent access", func(t *testing.T) {
		counter, _ := collector.GetCounter("concurrent")

		done := make(chan bool)
		for i := 0; i < 100; i++ {
			go func() {
				counter.Inc(nil)
				done <- true
			}()
		}

		for i := 0; i < 100; i++ {
			<-done
		}

		value := counter.Value(nil)
		assert.Equal(t, float64(100), value)
	})
}

func TestCollector_HTTPHandler(t *testing.T) {
	collector := NewCollector(nil)
	_, _ = collector.RegisterCounter(&MetricConfig{Name: "test_metric"})

	t.Run("returns HTTP handler", func(t *testing.T) {
		handler := collector.HTTPHandler()
		assert.NotNil(t, handler)
	})
}

func TestCollector_Push(t *testing.T) {
	collector := NewCollector(nil)

	t.Run("pushes to gateway", func(t *testing.T) {
		// Would push to Prometheus Pushgateway
		err := collector.Push(context.Background(), "http://localhost:9091", "test-job")
		// Expected to fail without real gateway
		assert.Error(t, err)
	})
}
