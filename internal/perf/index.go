// internal/perf/index.go
package perf

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// IndexType represents database index types
type IndexType string

const (
	IndexTypeBTree     IndexType = "btree"
	IndexTypeHash      IndexType = "hash"
	IndexTypeGIN       IndexType = "gin"
	IndexTypeGIST      IndexType = "gist"
	IndexTypeBRIN      IndexType = "brin"
	IndexTypeBloom     IndexType = "bloom"
	IndexTypeFullText  IndexType = "fulltext"
	IndexTypeComposite IndexType = "composite"
)

// IndexDefinition represents a database index
type IndexDefinition struct {
	Name      string
	Table     string
	Columns   []string
	Type      IndexType
	Unique    bool
	Partial   string   // WHERE clause for partial index
	Include   []string // INCLUDE columns (covering index)
	CreatedAt time.Time
}

// IndexUsageStats tracks index usage
type IndexUsageStats struct {
	IndexName     string
	TableName     string
	IndexScans    int64
	TuplesRead    int64
	TuplesFetched int64
	SizeBytes     int64
	LastUsed      time.Time
	IsUnused      bool
}

// IndexRecommendation represents an index suggestion
type IndexRecommendation struct {
	Table     string
	Columns   []string
	Type      IndexType
	Reason    string
	Impact    string
	CreateSQL string
	Priority  int // 1-10, higher is more important
}

// IndexOptimizer analyzes and recommends indexes
type IndexOptimizer struct {
	mu              sync.RWMutex
	indexes         map[string]*IndexDefinition
	usageStats      map[string]*IndexUsageStats
	recommendations []IndexRecommendation
	queryPatterns   map[string]int // query pattern -> count
	config          *IndexConfig
}

// IndexConfig configures the index optimizer
type IndexConfig struct {
	UnusedThresholdDays int
	MinScansForUseful   int64
	MaxRecommendations  int
}

// DefaultIndexConfig returns default configuration
func DefaultIndexConfig() *IndexConfig {
	return &IndexConfig{
		UnusedThresholdDays: 30,
		MinScansForUseful:   100,
		MaxRecommendations:  50,
	}
}

// NewIndexOptimizer creates a new index optimizer
func NewIndexOptimizer(config *IndexConfig) *IndexOptimizer {
	if config == nil {
		config = DefaultIndexConfig()
	}

	return &IndexOptimizer{
		indexes:         make(map[string]*IndexDefinition),
		usageStats:      make(map[string]*IndexUsageStats),
		recommendations: make([]IndexRecommendation, 0),
		queryPatterns:   make(map[string]int),
		config:          config,
	}
}

// RegisterIndex registers an existing index
func (o *IndexOptimizer) RegisterIndex(idx *IndexDefinition) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if idx.CreatedAt.IsZero() {
		idx.CreatedAt = time.Now()
	}
	o.indexes[idx.Name] = idx
}

// GetIndex returns an index definition
func (o *IndexOptimizer) GetIndex(name string) (*IndexDefinition, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	idx, ok := o.indexes[name]
	return idx, ok
}

// GetAllIndexes returns all registered indexes
func (o *IndexOptimizer) GetAllIndexes() map[string]*IndexDefinition {
	o.mu.RLock()
	defer o.mu.RUnlock()

	result := make(map[string]*IndexDefinition)
	for k, v := range o.indexes {
		result[k] = v
	}
	return result
}

// UpdateUsageStats updates index usage statistics
func (o *IndexOptimizer) UpdateUsageStats(stats *IndexUsageStats) {
	o.mu.Lock()
	defer o.mu.Unlock()

	stats.IsUnused = stats.IndexScans < o.config.MinScansForUseful
	o.usageStats[stats.IndexName] = stats
}

// GetUsageStats returns usage stats for an index
func (o *IndexOptimizer) GetUsageStats(indexName string) (*IndexUsageStats, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	stats, ok := o.usageStats[indexName]
	return stats, ok
}

// GetUnusedIndexes returns indexes that are rarely used
func (o *IndexOptimizer) GetUnusedIndexes() []*IndexUsageStats {
	o.mu.RLock()
	defer o.mu.RUnlock()

	var unused []*IndexUsageStats
	for _, stats := range o.usageStats {
		if stats.IsUnused {
			unused = append(unused, stats)
		}
	}
	return unused
}

// RecordQueryPattern records a query pattern for analysis
func (o *IndexOptimizer) RecordQueryPattern(pattern string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queryPatterns[pattern]++
}

