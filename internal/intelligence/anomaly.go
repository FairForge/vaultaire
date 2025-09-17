// internal/intelligence/anomaly.go
package intelligence

import (
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// AnomalyDetector detects unusual patterns
type AnomalyDetector struct {
	db        *sql.DB
	logger    *zap.Logger
	threshold map[string]float64
}

func NewAnomalyDetector(db *sql.DB, logger *zap.Logger) *AnomalyDetector {
	return &AnomalyDetector{
		db:        db,
		logger:    logger,
		threshold: make(map[string]float64),
	}
}

func (ad *AnomalyDetector) IsAnomaly(event AccessEvent) bool {
	if event.Latency > 10*time.Second {
		return true
	}
	if event.Size > 1<<30 && event.CacheHit {
		return true
	}
	return false
}

func (ad *AnomalyDetector) Report(event AccessEvent) {
	query := `
		INSERT INTO access_anomalies (tenant_id, anomaly_type, severity, description)
		VALUES ($1, $2, $3, $4)
	`
	_, err := ad.db.Exec(query, event.TenantID, "high_latency", "low", "Unusual latency detected")
	if err != nil {
		ad.logger.Error("failed to report anomaly", zap.Error(err))
	}
}

// Optimizer provides recommendations
type Optimizer struct {
	db       *sql.DB
	patterns *PatternAnalyzer
}

func NewOptimizer(db *sql.DB, patterns *PatternAnalyzer) *Optimizer {
	return &Optimizer{db: db, patterns: patterns}
}

func (o *Optimizer) GetRecommendations(tenantID string) ([]Recommendation, error) {
	var recs []Recommendation

	// Get hot data
	query := `
		SELECT artifact_key, access_count, temperature
		FROM access_patterns
		WHERE tenant_id = $1 AND access_count > 10
		ORDER BY access_count DESC
		LIMIT 10
	`

	rows, err := o.db.Query(query, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key string
		var count int
		var temp string
		_ = rows.Scan(&key, &count, &temp)

		if count > 100 && temp == "cold" {
			recs = append(recs, Recommendation{
				Type:   "promote_to_cache",
				Target: key,
				Reason: "High access count but marked cold",
				Impact: "Reduce latency by 90%",
			})
		}
	}

	return recs, nil
}
