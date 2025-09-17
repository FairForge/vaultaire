// internal/intelligence/ml_pipeline.go
package intelligence

import (
	"database/sql"
	"time"

	"go.uber.org/zap"
)

type MLPipeline struct {
	db       *sql.DB
	logger   *zap.Logger
	features *FeatureExtractor
	model    PredictionModel
}

func NewMLPipeline(db *sql.DB, logger *zap.Logger) *MLPipeline {
	return &MLPipeline{
		db:       db,
		logger:   logger,
		features: &FeatureExtractor{},
		model:    &HeuristicModel{},
	}
}

type FeatureExtractor struct{}

func (fe *FeatureExtractor) Extract(event AccessEvent) []float64 {
	features := []float64{
		float64(event.Size),
		float64(event.Timestamp.Hour()),
		float64(event.Timestamp.Weekday()),
		boolToFloat(event.CacheHit),
		float64(event.Latency.Milliseconds()),
	}
	return features
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

type PredictionModel interface {
	Predict(features []float64) Prediction
}

type HeuristicModel struct{}

func (hm *HeuristicModel) Predict(features []float64) Prediction {
	size := features[0]
	hour := features[1]

	if size > 1<<28 {
		return Prediction{
			NextAccess:  time.Now().Add(24 * time.Hour),
			Temperature: "cold",
			ShouldCache: false,
		}
	}

	if hour >= 9 && hour <= 17 {
		return Prediction{
			NextAccess:  time.Now().Add(1 * time.Hour),
			Temperature: "hot",
			ShouldCache: true,
		}
	}

	return Prediction{
		NextAccess:  time.Now().Add(6 * time.Hour),
		Temperature: "warm",
		ShouldCache: size < 1<<20,
	}
}
