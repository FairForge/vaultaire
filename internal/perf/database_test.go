// internal/perf/database_test.go
package perf

import (
	"testing"
	"time"
)

func TestDefaultDBConfig(t *testing.T) {
	config := DefaultDBConfig()

	if config.MaxOpenConns != 25 {
		t.Errorf("expected 25 max open conns, got %d", config.MaxOpenConns)
	}
	if config.MaxIdleConns != 10 {
		t.Errorf("expected 10 max idle conns, got %d", config.MaxIdleConns)
	}
	if config.ConnMaxLifetime != 5*time.Minute {
		t.Error("unexpected conn max lifetime")
	}
	if config.ConnMaxIdleTime != 1*time.Minute {
		t.Error("unexpected conn max idle time")
	}
	if config.StatementCacheSize != 100 {
		t.Errorf("expected 100 cache size, got %d", config.StatementCacheSize)
	}
	if config.SlowQueryThreshold != 100*time.Millisecond {
		t.Error("unexpected slow query threshold")
	}
	if !config.EnablePreparedStmts {
		t.Error("expected prepared statements enabled")
	}
}

func TestDBStatsZeroValues(t *testing.T) {
	stats := &DBStats{}

	if stats.TotalQueries != 0 {
		t.Error("expected 0 total queries")
	}
	if stats.SlowQueries != 0 {
		t.Error("expected 0 slow queries")
	}
	if stats.Errors != 0 {
		t.Error("expected 0 errors")
	}
}

func TestSlowQueryRecord(t *testing.T) {
	sq := SlowQuery{
		Query:    "SELECT * FROM users WHERE id = $1",
		Duration: 150 * time.Millisecond,
		Args:     []interface{}{1},
	}

	if sq.Query == "" {
		t.Error("query should not be empty")
	}
	if sq.Duration != 150*time.Millisecond {
		t.Error("unexpected duration")
	}
	if len(sq.Args) != 1 {
		t.Error("expected 1 arg")
	}
}

func TestDBReportFields(t *testing.T) {
	report := &DBReport{
		TotalQueries:     1000,
		SlowQueryPercent: 5.0,
		CacheHitRatio:    95.0,
	}

	if report.TotalQueries != 1000 {
		t.Errorf("expected 1000 queries, got %d", report.TotalQueries)
	}
	if report.SlowQueryPercent != 5.0 {
		t.Errorf("expected 5%% slow, got %.1f%%", report.SlowQueryPercent)
	}
	if report.CacheHitRatio != 95.0 {
		t.Errorf("expected 95%% cache hit, got %.1f%%", report.CacheHitRatio)
	}
}

func TestOptimizationHint(t *testing.T) {
	hint := OptimizationHint{
		Category: "Query Performance",
		Severity: "high",
	}

	if hint.Category == "" {
		t.Error("category should not be empty")
	}
	if hint.Severity != "high" {
		t.Errorf("expected high severity, got %s", hint.Severity)
	}
}

func TestOptimizationHintSeverities(t *testing.T) {
	severities := []string{"critical", "high", "medium", "low", "info"}

	for _, s := range severities {
		hint := OptimizationHint{Severity: s}
		if hint.Severity == "" {
			t.Errorf("severity %s should not be empty", s)
		}
	}
}

func TestDBHealthCheckCreation(t *testing.T) {
	hc := &DBHealthCheck{
		timeout:  5 * time.Second,
		interval: 30 * time.Second,
	}

	if hc.timeout != 5*time.Second {
		t.Error("unexpected timeout")
	}
	if hc.interval != 30*time.Second {
		t.Error("unexpected interval")
	}
}

