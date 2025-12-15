// internal/perf/query.go
package perf

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// QueryOptimizer optimizes and analyzes SQL queries
type QueryOptimizer struct {
	mu         sync.RWMutex
	queryStats map[string]*QueryStats
	queryPlans map[string]*QueryPlan
	config     *QueryConfig
	normalizer *QueryNormalizer
	maxQueries int
}

// QueryConfig configures the query optimizer
type QueryConfig struct {
	EnableNormalization bool
	EnablePlanCache     bool
	PlanCacheTTL        time.Duration
	MaxCachedPlans      int
	SlowQueryThreshold  time.Duration
	AnalyzeThreshold    int // Analyze after N executions
}

// DefaultQueryConfig returns default configuration
func DefaultQueryConfig() *QueryConfig {
	return &QueryConfig{
		EnableNormalization: true,
		EnablePlanCache:     true,
		PlanCacheTTL:        10 * time.Minute,
		MaxCachedPlans:      500,
		SlowQueryThreshold:  100 * time.Millisecond,
		AnalyzeThreshold:    100,
	}
}

// QueryStats tracks statistics for a query pattern
type QueryStats struct {
	QueryHash      string
	NormalizedSQL  string
	ExecutionCount int64
	TotalDuration  time.Duration
	MinDuration    time.Duration
	MaxDuration    time.Duration
	AvgDuration    time.Duration
	LastExecuted   time.Time
	SlowCount      int64
	ErrorCount     int64
}

// QueryPlan represents a cached query execution plan
type QueryPlan struct {
	QueryHash     string
	PlanType      string
	EstimatedCost float64
	ActualCost    float64
	RowsEstimate  int64
	RowsActual    int64
	CachedAt      time.Time
	ExpiresAt     time.Time
	Hints         []string
}

// QueryNormalizer normalizes SQL queries for comparison
type QueryNormalizer struct {
	numberPattern     *regexp.Regexp
	stringPattern     *regexp.Regexp
	inListPattern     *regexp.Regexp
	whitespacePattern *regexp.Regexp
}

// NewQueryNormalizer creates a new query normalizer
func NewQueryNormalizer() *QueryNormalizer {
	return &QueryNormalizer{
		numberPattern:     regexp.MustCompile(`\b\d+\b`),
		stringPattern:     regexp.MustCompile(`'[^']*'`),
		inListPattern:     regexp.MustCompile(`IN\s*\([^)]+\)`),
		whitespacePattern: regexp.MustCompile(`\s+`),
	}
}

// Normalize normalizes a SQL query
func (n *QueryNormalizer) Normalize(query string) string {
	normalized := query

	// Replace string literals
	normalized = n.stringPattern.ReplaceAllString(normalized, "'?'")

	// Replace numbers
	normalized = n.numberPattern.ReplaceAllString(normalized, "?")

	// Replace IN lists
	normalized = n.inListPattern.ReplaceAllString(normalized, "IN (?)")

	// Normalize whitespace
	normalized = n.whitespacePattern.ReplaceAllString(normalized, " ")

	// Trim and uppercase
	normalized = strings.TrimSpace(normalized)
	normalized = strings.ToUpper(normalized)

	return normalized
}

// Hash returns a hash of the normalized query
func (n *QueryNormalizer) Hash(query string) string {
	normalized := n.Normalize(query)
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:8])
}

// NewQueryOptimizer creates a new query optimizer
func NewQueryOptimizer(config *QueryConfig) *QueryOptimizer {
	if config == nil {
		config = DefaultQueryConfig()
	}

	return &QueryOptimizer{
		queryStats: make(map[string]*QueryStats),
		queryPlans: make(map[string]*QueryPlan),
		config:     config,
		normalizer: NewQueryNormalizer(),
		maxQueries: 10000,
	}
}

// RecordExecution records a query execution
func (o *QueryOptimizer) RecordExecution(query string, duration time.Duration, err error) {
	hash := o.normalizer.Hash(query)

	o.mu.Lock()
	defer o.mu.Unlock()

	stats, ok := o.queryStats[hash]
	if !ok {
		stats = &QueryStats{
			QueryHash:     hash,
			NormalizedSQL: o.normalizer.Normalize(query),
			MinDuration:   duration,
			MaxDuration:   duration,
		}
		o.queryStats[hash] = stats

		// Trim if too many
		if len(o.queryStats) > o.maxQueries {
			o.trimOldestQueries()
		}
	}

	stats.ExecutionCount++
	stats.TotalDuration += duration
	stats.LastExecuted = time.Now()

	if duration < stats.MinDuration {
		stats.MinDuration = duration
	}
	if duration > stats.MaxDuration {
		stats.MaxDuration = duration
	}
	stats.AvgDuration = stats.TotalDuration / time.Duration(stats.ExecutionCount)

	if duration > o.config.SlowQueryThreshold {
		stats.SlowCount++
	}
	if err != nil {
		stats.ErrorCount++
	}
}

