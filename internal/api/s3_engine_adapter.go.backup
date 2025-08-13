package api

import (
	"context"
	"net/http"

	"github.com/FairForge/vaultaire/internal/engine"
)

// S3ToEngine adapts S3 requests to engine operations
type S3ToEngine struct {
	engine engine.Engine
}

// NewS3ToEngine creates a new adapter
func NewS3ToEngine(e engine.Engine) *S3ToEngine {
	return &S3ToEngine{engine: e}
}

// TranslateRequest converts S3 terminology to engine terminology
func (a *S3ToEngine) TranslateRequest(req *S3Request) engine.Operation {
	return engine.Operation{
		Type:      req.Operation,
		Container: req.Bucket, // Bucket → Container
		Artifact:  req.Object, // Object → Artifact
		Context:   context.Background(),
		Metadata:  make(map[string]interface{}),
	}
}

// HandleGet processes S3 GET requests using the engine
func (a *S3ToEngine) HandleGet(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// Internally use Container/Artifact
	container := bucket
	artifact := object

	data, err := a.engine.Get(r.Context(), container, artifact)
	if err != nil {
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}
	defer data.Close()

	// Stream to client
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	// Copy data to response
}