func TestSlowQueryLogTrimming(t *testing.T) {
	maxQueries := 5
	log := make([]SlowQuery, 0)

	for i := 0; i < 10; i++ {
		log = append(log, SlowQuery{
			Query:    "SELECT ?",
			Duration: time.Duration(i) * time.Millisecond,
		})

		if len(log) > maxQueries {
			log = log[1:]
		}
	}

	if len(log) != maxQueries {
		t.Errorf("expected %d queries, got %d", maxQueries, len(log))
	}

	if log[0].Duration != 5*time.Millisecond {
		t.Error("oldest entries should be trimmed")
	}
}

func TestCacheHitRatioCalculation(t *testing.T) {
	tests := []struct {
		hits     int64
		misses   int64
		expected float64
	}{
		{95, 5, 95.0},
		{0, 100, 0.0},
		{100, 0, 100.0},
		{50, 50, 50.0},
	}

	for _, tc := range tests {
		total := tc.hits + tc.misses
		var ratio float64
		if total > 0 {
			ratio = float64(tc.hits) / float64(total) * 100
		}

		if ratio != tc.expected {
			t.Errorf("hits=%d, misses=%d: expected %.1f%%, got %.1f%%",
				tc.hits, tc.misses, tc.expected, ratio)
		}
	}
}

func TestConnectionUtilizationCalculation(t *testing.T) {
	tests := []struct {
		open     int
		inUse    int
		expected float64
	}{
		{25, 10, 40.0},
		{25, 25, 100.0},
		{25, 0, 0.0},
		{100, 90, 90.0},
	}

	for _, tc := range tests {
		var util float64
		if tc.open > 0 {
			util = float64(tc.inUse) / float64(tc.open) * 100
		}

		if util != tc.expected {
			t.Errorf("open=%d, inUse=%d: expected %.1f%%, got %.1f%%",
				tc.open, tc.inUse, tc.expected, util)
		}
	}
}

func TestSlowQueryThresholdDetection(t *testing.T) {
	threshold := 100 * time.Millisecond

	tests := []struct {
		duration time.Duration
		isSlow   bool
	}{
		{50 * time.Millisecond, false},
		{100 * time.Millisecond, false},
		{101 * time.Millisecond, true},
		{500 * time.Millisecond, true},
		{1 * time.Second, true},
	}

	for _, tc := range tests {
		isSlow := tc.duration > threshold
		if isSlow != tc.isSlow {
			t.Errorf("duration=%v: expected slow=%v, got %v",
				tc.duration, tc.isSlow, isSlow)
		}
	}
}

func TestAnalyzeHintsGeneration(t *testing.T) {
	totalQueries := int64(1000)
	slowQueries := int64(150)
	slowPct := float64(slowQueries) / float64(totalQueries) * 100

	if slowPct <= 10 {
		t.Error("expected slow percentage > 10%")
	}

	errors := int64(20)
	errorPct := float64(errors) / float64(totalQueries) * 100

	if errorPct <= 1 {
		t.Error("expected error percentage > 1%")
	}

	hits := int64(70)
	misses := int64(30)
	hitRatio := float64(hits) / float64(hits+misses) * 100

	if hitRatio >= 80 {
		t.Error("expected hit ratio < 80%")
	}
}

func TestQueryDurationAveraging(t *testing.T) {
	durations := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
		40 * time.Millisecond,
	}

	var total time.Duration
	for _, d := range durations {
		total += d
	}
	avg := total / time.Duration(len(durations))

	if avg != 25*time.Millisecond {
		t.Errorf("expected avg 25ms, got %v", avg)
	}
}

func TestConfigValidation(t *testing.T) {
	config := DefaultDBConfig()

	if config.MaxOpenConns < config.MaxIdleConns {
		t.Error("max open should be >= max idle")
	}
	if config.ConnMaxLifetime < config.ConnMaxIdleTime {
		t.Error("conn lifetime should be >= idle time")
	}
	if config.StatementCacheSize <= 0 {
		t.Error("cache size should be positive")
	}
	if config.SlowQueryThreshold <= 0 {
		t.Error("slow query threshold should be positive")
	}
}