// AnalyzeQuery analyzes a query and suggests indexes
func (o *IndexOptimizer) AnalyzeQuery(query string) []IndexRecommendation {
	recommendations := make([]IndexRecommendation, 0)
	upperQuery := strings.ToUpper(query)

	// Extract table and WHERE columns (simplified)
	table := extractTableName(upperQuery)
	whereColumns := extractWhereColumns(upperQuery)
	orderColumns := extractOrderByColumns(upperQuery)
	joinColumns := extractJoinColumns(upperQuery)

	// Recommend index for WHERE clause columns
	if len(whereColumns) > 0 && table != "" {
		rec := IndexRecommendation{
			Table:    table,
			Columns:  whereColumns,
			Type:     IndexTypeBTree,
			Reason:   "Frequently filtered columns",
			Impact:   "high",
			Priority: 8,
		}
		rec.CreateSQL = o.generateCreateSQL(rec)
		recommendations = append(recommendations, rec)
	}

	// Recommend index for ORDER BY
	if len(orderColumns) > 0 && table != "" {
		rec := IndexRecommendation{
			Table:    table,
			Columns:  orderColumns,
			Type:     IndexTypeBTree,
			Reason:   "Sort optimization",
			Impact:   "medium",
			Priority: 5,
		}
		rec.CreateSQL = o.generateCreateSQL(rec)
		recommendations = append(recommendations, rec)
	}

	// Recommend index for JOIN columns
	if len(joinColumns) > 0 && table != "" {
		rec := IndexRecommendation{
			Table:    table,
			Columns:  joinColumns,
			Type:     IndexTypeBTree,
			Reason:   "Join optimization",
			Impact:   "high",
			Priority: 9,
		}
		rec.CreateSQL = o.generateCreateSQL(rec)
		recommendations = append(recommendations, rec)
	}

	return recommendations
}

func extractTableName(query string) string {
	// Simple extraction - find FROM table
	idx := strings.Index(query, "FROM ")
	if idx == -1 {
		return ""
	}
	rest := query[idx+5:]
	parts := strings.Fields(rest)
	if len(parts) > 0 {
		return strings.ToLower(strings.TrimSuffix(parts[0], ","))
	}
	return ""
}

func extractWhereColumns(query string) []string {
	columns := make([]string, 0)

	idx := strings.Index(query, "WHERE ")
	if idx == -1 {
		return columns
	}

	// Simple pattern matching for column = value
	rest := query[idx+6:]
	// Stop at ORDER BY, GROUP BY, LIMIT, etc.
	for _, stop := range []string{"ORDER BY", "GROUP BY", "LIMIT", "HAVING"} {
		if i := strings.Index(rest, stop); i != -1 {
			rest = rest[:i]
		}
	}

	// Find column names (very simplified)
	parts := strings.Fields(rest)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Skip operators and values
		if p == "=" || p == ">" || p == "<" || p == "AND" || p == "OR" || p == "IN" || p == "LIKE" {
			continue
		}
		if strings.HasPrefix(p, "'") || strings.HasPrefix(p, "(") {
			continue
		}
		if len(p) > 0 && p[0] >= 'A' && p[0] <= 'Z' {
			// Looks like a column name
			col := strings.ToLower(strings.TrimSuffix(p, ","))
			if col != "" && !contains(columns, col) {
				columns = append(columns, col)
			}
		}
	}

	return columns
}

func extractOrderByColumns(query string) []string {
	columns := make([]string, 0)

	idx := strings.Index(query, "ORDER BY ")
	if idx == -1 {
		return columns
	}

	rest := query[idx+9:]
	// Stop at LIMIT
	if i := strings.Index(rest, "LIMIT"); i != -1 {
		rest = rest[:i]
	}

	parts := strings.Split(rest, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimSuffix(p, " ASC")
		p = strings.TrimSuffix(p, " DESC")
		if p != "" {
			columns = append(columns, strings.ToLower(p))
		}
	}

	return columns
}

func extractJoinColumns(query string) []string {
	columns := make([]string, 0)

	// Find ON clauses
	rest := query
	for {
		idx := strings.Index(rest, " ON ")
		if idx == -1 {
			break
		}
		rest = rest[idx+4:]

		// Get the join condition
		endIdx := strings.Index(rest, " ")
		if endIdx == -1 {
			endIdx = len(rest)
		}
		condition := rest[:endIdx]

		// Extract column from table.column = other.column
		parts := strings.Split(condition, "=")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if dotIdx := strings.Index(p, "."); dotIdx != -1 {
				col := strings.ToLower(p[dotIdx+1:])
				if col != "" && !contains(columns, col) {
					columns = append(columns, col)
				}
			}
		}
	}

	return columns
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func (o *IndexOptimizer) generateCreateSQL(rec IndexRecommendation) string {
	indexName := fmt.Sprintf("idx_%s_%s", rec.Table, strings.Join(rec.Columns, "_"))
	columns := strings.Join(rec.Columns, ", ")

	sql := fmt.Sprintf("CREATE INDEX %s ON %s", indexName, rec.Table)

	switch rec.Type {
	case IndexTypeGIN:
		sql += fmt.Sprintf(" USING gin (%s)", columns)
	case IndexTypeGIST:
		sql += fmt.Sprintf(" USING gist (%s)", columns)
	case IndexTypeBRIN:
		sql += fmt.Sprintf(" USING brin (%s)", columns)
	case IndexTypeHash:
		sql += fmt.Sprintf(" USING hash (%s)", columns)
	default:
		sql += fmt.Sprintf(" (%s)", columns)
	}

	return sql
}

