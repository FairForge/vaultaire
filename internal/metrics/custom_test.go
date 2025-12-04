// internal/metrics/custom_test.go
package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomMetricConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &CustomMetricConfig{
			Name:        "storage.bytes_uploaded",
			Unit:        UnitBytes,
			Aggregation: AggregationSum,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &CustomMetricConfig{Unit: UnitBytes}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewCustomMetrics(t *testing.T) {
	t.Run("creates custom metrics", func(t *testing.T) {
		cm := NewCustomMetrics(nil)
		assert.NotNil(t, cm)
	})
}

func TestCustomMetrics_Record(t *testing.T) {
	cm := NewCustomMetrics(nil)

	t.Run("records metric value", func(t *testing.T) {
		cm.Record("requests.count", 1, nil)
		cm.Record("requests.count", 1, nil)
		cm.Record("requests.count", 1, nil)

		value := cm.Get("requests.count", nil)
		assert.Equal(t, float64(3), value)
	})

	t.Run("records with dimensions", func(t *testing.T) {
		cm.Record("api.latency", 150, Dimensions{"endpoint": "/users", "method": "GET"})
		cm.Record("api.latency", 200, Dimensions{"endpoint": "/users", "method": "POST"})

		getLatency := cm.Get("api.latency", Dimensions{"endpoint": "/users", "method": "GET"})
		assert.Equal(t, float64(150), getLatency)
	})
}

func TestCustomMetrics_Aggregations(t *testing.T) {
	cm := NewCustomMetrics(nil)

	// Record multiple values
	for i := 1; i <= 10; i++ {
		cm.RecordValue("response.time", float64(i*10), nil)
	}

	t.Run("calculates sum", func(t *testing.T) {
		sum := cm.Aggregate("response.time", AggregationSum, nil)
		assert.Equal(t, float64(550), sum)
	})

	t.Run("calculates average", func(t *testing.T) {
		avg := cm.Aggregate("response.time", AggregationAvg, nil)
		assert.Equal(t, float64(55), avg)
	})

	t.Run("calculates min", func(t *testing.T) {
		min := cm.Aggregate("response.time", AggregationMin, nil)
		assert.Equal(t, float64(10), min)
	})

	t.Run("calculates max", func(t *testing.T) {
		max := cm.Aggregate("response.time", AggregationMax, nil)
		assert.Equal(t, float64(100), max)
	})

	t.Run("calculates count", func(t *testing.T) {
		count := cm.Aggregate("response.time", AggregationCount, nil)
		assert.Equal(t, float64(10), count)
	})
}

func TestCustomMetrics_Percentiles(t *testing.T) {
	cm := NewCustomMetrics(nil)

	// Record 100 values
	for i := 1; i <= 100; i++ {
		cm.RecordValue("latency", float64(i), nil)
	}

	t.Run("calculates p50", func(t *testing.T) {
		p50 := cm.Percentile("latency", 50, nil)
		assert.InDelta(t, 50, p50, 2)
	})

	t.Run("calculates p95", func(t *testing.T) {
		p95 := cm.Percentile("latency", 95, nil)
		assert.InDelta(t, 95, p95, 2)
	})

	t.Run("calculates p99", func(t *testing.T) {
		p99 := cm.Percentile("latency", 99, nil)
		assert.InDelta(t, 99, p99, 2)
	})
}

func TestCustomMetrics_TimeSeries(t *testing.T) {
	cm := NewCustomMetrics(&CustomMetricsConfig{
		RetentionPeriod: time.Hour,
		Resolution:      time.Minute,
	})

	t.Run("records time series data", func(t *testing.T) {
		cm.RecordWithTime("cpu.usage", 45.5, nil, time.Now())
		cm.RecordWithTime("cpu.usage", 55.0, nil, time.Now())

		series := cm.TimeSeries("cpu.usage", nil, time.Now().Add(-time.Hour), time.Now())
		assert.NotEmpty(t, series)
	})

	t.Run("aggregates by time bucket", func(t *testing.T) {
		now := time.Now()
		cm.RecordWithTime("memory.usage", 100, nil, now.Add(-30*time.Minute))
		cm.RecordWithTime("memory.usage", 150, nil, now.Add(-15*time.Minute))
		cm.RecordWithTime("memory.usage", 200, nil, now)

		buckets := cm.TimeSeriesBuckets("memory.usage", nil, now.Add(-time.Hour), now, time.Minute*15)
		assert.NotEmpty(t, buckets)
	})
}

func TestCustomMetrics_Rate(t *testing.T) {
	cm := NewCustomMetrics(nil)

	t.Run("calculates rate", func(t *testing.T) {
		now := time.Now()
		cm.RecordWithTime("requests", 100, nil, now.Add(-time.Minute))
		cm.RecordWithTime("requests", 200, nil, now)

		rate := cm.Rate("requests", nil, time.Minute)
		assert.InDelta(t, 100, rate, 5) // ~100 per minute
	})
}

func TestCustomMetrics_Delta(t *testing.T) {
	cm := NewCustomMetrics(nil)

	t.Run("calculates delta", func(t *testing.T) {
		cm.RecordValue("counter", 100, nil)
		cm.RecordValue("counter", 150, nil)

		delta := cm.Delta("counter", nil)
		assert.Equal(t, float64(50), delta)
	})
}

func TestCustomMetrics_Tags(t *testing.T) {
	cm := NewCustomMetrics(nil)

	t.Run("filters by tags", func(t *testing.T) {
		cm.Record("errors", 1, Dimensions{"service": "api", "type": "timeout"})
		cm.Record("errors", 2, Dimensions{"service": "api", "type": "connection"})
		cm.Record("errors", 3, Dimensions{"service": "worker", "type": "timeout"})

		apiErrors := cm.SumByTag("errors", "service", "api")
		assert.Equal(t, float64(3), apiErrors)

		timeoutErrors := cm.SumByTag("errors", "type", "timeout")
		assert.Equal(t, float64(4), timeoutErrors)
	})
}

func TestCustomMetrics_Reset(t *testing.T) {
	cm := NewCustomMetrics(nil)

	cm.Record("test.metric", 100, nil)

	t.Run("resets metric", func(t *testing.T) {
		cm.Reset("test.metric", nil)
		value := cm.Get("test.metric", nil)
		assert.Equal(t, float64(0), value)
	})
}

func TestCustomMetrics_List(t *testing.T) {
	cm := NewCustomMetrics(nil)

	cm.Record("metric.one", 1, nil)
	cm.Record("metric.two", 2, nil)
	cm.Record("metric.three", 3, nil)

	t.Run("lists all metrics", func(t *testing.T) {
		names := cm.List()
		assert.Len(t, names, 3)
		assert.Contains(t, names, "metric.one")
	})

	t.Run("lists metrics by prefix", func(t *testing.T) {
		names := cm.ListByPrefix("metric.")
		assert.Len(t, names, 3)
	})
}

func TestUnits(t *testing.T) {
	t.Run("defines units", func(t *testing.T) {
		assert.Equal(t, "bytes", UnitBytes)
		assert.Equal(t, "seconds", UnitSeconds)
		assert.Equal(t, "count", UnitCount)
		assert.Equal(t, "percent", UnitPercent)
		assert.Equal(t, "requests", UnitRequests)
	})
}

func TestAggregations(t *testing.T) {
	t.Run("defines aggregations", func(t *testing.T) {
		assert.Equal(t, "sum", AggregationSum)
		assert.Equal(t, "avg", AggregationAvg)
		assert.Equal(t, "min", AggregationMin)
		assert.Equal(t, "max", AggregationMax)
		assert.Equal(t, "count", AggregationCount)
	})
}

func TestDimensions(t *testing.T) {
	t.Run("creates dimensions", func(t *testing.T) {
		dims := Dimensions{"region": "us-east-1", "env": "prod"}
		assert.Equal(t, "us-east-1", dims["region"])
	})

	t.Run("generates key", func(t *testing.T) {
		dims := Dimensions{"b": "2", "a": "1"}
		key := dims.Key()
		assert.Equal(t, "a=1,b=2", key)
	})
}

func TestBusinessMetrics(t *testing.T) {
	cm := NewCustomMetrics(nil)

	t.Run("tracks storage metrics", func(t *testing.T) {
		cm.RecordStorage("tenant-1", 1024*1024*1024) // 1GB
		cm.RecordStorage("tenant-2", 512*1024*1024)  // 512MB

		total := cm.TotalStorage()
		assert.Equal(t, int64(1536*1024*1024), total)
	})

	t.Run("tracks bandwidth", func(t *testing.T) {
		cm.RecordBandwidth("tenant-1", 100*1024*1024, "egress")
		cm.RecordBandwidth("tenant-1", 50*1024*1024, "ingress")

		egress := cm.Bandwidth("tenant-1", "egress")
		assert.Equal(t, int64(100*1024*1024), egress)
	})

	t.Run("tracks API calls", func(t *testing.T) {
		cm.RecordAPICall("GetObject", 150*time.Millisecond, true)
		cm.RecordAPICall("GetObject", 200*time.Millisecond, true)
		cm.RecordAPICall("GetObject", 0, false)

		stats := cm.APIStats("GetObject")
		assert.Equal(t, int64(3), stats.TotalCalls)
		assert.Equal(t, int64(2), stats.SuccessCalls)
		assert.Equal(t, int64(1), stats.FailedCalls)
	})
}

func TestMetricSnapshot(t *testing.T) {
	cm := NewCustomMetrics(nil)

	cm.Record("metric1", 100, nil)
	cm.Record("metric2", 200, Dimensions{"env": "prod"})

	t.Run("creates snapshot", func(t *testing.T) {
		snapshot := cm.Snapshot()
		require.NotNil(t, snapshot)
		assert.NotEmpty(t, snapshot.Metrics)
		assert.False(t, snapshot.Timestamp.IsZero())
	})
}
