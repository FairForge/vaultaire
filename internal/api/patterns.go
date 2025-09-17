// internal/api/patterns.go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/go-chi/chi/v5"
)

func (s *Server) setupPatternRoutes() {
	if s.db == nil {
		// No database, no pattern routes
		return
	}

	s.router.Route("/api/patterns", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleGetPatterns)
		r.Get("/hot", s.handleGetHotData)
		r.Get("/recommendations", s.handleGetRecommendations)
		r.Get("/anomalies", s.handleGetAnomalies)
		r.Post("/train", s.handleTriggerTraining)
		r.Get("/export", s.handleExportTrainingData)
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simple auth check - improve this later
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			http.Error(w, "Missing API key", http.StatusUnauthorized)
			return
		}
		// TODO: Validate API key against database
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleGetPatterns(w http.ResponseWriter, r *http.Request) {
	patterns, err := s.engine.GetAccessPatterns(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(patterns)
}

func (s *Server) handleGetHotData(w http.ResponseWriter, r *http.Request) {
	hotData, err := s.engine.GetHotData(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"hot_data": hotData,
		"count":    len(hotData),
	})
}

func (s *Server) handleGetRecommendations(w http.ResponseWriter, r *http.Request) {
	recs, err := s.engine.GetRecommendations(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(recs)
}

func (s *Server) handleGetAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database not available", http.StatusServiceUnavailable)
		return
	}

	tenantID := common.GetTenantID(r.Context())
	query := `
		SELECT anomaly_type, severity, description, detected_at
		FROM access_anomalies
		WHERE tenant_id = $1 AND resolved = false
		ORDER BY detected_at DESC
		LIMIT 100
	`

	rows, err := s.db.Query(query, tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()

	var anomalies []map[string]interface{}
	for rows.Next() {
		var aType, severity, description string
		var detectedAt time.Time
		_ = rows.Scan(&aType, &severity, &description, &detectedAt)

		anomalies = append(anomalies, map[string]interface{}{
			"type":        aType,
			"severity":    severity,
			"description": description,
			"detected_at": detectedAt,
		})
	}

	_ = json.NewEncoder(w).Encode(anomalies)
}

func (s *Server) handleTriggerTraining(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "training_started",
		"job_id": fmt.Sprintf("train-%d", time.Now().Unix()),
	})
}

func (s *Server) handleExportTrainingData(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database not available", http.StatusServiceUnavailable)
		return
	}

	tenantID := common.GetTenantID(r.Context())

	query := `
		SELECT tenant_id, container, artifact_key, operation,
		       size_bytes, latency_ms, cache_hit, access_time
		FROM access_patterns
		WHERE tenant_id = $1
		ORDER BY access_time DESC
		LIMIT 10000
	`

	rows, err := s.db.Query(query, tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=access_patterns.csv")

	_, _ = fmt.Fprintln(w, "tenant_id,container,artifact,operation,size,latency,cache_hit,time")

	for rows.Next() {
		var tenantID, container, artifact, operation string
		var size, latency int64
		var cacheHit bool
		var accessTime time.Time

		_ = rows.Scan(&tenantID, &container, &artifact, &operation,
			&size, &latency, &cacheHit, &accessTime)

		_, _ = fmt.Fprintf(w, "%s,%s,%s,%s,%d,%d,%v,%s\n",
			tenantID, container, artifact, operation,
			size, latency, cacheHit, accessTime.Format(time.RFC3339))
	}
}
