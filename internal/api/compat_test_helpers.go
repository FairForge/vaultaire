package api

import (
	"database/sql"
	"net/http"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

func NewTestServer(eng *engine.CoreEngine, db *sql.DB, logger *zap.Logger) *Server {
	return &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: true,
	}
}

func (s *Server) HandleHeadObject(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.handleHeadObject(w, r, &S3Request{Bucket: bucket, Object: object})
}

func (s *Server) HandleCopyObject(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.handleCopyObject(w, r, &S3Request{Bucket: bucket, Object: object})
}

func (s *Server) HandleInitiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.handleInitiateMultipartUpload(w, r, bucket, object)
}

func (s *Server) HandleUploadPart(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.handleUploadPart(w, r, bucket, object)
}

func (s *Server) HandleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.handleCompleteMultipartUpload(w, r, bucket, object)
}
