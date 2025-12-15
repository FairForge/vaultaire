// internal/perf/query_test.go
package perf

import (
	"testing"
	"time"
)

func TestDefaultQueryConfig(t *testing.T) {
	config := DefaultQueryConfig()

	if !config.EnableNormalization {
		t.Error("expected normalization enabled")
	}
	if !config.EnablePlanCache {
		t.Error("expected plan cache enabled")
	}
	if config.PlanCacheTTL != 10*time.Minute {
		t.Error("unexpected plan cache TTL")
	}
	if config.MaxCachedPlans != 500 {
		t.Errorf("expected 500 max plans, got %d", config.MaxCachedPlans)
	}
}

func TestQueryNormalizerNormalize(t *testing.T) {
	n := NewQueryNormalizer()

	tests := []struct {
		input    string
		expected string
	}{
		{
			"SELECT * FROM users WHERE id = 123",
			"SELECT * FROM USERS WHERE ID = ?",
		},
		{
			"SELECT name FROM users WHERE name = 'john'",
			"SELECT NAME FROM USERS WHERE NAME = '?'",
		},
		{
			"SELECT * FROM users WHERE id IN (1, 2, 3)",
			"SELECT * FROM USERS WHERE ID IN (?)",
		},
		{
			"SELECT   *   FROM   users",
			"SELECT * FROM USERS",
		},
	}

	for _, tc := range tests {
		result := n.Normalize(tc.input)
		if result != tc.expected {
			t.Errorf("Normalize(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestQueryNormalizerHash(t *testing.T) {
	n := NewQueryNormalizer()

	// Same query pattern should have same hash
	hash1 := n.Hash("SELECT * FROM users WHERE id = 1")
	hash2 := n.Hash("SELECT * FROM users WHERE id = 2")

	if hash1 != hash2 {
		t.Error("same query pattern should have same hash")
	}

	// Different queries should have different hash
	hash3 := n.Hash("SELECT * FROM orders WHERE id = 1")

	if hash1 == hash3 {
		t.Error("different queries should have different hash")
	}
}

func TestNewQueryOptimizer(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	if opt == nil {
		t.Fatal("expected non-nil optimizer")
	}
	if opt.config == nil {
		t.Error("expected default config")
	}
	if opt.normalizer == nil {
		t.Error("expected normalizer")
	}
}

func TestQueryOptimizerRecordExecution(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	opt.RecordExecution("SELECT * FROM users WHERE id = 1", 50*time.Millisecond, nil)

	stats, ok := opt.GetStats("SELECT * FROM users WHERE id = 2") // Same pattern
	if !ok {
		t.Fatal("expected stats for query")
	}

	if stats.ExecutionCount != 1 {
		t.Errorf("expected 1 execution, got %d", stats.ExecutionCount)
	}
	if stats.AvgDuration != 50*time.Millisecond {
		t.Errorf("expected 50ms avg, got %v", stats.AvgDuration)
	}
}

func TestQueryOptimizerMultipleExecutions(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	durations := []time.Duration{10, 20, 30, 40, 50}
	for _, d := range durations {
		opt.RecordExecution("SELECT * FROM test", d*time.Millisecond, nil)
	}

	stats, _ := opt.GetStats("SELECT * FROM test")

	if stats.ExecutionCount != 5 {
		t.Errorf("expected 5 executions, got %d", stats.ExecutionCount)
	}
	if stats.MinDuration != 10*time.Millisecond {
		t.Errorf("expected min 10ms, got %v", stats.MinDuration)
	}
	if stats.MaxDuration != 50*time.Millisecond {
		t.Errorf("expected max 50ms, got %v", stats.MaxDuration)
	}
	if stats.AvgDuration != 30*time.Millisecond {
		t.Errorf("expected avg 30ms, got %v", stats.AvgDuration)
	}
}

func TestQueryOptimizerSlowQueryTracking(t *testing.T) {
	config := &QueryConfig{
		SlowQueryThreshold: 50 * time.Millisecond,
	}
	opt := NewQueryOptimizer(config)

	opt.RecordExecution("SELECT 1", 30*time.Millisecond, nil)  // Fast
	opt.RecordExecution("SELECT 1", 100*time.Millisecond, nil) // Slow

	stats, _ := opt.GetStats("SELECT 1")
	if stats.SlowCount != 1 {
		t.Errorf("expected 1 slow query, got %d", stats.SlowCount)
	}
}

func TestQueryOptimizerGetAllStats(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	opt.RecordExecution("SELECT * FROM users", 10*time.Millisecond, nil)
	opt.RecordExecution("SELECT * FROM orders", 20*time.Millisecond, nil)

	all := opt.GetAllStats()
	if len(all) != 2 {
		t.Errorf("expected 2 queries, got %d", len(all))
	}
}

func TestQueryOptimizerGetTopSlowQueries(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	opt.RecordExecution("SELECT * FROM fast", 10*time.Millisecond, nil)
	opt.RecordExecution("SELECT * FROM medium", 50*time.Millisecond, nil)
	opt.RecordExecution("SELECT * FROM slow", 100*time.Millisecond, nil)

	top := opt.GetTopSlowQueries(2)
	if len(top) != 2 {
		t.Fatalf("expected 2 queries, got %d", len(top))
	}
	if top[0].AvgDuration != 100*time.Millisecond {
		t.Error("slowest should be first")
	}
}

func TestQueryOptimizerGetTopFrequentQueries(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	for i := 0; i < 10; i++ {
		opt.RecordExecution("SELECT * FROM frequent", 10*time.Millisecond, nil)
	}
	opt.RecordExecution("SELECT * FROM rare", 10*time.Millisecond, nil)

	top := opt.GetTopFrequentQueries(1)
	if len(top) != 1 {
		t.Fatalf("expected 1 query, got %d", len(top))
	}
	if top[0].ExecutionCount != 10 {
		t.Errorf("expected 10 executions, got %d", top[0].ExecutionCount)
	}
}

func TestQueryOptimizerCachePlan(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	plan := &QueryPlan{
		PlanType:      "IndexScan",
		EstimatedCost: 100.5,
	}

	opt.CachePlan("SELECT * FROM users WHERE id = 1", plan)

	retrieved, ok := opt.GetPlan("SELECT * FROM users WHERE id = 2") // Same pattern
	if !ok {
		t.Fatal("expected cached plan")
	}
	if retrieved.PlanType != "IndexScan" {
		t.Error("plan not cached correctly")
	}
}

func TestQueryOptimizerPlanExpiration(t *testing.T) {
	config := &QueryConfig{
		EnablePlanCache: true,
		PlanCacheTTL:    1 * time.Millisecond,
	}
	opt := NewQueryOptimizer(config)

	opt.CachePlan("SELECT 1", &QueryPlan{PlanType: "SeqScan"})

	time.Sleep(5 * time.Millisecond)

	_, ok := opt.GetPlan("SELECT 1")
	if ok {
		t.Error("expected plan to be expired")
	}
}

func TestQueryOptimizerPlanCacheDisabled(t *testing.T) {
	config := &QueryConfig{
		EnablePlanCache: false,
	}
	opt := NewQueryOptimizer(config)

	opt.CachePlan("SELECT 1", &QueryPlan{PlanType: "SeqScan"})

	_, ok := opt.GetPlan("SELECT 1")
	if ok {
		t.Error("expected no plan when cache disabled")
	}
}

func TestQueryOptimizerAnalyzeQuery(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	// SELECT * hint
	hints := opt.AnalyzeQuery("SELECT * FROM users WHERE id = 1")
	found := false
	for _, h := range hints {
		if h.Type == "column_selection" {
			found = true
		}
	}
	if !found {
		t.Error("expected column_selection hint for SELECT *")
	}
}

func TestQueryOptimizerAnalyzeNoWhere(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	hints := opt.AnalyzeQuery("SELECT * FROM users")
	found := false
	for _, h := range hints {
		if h.Type == "full_table_scan" {
			found = true
		}
	}
	if !found {
		t.Error("expected full_table_scan hint")
	}
}

func TestQueryOptimizerAnalyzeLikeWildcard(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	hints := opt.AnalyzeQuery("SELECT * FROM users WHERE name LIKE '%john%'")
	found := false
	for _, h := range hints {
		if h.Type == "index_usage" {
			found = true
		}
	}
	if !found {
		t.Error("expected index_usage hint for leading wildcard")
	}
}

func TestQueryOptimizerAnalyzeComplexJoin(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	query := `SELECT * FROM a
		JOIN b ON a.id = b.a_id
		JOIN c ON b.id = c.b_id
		JOIN d ON c.id = d.c_id
		JOIN e ON d.id = e.d_id`

	hints := opt.AnalyzeQuery(query)
	found := false
	for _, h := range hints {
		if h.Type == "complex_join" {
			found = true
		}
	}
	if !found {
		t.Error("expected complex_join hint")
	}
}

func TestQueryOptimizerGenerateReport(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	opt.RecordExecution("SELECT * FROM a", 10*time.Millisecond, nil)
	opt.RecordExecution("SELECT * FROM b", 20*time.Millisecond, nil)
	opt.RecordExecution("SELECT * FROM b", 30*time.Millisecond, nil)

	report := opt.GenerateReport()

	if report.UniqueQueries != 2 {
		t.Errorf("expected 2 unique queries, got %d", report.UniqueQueries)
	}
	if report.TotalExecutions != 3 {
		t.Errorf("expected 3 total executions, got %d", report.TotalExecutions)
	}
}

func TestQueryOptimizerReset(t *testing.T) {
	opt := NewQueryOptimizer(nil)

	opt.RecordExecution("SELECT 1", 10*time.Millisecond, nil)
	opt.CachePlan("SELECT 1", &QueryPlan{})

	opt.Reset()

	all := opt.GetAllStats()
	if len(all) != 0 {
		t.Error("expected empty stats after reset")
	}

	_, ok := opt.GetPlan("SELECT 1")
	if ok {
		t.Error("expected no plan after reset")
	}
}

func TestQueryHintFields(t *testing.T) {
	hint := QueryHint{
		Type:        "test",
		Description: "test hint",
		Suggestion:  "do something",
		Impact:      "high",
	}

	if hint.Type == "" {
		t.Error("type should not be empty")
	}
	if hint.Impact != "high" {
		t.Error("unexpected impact")
	}
}

func TestQueryPlanFields(t *testing.T) {
	plan := &QueryPlan{
		QueryHash:     "abc123",
		PlanType:      "IndexScan",
		EstimatedCost: 100.5,
		ActualCost:    95.0,
		RowsEstimate:  1000,
		RowsActual:    980,
	}

	if plan.PlanType != "IndexScan" {
		t.Error("unexpected plan type")
	}
	if plan.EstimatedCost != 100.5 {
		t.Error("unexpected estimated cost")
	}
}