func (o *QueryOptimizer) trimOldestQueries() {
	// Remove 10% of oldest queries
	toRemove := len(o.queryStats) / 10
	if toRemove == 0 {
		toRemove = 1
	}

	// Find oldest by LastExecuted
	type queryTime struct {
		hash string
		time time.Time
	}
	queries := make([]queryTime, 0, len(o.queryStats))
	for hash, stats := range o.queryStats {
		queries = append(queries, queryTime{hash, stats.LastExecuted})
	}

	// Simple sort - find oldest
	for i := 0; i < toRemove; i++ {
		oldestIdx := 0
		for j := 1; j < len(queries); j++ {
			if queries[j].time.Before(queries[oldestIdx].time) {
				oldestIdx = j
			}
		}
		delete(o.queryStats, queries[oldestIdx].hash)
		queries = append(queries[:oldestIdx], queries[oldestIdx+1:]...)
	}
}

// GetStats returns stats for a query
func (o *QueryOptimizer) GetStats(query string) (*QueryStats, bool) {
	hash := o.normalizer.Hash(query)

	o.mu.RLock()
	defer o.mu.RUnlock()

	stats, ok := o.queryStats[hash]
	return stats, ok
}

// GetAllStats returns all query stats
func (o *QueryOptimizer) GetAllStats() map[string]*QueryStats {
	o.mu.RLock()
	defer o.mu.RUnlock()

	result := make(map[string]*QueryStats)
	for k, v := range o.queryStats {
		result[k] = v
	}
	return result
}

// GetTopSlowQueries returns the slowest queries
func (o *QueryOptimizer) GetTopSlowQueries(n int) []*QueryStats {
	o.mu.RLock()
	defer o.mu.RUnlock()

	// Collect all stats
	all := make([]*QueryStats, 0, len(o.queryStats))
	for _, stats := range o.queryStats {
		all = append(all, stats)
	}

	// Sort by avg duration (simple bubble sort for small n)
	for i := 0; i < len(all)-1; i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].AvgDuration > all[i].AvgDuration {
				all[i], all[j] = all[j], all[i]
			}
		}
	}

	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// GetTopFrequentQueries returns the most frequently executed queries
func (o *QueryOptimizer) GetTopFrequentQueries(n int) []*QueryStats {
	o.mu.RLock()
	defer o.mu.RUnlock()

	all := make([]*QueryStats, 0, len(o.queryStats))
	for _, stats := range o.queryStats {
		all = append(all, stats)
	}

	// Sort by execution count
	for i := 0; i < len(all)-1; i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].ExecutionCount > all[i].ExecutionCount {
				all[i], all[j] = all[j], all[i]
			}
		}
	}

	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// CachePlan caches a query execution plan
func (o *QueryOptimizer) CachePlan(query string, plan *QueryPlan) {
	if !o.config.EnablePlanCache {
		return
	}

	hash := o.normalizer.Hash(query)

	o.mu.Lock()
	defer o.mu.Unlock()

	plan.QueryHash = hash
	plan.CachedAt = time.Now()
	plan.ExpiresAt = time.Now().Add(o.config.PlanCacheTTL)

	o.queryPlans[hash] = plan

	// Trim if too many
	if len(o.queryPlans) > o.config.MaxCachedPlans {
		o.trimExpiredPlans()
	}
}

func (o *QueryOptimizer) trimExpiredPlans() {
	now := time.Now()
	for hash, plan := range o.queryPlans {
		if plan.ExpiresAt.Before(now) {
			delete(o.queryPlans, hash)
		}
	}
}

// GetPlan retrieves a cached plan
func (o *QueryOptimizer) GetPlan(query string) (*QueryPlan, bool) {
	if !o.config.EnablePlanCache {
		return nil, false
	}

	hash := o.normalizer.Hash(query)

	o.mu.RLock()
	defer o.mu.RUnlock()

	plan, ok := o.queryPlans[hash]
	if !ok {
		return nil, false
	}

	// Check expiration
	if plan.ExpiresAt.Before(time.Now()) {
		return nil, false
	}

	return plan, true
}

// QueryHint represents a query optimization hint
type QueryHint struct {
	Type        string
	Description string
	Suggestion  string
	Impact      string
}