// GenerateRecommendations generates index recommendations from query patterns
func (o *IndexOptimizer) GenerateRecommendations() []IndexRecommendation {
	o.mu.Lock()
	defer o.mu.Unlock()

	recommendations := make([]IndexRecommendation, 0)

	// Analyze frequent query patterns
	for pattern, count := range o.queryPatterns {
		if count < 10 { // Only consider patterns seen 10+ times
			continue
		}

		recs := o.AnalyzeQuery(pattern)
		for _, rec := range recs {
			// Boost priority based on frequency
			rec.Priority = min(10, rec.Priority+count/100)
			recommendations = append(recommendations, rec)
		}
	}

	// Sort by priority
	for i := 0; i < len(recommendations)-1; i++ {
		for j := i + 1; j < len(recommendations); j++ {
			if recommendations[j].Priority > recommendations[i].Priority {
				recommendations[i], recommendations[j] = recommendations[j], recommendations[i]
			}
		}
	}

	// Limit recommendations
	if len(recommendations) > o.config.MaxRecommendations {
		recommendations = recommendations[:o.config.MaxRecommendations]
	}

	o.recommendations = recommendations
	return recommendations
}

// GetRecommendations returns current recommendations
func (o *IndexOptimizer) GetRecommendations() []IndexRecommendation {
	o.mu.RLock()
	defer o.mu.RUnlock()

	result := make([]IndexRecommendation, len(o.recommendations))
	copy(result, o.recommendations)
	return result
}

// SuggestIndexType suggests the best index type for a use case
func SuggestIndexType(useCase string) IndexType {
	switch strings.ToLower(useCase) {
	case "equality", "range", "sorting":
		return IndexTypeBTree
	case "hash_lookup", "exact_match":
		return IndexTypeHash
	case "array", "jsonb", "fulltext_search":
		return IndexTypeGIN
	case "geometric", "range_types":
		return IndexTypeGIST
	case "large_table", "sequential":
		return IndexTypeBRIN
	case "multi_column_filter":
		return IndexTypeBloom
	default:
		return IndexTypeBTree
	}
}

// IndexReport represents an index analysis report
type IndexReport struct {
	GeneratedAt        time.Time
	TotalIndexes       int
	UnusedIndexes      int
	TotalSizeBytes     int64
	UnusedSizeBytes    int64
	Recommendations    int
	TopUnused          []*IndexUsageStats
	TopRecommendations []IndexRecommendation
}

// GenerateReport generates an index analysis report
func (o *IndexOptimizer) GenerateReport() *IndexReport {
	o.mu.RLock()
	defer o.mu.RUnlock()

	report := &IndexReport{
		GeneratedAt:  time.Now(),
		TotalIndexes: len(o.indexes),
	}

	var unusedList []*IndexUsageStats
	for _, stats := range o.usageStats {
		report.TotalSizeBytes += stats.SizeBytes
		if stats.IsUnused {
			report.UnusedIndexes++
			report.UnusedSizeBytes += stats.SizeBytes
			unusedList = append(unusedList, stats)
		}
	}

	// Top 5 unused by size
	for i := 0; i < len(unusedList)-1; i++ {
		for j := i + 1; j < len(unusedList); j++ {
			if unusedList[j].SizeBytes > unusedList[i].SizeBytes {
				unusedList[i], unusedList[j] = unusedList[j], unusedList[i]
			}
		}
	}
	if len(unusedList) > 5 {
		unusedList = unusedList[:5]
	}
	report.TopUnused = unusedList

	report.Recommendations = len(o.recommendations)
	if len(o.recommendations) > 5 {
		report.TopRecommendations = o.recommendations[:5]
	} else {
		report.TopRecommendations = o.recommendations
	}

	return report
}

// CommonIndexPatterns returns common index patterns for storage systems
func CommonIndexPatterns() []IndexDefinition {
	return []IndexDefinition{
		{
			Name:    "idx_objects_bucket_key",
			Table:   "objects",
			Columns: []string{"bucket_id", "key"},
			Type:    IndexTypeBTree,
			Unique:  true,
		},
		{
			Name:    "idx_objects_tenant",
			Table:   "objects",
			Columns: []string{"tenant_id"},
			Type:    IndexTypeBTree,
		},
		{
			Name:    "idx_objects_created",
			Table:   "objects",
			Columns: []string{"created_at"},
			Type:    IndexTypeBRIN,
		},
		{
			Name:    "idx_objects_metadata",
			Table:   "objects",
			Columns: []string{"metadata"},
			Type:    IndexTypeGIN,
		},
		{
			Name:    "idx_buckets_tenant_name",
			Table:   "buckets",
			Columns: []string{"tenant_id", "name"},
			Type:    IndexTypeBTree,
			Unique:  true,
		},
		{
			Name:    "idx_audit_tenant_time",
			Table:   "audit_logs",
			Columns: []string{"tenant_id", "created_at"},
			Type:    IndexTypeBTree,
		},
		{
			Name:    "idx_usage_tenant_period",
			Table:   "usage_records",
			Columns: []string{"tenant_id", "period_start"},
			Type:    IndexTypeBTree,
		},
	}
}
