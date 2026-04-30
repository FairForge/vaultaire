package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handleGetPresignedURL(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	method := r.URL.Query().Get("method")
	expiresStr := r.URL.Query().Get("expires")

	if bucket == "" || key == "" {
		http.Error(w, `{"error":"bucket and key are required"}`, http.StatusBadRequest)
		return
	}

	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "PUT" {
		http.Error(w, `{"error":"method must be GET or PUT"}`, http.StatusBadRequest)
		return
	}

	expiresSec := 3600
	if expiresStr != "" {
		var err error
		expiresSec, err = strconv.Atoi(expiresStr)
		if err != nil || expiresSec < 1 || expiresSec > presignMaxExpires {
			http.Error(w, `{"error":"expires must be between 1 and 604800 seconds"}`, http.StatusBadRequest)
			return
		}
	}

	tenantID, ok := r.Context().Value(tenantIDKey).(string)
	if !ok || tenantID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	if s.db == nil {
		http.Error(w, `{"error":"database not available"}`, http.StatusInternalServerError)
		return
	}

	var accessKey, secretKey string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT access_key, secret_key FROM tenants WHERE id = $1`, tenantID,
	).Scan(&accessKey, &secretKey)
	if err != nil {
		http.Error(w, `{"error":"tenant credentials not found"}`, http.StatusNotFound)
		return
	}

	baseURL := s.getBaseURL()

	presignedURL, expiresAt := generatePresignedS3URL(baseURL, accessKey, secretKey, bucket, key, method, expiresSec)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url":        presignedURL,
		"expires_at": expiresAt.Format(time.RFC3339),
		"method":     method,
	})
}

func (s *Server) getBaseURL() string {
	port := 8000
	if s.config != nil {
		port = s.config.Server.Port
	}
	return getPublicEndpoint(port)
}