// AnalyzeQuery analyzes a query and returns optimization hints
func (o *QueryOptimizer) AnalyzeQuery(query string) []QueryHint {
	hints := make([]QueryHint, 0)
	upperQuery := strings.ToUpper(query)

	// Check for SELECT *
	if strings.Contains(upperQuery, "SELECT *") {
		hints = append(hints, QueryHint{
			Type:        "column_selection",
			Description: "Query uses SELECT *",
			Suggestion:  "Specify only needed columns to reduce data transfer",
			Impact:      "medium",
		})
	}

	// Check for missing WHERE clause
	if strings.Contains(upperQuery, "SELECT") && !strings.Contains(upperQuery, "WHERE") {
		if !strings.Contains(upperQuery, "LIMIT") {
			hints = append(hints, QueryHint{
				Type:        "full_table_scan",
				Description: "Query has no WHERE clause",
				Suggestion:  "Add WHERE clause or LIMIT to avoid full table scan",
				Impact:      "high",
			})
		}
	}

	// Check for LIKE with leading wildcard
	if strings.Contains(upperQuery, "LIKE '%") || strings.Contains(upperQuery, "LIKE '_") {
		hints = append(hints, QueryHint{
			Type:        "index_usage",
			Description: "LIKE pattern starts with wildcard",
			Suggestion:  "Leading wildcards prevent index usage; consider full-text search",
			Impact:      "high",
		})
	}

	// Check for OR in WHERE clause
	if strings.Contains(upperQuery, " OR ") {
		hints = append(hints, QueryHint{
			Type:        "or_clause",
			Description: "Query uses OR in WHERE clause",
			Suggestion:  "Consider using UNION or IN clause for better index usage",
			Impact:      "medium",
		})
	}

	// Check for NOT IN
	if strings.Contains(upperQuery, "NOT IN") {
		hints = append(hints, QueryHint{
			Type:        "not_in",
			Description: "Query uses NOT IN",
			Suggestion:  "Consider using NOT EXISTS or LEFT JOIN for better performance",
			Impact:      "medium",
		})
	}

	// Check for functions on indexed columns
	if regexp.MustCompile(`WHERE\s+\w+\s*\(`).MatchString(upperQuery) {
		hints = append(hints, QueryHint{
			Type:        "function_on_column",
			Description: "Function applied to column in WHERE clause",
			Suggestion:  "Functions on columns prevent index usage; restructure if possible",
			Impact:      "high",
		})
	}

	// Check for ORDER BY without LIMIT
	if strings.Contains(upperQuery, "ORDER BY") && !strings.Contains(upperQuery, "LIMIT") {
		hints = append(hints, QueryHint{
			Type:        "unbounded_sort",
			Description: "ORDER BY without LIMIT",
			Suggestion:  "Add LIMIT to avoid sorting entire result set",
			Impact:      "medium",
		})
	}

	// Check for multiple table joins
	joinCount := strings.Count(upperQuery, "JOIN ")
	if joinCount > 3 {
		hints = append(hints, QueryHint{
			Type:        "complex_join",
			Description: fmt.Sprintf("Query has %d JOINs", joinCount),
			Suggestion:  "Consider denormalization or breaking into smaller queries",
			Impact:      "high",
		})
	}

	return hints
}

// QueryReport represents a query performance report
type QueryReport struct {
	GeneratedAt     time.Time
	TotalQueries    int
	UniqueQueries   int
	TotalExecutions int64
	SlowQueries     int
	AvgDuration     time.Duration
	TopSlow         []*QueryStats
	TopFrequent     []*QueryStats
	Recommendations []string
}

// GenerateReport generates a query performance report
func (o *QueryOptimizer) GenerateReport() *QueryReport {
	o.mu.RLock()
	defer o.mu.RUnlock()

	report := &QueryReport{
		GeneratedAt:   time.Now(),
		UniqueQueries: len(o.queryStats),
		TopSlow:       make([]*QueryStats, 0),
		TopFrequent:   make([]*QueryStats, 0),
	}

	var totalDuration time.Duration
	var slowCount int

	for _, stats := range o.queryStats {
		report.TotalExecutions += stats.ExecutionCount
		totalDuration += stats.TotalDuration
		if stats.SlowCount > 0 {
			slowCount++
		}
	}

	report.SlowQueries = slowCount
	report.TotalQueries = int(report.TotalExecutions)

	if report.TotalExecutions > 0 {
		report.AvgDuration = totalDuration / time.Duration(report.TotalExecutions)
	}

	// Generate recommendations
	if slowCount > len(o.queryStats)/10 {
		report.Recommendations = append(report.Recommendations,
			"More than 10% of unique queries are slow - review indexing strategy")
	}

	return report
}

// Reset clears all stats
func (o *QueryOptimizer) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.queryStats = make(map[string]*QueryStats)
	o.queryPlans = make(map[string]*QueryPlan)
}
