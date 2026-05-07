package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	metadataMaxKeys     = 50
	metadataMaxValueLen = 500
	metadataMaxTotalLen = 2048
	s3MetaPrefix        = "X-Amz-Meta-"
)

func extractS3Metadata(r *http.Request) map[string]string {
	meta := make(map[string]string)
	for k, vals := range r.Header {
		if strings.HasPrefix(k, s3MetaPrefix) && len(vals) > 0 {
			key := strings.ToLower(k[len(s3MetaPrefix):])
			if key != "" {
				meta[key] = vals[0]
			}
		}
	}
	return meta
}

func setS3MetadataHeaders(w http.ResponseWriter, metadataJSON []byte) {
	if len(metadataJSON) == 0 {
		return
	}
	var meta map[string]string
	if err := json.Unmarshal(metadataJSON, &meta); err != nil {
		return
	}
	for k, v := range meta {
		w.Header().Set(s3MetaPrefix+k, v)
	}
}

func validateMetadata(meta map[string]string) error {
	if len(meta) > metadataMaxKeys {
		return fmt.Errorf("metadata exceeds maximum of %d keys", metadataMaxKeys)
	}
	total := 0
	for k, v := range meta {
		if len(v) > metadataMaxValueLen {
			return fmt.Errorf("metadata value for key %q exceeds %d characters", k, metadataMaxValueLen)
		}
		total += len(k) + len(v)
	}
	if total > metadataMaxTotalLen {
		return fmt.Errorf("metadata total size exceeds %d bytes", metadataMaxTotalLen)
	}
	return nil
}

func mergeMetadata(existing json.RawMessage, patch map[string]interface{}) ([]byte, error) {
	current := make(map[string]string)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &current)
	}
	for k, v := range patch {
		if v == nil {
			delete(current, k)
		} else if s, ok := v.(string); ok {
			current[k] = s
		} else {
			return nil, fmt.Errorf("metadata value for key %q must be a string or null", k)
		}
	}
	if err := validateMetadata(current); err != nil {
		return nil, err
	}
	return json.Marshal(current)
}
