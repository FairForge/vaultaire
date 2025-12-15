// internal/perf/index_test.go
package perf

import (
	"testing"
	"time"
)

func TestDefaultIndexConfig(t *testing.T) {
	config := DefaultIndexConfig()

	if config.UnusedThresholdDays != 30 {
		t.Errorf("expected 30 days, got %d", config.UnusedThresholdDays)
	}
	if config.MinScansForUseful != 100 {
		t.Errorf("expected 100 min scans, got %d", config.MinScansForUseful)
	}
	if config.MaxRecommendations != 50 {
		t.Errorf("expected 50 max recommendations, got %d", config.MaxRecommendations)
	}
}

func TestNewIndexOptimizer(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	if opt == nil {
		t.Fatal("expected non-nil optimizer")
	}
	if opt.config == nil {
		t.Error("expected default config")
	}
}

func TestIndexOptimizerRegisterIndex(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	idx := &IndexDefinition{
		Name:    "idx_users_email",
		Table:   "users",
		Columns: []string{"email"},
		Type:    IndexTypeBTree,
		Unique:  true,
	}

	opt.RegisterIndex(idx)

	retrieved, ok := opt.GetIndex("idx_users_email")
	if !ok {
		t.Fatal("expected index")
	}
	if retrieved.Table != "users" {
		t.Error("index not registered correctly")
	}
	if retrieved.CreatedAt.IsZero() {
		t.Error("expected created_at to be set")
	}
}

func TestIndexOptimizerGetAllIndexes(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	opt.RegisterIndex(&IndexDefinition{Name: "idx1", Table: "t1", Columns: []string{"c1"}})
	opt.RegisterIndex(&IndexDefinition{Name: "idx2", Table: "t2", Columns: []string{"c2"}})

	all := opt.GetAllIndexes()
	if len(all) != 2 {
		t.Errorf("expected 2 indexes, got %d", len(all))
	}
}

func TestIndexOptimizerUpdateUsageStats(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	stats := &IndexUsageStats{
		IndexName:  "idx_test",
		TableName:  "test",
		IndexScans: 50, // Below threshold
		SizeBytes:  1024,
	}

	opt.UpdateUsageStats(stats)

	retrieved, ok := opt.GetUsageStats("idx_test")
	if !ok {
		t.Fatal("expected stats")
	}
	if !retrieved.IsUnused {
		t.Error("expected index to be marked unused (below threshold)")
	}
}

func TestIndexOptimizerUsedIndex(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	stats := &IndexUsageStats{
		IndexName:  "idx_test",
		IndexScans: 500, // Above threshold
	}

	opt.UpdateUsageStats(stats)

	retrieved, _ := opt.GetUsageStats("idx_test")
	if retrieved.IsUnused {
		t.Error("expected index to be marked used")
	}
}

func TestIndexOptimizerGetUnusedIndexes(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	opt.UpdateUsageStats(&IndexUsageStats{IndexName: "used", IndexScans: 500})
	opt.UpdateUsageStats(&IndexUsageStats{IndexName: "unused1", IndexScans: 5})
	opt.UpdateUsageStats(&IndexUsageStats{IndexName: "unused2", IndexScans: 10})

	unused := opt.GetUnusedIndexes()
	if len(unused) != 2 {
		t.Errorf("expected 2 unused indexes, got %d", len(unused))
	}
}

func TestIndexOptimizerRecordQueryPattern(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	pattern := "SELECT * FROM users WHERE email = ?"
	opt.RecordQueryPattern(pattern)
	opt.RecordQueryPattern(pattern)

	opt.mu.RLock()
	count := opt.queryPatterns[pattern]
	opt.mu.RUnlock()

	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}
}

func TestIndexOptimizerAnalyzeQueryWhere(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	recs := opt.AnalyzeQuery("SELECT * FROM users WHERE email = 'test@test.com'")

	foundWhere := false
	for _, r := range recs {
		if r.Reason == "Frequently filtered columns" {
			foundWhere = true
		}
	}
	if !foundWhere {
		t.Error("expected WHERE column recommendation")
	}
}

func TestIndexOptimizerAnalyzeQueryOrderBy(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	recs := opt.AnalyzeQuery("SELECT * FROM users WHERE id = 1 ORDER BY created_at DESC")

	foundOrder := false
	for _, r := range recs {
		if r.Reason == "Sort optimization" {
			foundOrder = true
		}
	}
	if !foundOrder {
		t.Error("expected ORDER BY recommendation")
	}
}

func TestIndexOptimizerGenerateRecommendations(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	// Record pattern many times
	for i := 0; i < 20; i++ {
		opt.RecordQueryPattern("SELECT * FROM users WHERE status = 'active'")
	}

	recs := opt.GenerateRecommendations()
	if len(recs) == 0 {
		t.Error("expected recommendations")
	}
}

