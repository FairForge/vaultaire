package api

import (
	"fmt"
	"net/http"

	"github.com/FairForge/vaultaire/internal/modes"
)

const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuthentication = "authentication_error"
	ErrTypePermission     = "permission_error"
	ErrTypeNotFound       = "not_found_error"
	ErrTypeConflict       = "conflict_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeAPI            = "api_error"
	// ErrTypeGone: the resource existed and is gone for good (an expired
	// export, WP-R10-3b).
	ErrTypeGone = "gone_error"
	// ErrTypeUnavailable: the request is valid but cannot be served now; it
	// carries Retry-After (a region whose storage is down, Prompt 10 Part A).
	ErrTypeUnavailable = "unavailable_error"
)

var managementErrorStatus = map[string]int{
	ErrTypeInvalidRequest: http.StatusBadRequest,
	ErrTypeAuthentication: http.StatusUnauthorized,
	ErrTypePermission:     http.StatusForbidden,
	ErrTypeNotFound:       http.StatusNotFound,
	ErrTypeConflict:       http.StatusConflict,
	ErrTypeRateLimit:      http.StatusTooManyRequests,
	ErrTypeAPI:            http.StatusInternalServerError,
	ErrTypeGone:           http.StatusGone,
	ErrTypeUnavailable:    http.StatusServiceUnavailable,
}

type managementError struct {
	Error managementErrorBody `json:"error"`
}

type managementErrorBody struct {
	Type      string `json:"type"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Param     string `json:"param,omitempty"`
	RequestID string `json:"request_id"`
	// Readable is what a refused Private change counted (409
	// bucket_has_readable_objects, RP33-1).
	Readable *mgmtReadableCounts `json:"readable,omitempty"`
}

// mgmtReadableCounts is what a bucket holds that we can read.
type mgmtReadableCounts struct {
	Objects  int64 `json:"readable_objects"`
	Versions int64 `json:"readable_versions"`
	Uploads  int64 `json:"uploads_without_key"`
}

// writeReadableObjectsError is the 409 of a Private change refused over
// readable bytes (RP33-1): the counts, and what to do instead.
func writeReadableObjectsError(w http.ResponseWriter, e *modes.ReadableObjectsError) {
	writeJSON(w, http.StatusConflict, managementError{Error: managementErrorBody{
		Type: ErrTypeConflict, Code: modes.ErrReadableObjects.Error(),
		Message: fmt.Sprintf("the bucket holds %d object(s), %d version(s) and %d upload(s) in progress that are not encrypted with your key, "+
			"so it cannot become private: create a new bucket with \"mode\": \"private\" and copy the data into it with your key", e.Objects, e.Versions, e.Uploads),
		Param:     "ssec_required",
		RequestID: getRequestID(w),
		Readable:  &mgmtReadableCounts{Objects: e.Objects, Versions: e.Versions, Uploads: e.Uploads},
	}})
}

func writeManagementError(w http.ResponseWriter, errType, code, message, param string) {
	status := managementErrorStatus[errType]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	resp := managementError{
		Error: managementErrorBody{
			Type:      errType,
			Code:      code,
			Message:   message,
			Param:     param,
			RequestID: getRequestID(w),
		},
	}
	writeJSON(w, status, resp)
}
