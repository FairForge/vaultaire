package api

import "net/http"

const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuthentication = "authentication_error"
	ErrTypePermission     = "permission_error"
	ErrTypeNotFound       = "not_found_error"
	ErrTypeConflict       = "conflict_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeAPI            = "api_error"
)

var managementErrorStatus = map[string]int{
	ErrTypeInvalidRequest: http.StatusBadRequest,
	ErrTypeAuthentication: http.StatusUnauthorized,
	ErrTypePermission:     http.StatusForbidden,
	ErrTypeNotFound:       http.StatusNotFound,
	ErrTypeConflict:       http.StatusConflict,
	ErrTypeRateLimit:      http.StatusTooManyRequests,
	ErrTypeAPI:            http.StatusInternalServerError,
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