func TestIndexOptimizerGetRecommendations(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	for i := 0; i < 20; i++ {
		opt.RecordQueryPattern("SELECT * FROM orders WHERE customer_id = 123")
	}
	opt.GenerateRecommendations()

	recs := opt.GetRecommendations()
	if len(recs) == 0 {
		t.Error("expected recommendations")
	}
}

func TestSuggestIndexType(t *testing.T) {
	tests := []struct {
		useCase  string
		expected IndexType
	}{
		{"equality", IndexTypeBTree},
		{"range", IndexTypeBTree},
		{"hash_lookup", IndexTypeHash},
		{"array", IndexTypeGIN},
		{"jsonb", IndexTypeGIN},
		{"geometric", IndexTypeGIST},
		{"large_table", IndexTypeBRIN},
		{"unknown", IndexTypeBTree},
	}

	for _, tc := range tests {
		result := SuggestIndexType(tc.useCase)
		if result != tc.expected {
			t.Errorf("SuggestIndexType(%q) = %v, want %v", tc.useCase, result, tc.expected)
		}
	}
}

func TestIndexOptimizerGenerateReport(t *testing.T) {
	opt := NewIndexOptimizer(nil)

	opt.RegisterIndex(&IndexDefinition{Name: "idx1", Table: "t1", Columns: []string{"c1"}})
	opt.RegisterIndex(&IndexDefinition{Name: "idx2", Table: "t2", Columns: []string{"c2"}})

	opt.UpdateUsageStats(&IndexUsageStats{IndexName: "idx1", IndexScans: 500, SizeBytes: 1000})
	opt.UpdateUsageStats(&IndexUsageStats{IndexName: "idx2", IndexScans: 5, SizeBytes: 2000})

	report := opt.GenerateReport()

	if report.TotalIndexes != 2 {
		t.Errorf("expected 2 indexes, got %d", report.TotalIndexes)
	}
	if report.UnusedIndexes != 1 {
		t.Errorf("expected 1 unused, got %d", report.UnusedIndexes)
	}
	if report.TotalSizeBytes != 3000 {
		t.Errorf("expected 3000 bytes, got %d", report.TotalSizeBytes)
	}
	if report.UnusedSizeBytes != 2000 {
		t.Errorf("expected 2000 unused bytes, got %d", report.UnusedSizeBytes)
	}
}

func TestCommonIndexPatterns(t *testing.T) {
	patterns := CommonIndexPatterns()

	if len(patterns) == 0 {
		t.Fatal("expected common patterns")
	}

	foundObjects := false
	for _, p := range patterns {
		if p.Table == "objects" {
			foundObjects = true
		}
	}
	if !foundObjects {
		t.Error("expected objects table index")
	}
}

func TestIndexTypes(t *testing.T) {
	types := []IndexType{
		IndexTypeBTree,
		IndexTypeHash,
		IndexTypeGIN,
		IndexTypeGIST,
		IndexTypeBRIN,
		IndexTypeBloom,
		IndexTypeFullText,
		IndexTypeComposite,
	}

	for _, it := range types {
		if it == "" {
			t.Error("index type should not be empty")
		}
	}
}

func TestExtractTableName(t *testing.T) {
	tests := []struct {
		query    string
		expected string
	}{
		{"SELECT * FROM USERS WHERE ID = 1", "users"},
		{"SELECT * FROM ORDERS", "orders"},
		{"SELECT * FROM USER_ACCOUNTS WHERE ACTIVE = TRUE", "user_accounts"},
	}

	for _, tc := range tests {
		result := extractTableName(tc.query)
		if result != tc.expected {
			t.Errorf("extractTableName(%q) = %q, want %q", tc.query, result, tc.expected)
		}
	}
}

func TestIndexDefinitionFields(t *testing.T) {
	idx := &IndexDefinition{
		Name:      "idx_test",
		Table:     "test",
		Columns:   []string{"col1", "col2"},
		Type:      IndexTypeBTree,
		Unique:    true,
		Partial:   "WHERE active = true",
		Include:   []string{"col3"},
		CreatedAt: time.Now(),
	}

	if idx.Name == "" {
		t.Error("name should not be empty")
	}
	if len(idx.Columns) != 2 {
		t.Error("expected 2 columns")
	}
	if !idx.Unique {
		t.Error("expected unique")
	}
}

func TestIndexRecommendationFields(t *testing.T) {
	rec := IndexRecommendation{
		Table:     "users",
		Columns:   []string{"email"},
		Type:      IndexTypeBTree,
		Reason:    "Frequently filtered",
		Impact:    "high",
		CreateSQL: "CREATE INDEX idx_users_email ON users (email)",
		Priority:  8,
	}

	if rec.CreateSQL == "" {
		t.Error("create SQL should not be empty")
	}
	if rec.Priority < 1 || rec.Priority > 10 {
		t.Error("priority should be 1-10")
	}
}
