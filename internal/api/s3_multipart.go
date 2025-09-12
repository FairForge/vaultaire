package api

import (
	"fmt"
	"net/http"
	"time"
)

func (s *Server) handleInitiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	uploadID := fmt.Sprintf("upload-%d", time.Now().Unix())

	response := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult>
    <Bucket>%s</Bucket>
    <Key>%s</Key>
    <UploadId>%s</UploadId>
</InitiateMultipartUploadResult>`, bucket, object, uploadID)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(response))
}

func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// For MVP, just return success
	response := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult>
    <Location>http://localhost:8000/%s/%s</Location>
    <Bucket>%s</Bucket>
    <Key>%s</Key>
    <ETag>"mock-etag-12345"</ETag>
</CompleteMultipartUploadResult>`, bucket, object, bucket, object)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(response))
}
