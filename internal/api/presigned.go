package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func generateToken() string {
	b := make([]byte, 16)
	_ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleGetPresignedURL(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")

	// Build public URL (no auth needed for 1 hour)
	publicURL := fmt.Sprintf("http://localhost:8000/%s/%s?token=%s&expires=%d",
		bucket, key,
		generateToken(),
		time.Now().Add(time.Hour).Unix())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url":     publicURL,
		"expires": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}
