package api

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const (
	maxTaggingBodyBytes = 10240 // 10 KB
	maxObjectTags       = 10
	maxTagKeyLen        = 128
	maxTagValueLen      = 256
)

// Tagging is the S3 XML document for object tagging (GET/PUT ?tagging).
type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	TagSet  TagSet   `xml:"TagSet"`
}

// TagSet wraps the list of tags.
type TagSet struct {
	Tags []Tag `xml:"Tag"`
}

// Tag is a single key/value tag.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// tagCount returns the number of tags in a JSONB tag map. Returns 0 for empty
// or malformed input. Used to emit x-amz-tagging-count on HEAD and GET.
func tagCount(tagsJSON []byte) int {
	if len(tagsJSON) == 0 {
		return 0
	}
	tagMap := map[string]string{}
	if err := json.Unmarshal(tagsJSON, &tagMap); err != nil {
		return 0
	}
	return len(tagMap)
}

// handleGetObjectTagging returns the tag set for an object as XML.
func (s *Server) handleGetObjectTagging(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var tagsJSON []byte
	err = s.db.QueryRowContext(r.Context(), `
		SELECT COALESCE(tags, '{}')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		t.ID, req.Bucket, req.Object).Scan(&tagsJSON)
	if err == sql.ErrNoRows {
		reqID := generateRequestID()
		if suggestion := keySuggestion(r.Context(), s.db, t.ID, req.Bucket, req.Object); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
		}
		return
	}
	if err != nil {
		s.logger.Error("query object tags", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	tagMap := map[string]string{}
	if len(tagsJSON) > 0 {
		_ = json.Unmarshal(tagsJSON, &tagMap)
	}

	resp := Tagging{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for k, v := range tagMap {
		resp.TagSet.Tags = append(resp.TagSet.Tags, Tag{Key: k, Value: v})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

// handlePutObjectTagging replaces the entire tag set for an object. It is not a
// merge — the provided tag set fully replaces any existing tags.
func (s *Server) handlePutObjectTagging(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxTaggingBodyBytes))
	if err != nil {
		WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
		return
	}

	var doc Tagging
	if err := xml.Unmarshal(body, &doc); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	// Validate and build the tag map.
	if len(doc.TagSet.Tags) > maxObjectTags {
		WriteS3ErrorWithContext(w, ErrInvalidTag, r.URL.Path, generateRequestID(),
			WithSuggestion("An object cannot have more than 10 tags."))
		return
	}
	tagMap := make(map[string]string, len(doc.TagSet.Tags))
	for _, tag := range doc.TagSet.Tags {
		if tag.Key == "" {
			WriteS3ErrorWithContext(w, ErrInvalidTag, r.URL.Path, generateRequestID(),
				WithSuggestion("Tag keys must not be empty."))
			return
		}
		if len(tag.Key) > maxTagKeyLen {
			WriteS3ErrorWithContext(w, ErrInvalidTag, r.URL.Path, generateRequestID(),
				WithSuggestion("Tag keys may be at most 128 characters."))
			return
		}
		if len(tag.Value) > maxTagValueLen {
			WriteS3ErrorWithContext(w, ErrInvalidTag, r.URL.Path, generateRequestID(),
				WithSuggestion("Tag values may be at most 256 characters."))
			return
		}
		if _, dup := tagMap[tag.Key]; dup {
			WriteS3ErrorWithContext(w, ErrInvalidTag, r.URL.Path, generateRequestID(),
				WithSuggestion("Duplicate tag keys are not allowed."))
			return
		}
		tagMap[tag.Key] = tag.Value
	}

	tagsJSON, err := json.Marshal(tagMap)
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE object_head_cache SET tags = $4
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		t.ID, req.Bucket, req.Object, tagsJSON)
	if err != nil {
		s.logger.Error("update object tags", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		reqID := generateRequestID()
		if suggestion := keySuggestion(r.Context(), s.db, t.ID, req.Bucket, req.Object); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
		}
		return
	}

	w.Header().Set("x-amz-version-id", "null")
	w.WriteHeader(http.StatusOK)
}

// handleDeleteObjectTagging removes all tags from an object (resets to '{}').
func (s *Server) handleDeleteObjectTagging(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	_, err = s.db.ExecContext(r.Context(), `
		UPDATE object_head_cache SET tags = '{}'
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		t.ID, req.Bucket, req.Object)
	if err != nil {
		s.logger.Error("delete object tags", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
