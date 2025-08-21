package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"time"
)

// S3Error represents an S3 error response
type S3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

// S3 Error codes
const (
	ErrNoSuchBucket          = "NoSuchBucket"
	ErrNoSuchKey             = "NoSuchKey"
	ErrBucketAlreadyExists   = "BucketAlreadyExists"
	ErrBucketNotEmpty        = "BucketNotEmpty"
	ErrInvalidBucketName     = "InvalidBucketName"
	ErrInvalidObjectName     = "InvalidObjectName"
	ErrAccessDenied          = "AccessDenied"
	ErrInvalidRequest        = "InvalidRequest"
	ErrIncompleteBody        = "IncompleteBody"
	ErrInternalError         = "InternalError"
	ErrNotImplemented        = "NotImplemented"
	ErrMissingContentLength  = "MissingContentLength"
	ErrRequestTimeout        = "RequestTimeout"
	ErrBadDigest             = "BadDigest"
	ErrEntityTooLarge        = "EntityTooLarge"
	ErrMalformedXML          = "MalformedXML"
	ErrMethodNotAllowed      = "MethodNotAllowed"
	ErrSignatureDoesNotMatch = "SignatureDoesNotMatch"
)

// Error messages
var errorMessages = map[string]string{
	ErrNoSuchBucket:          "The specified bucket does not exist",
	ErrNoSuchKey:             "The specified key does not exist",
	ErrBucketAlreadyExists:   "The requested bucket name is not available",
	ErrBucketNotEmpty:        "The bucket you tried to delete is not empty",
	ErrInvalidBucketName:     "The specified bucket is not valid",
	ErrInvalidObjectName:     "The specified object name is not valid",
	ErrAccessDenied:          "Access denied",
	ErrInvalidRequest:        "Invalid request",
	ErrIncompleteBody:        "You did not provide the number of bytes specified by the Content-Length HTTP header",
	ErrInternalError:         "We encountered an internal error. Please try again",
	ErrNotImplemented:        "A feature you requested is not yet implemented",
	ErrMissingContentLength:  "You must provide the Content-Length HTTP header",
	ErrRequestTimeout:        "Your socket connection to the server was not read from or written to within the timeout period",
	ErrBadDigest:             "The Content-MD5 you specified did not match what we received",
	ErrEntityTooLarge:        "Your proposed upload exceeds the maximum allowed size",
	ErrMalformedXML:          "The XML you provided was not well-formed or did not validate against our published schema",
	ErrMethodNotAllowed:      "The specified method is not allowed against this resource",
	ErrSignatureDoesNotMatch: "The request signature we calculated does not match the signature you provided",
}

// HTTP status codes for errors
var errorStatusCodes = map[string]int{
	ErrNoSuchBucket:          http.StatusNotFound,
	ErrNoSuchKey:             http.StatusNotFound,
	ErrBucketAlreadyExists:   http.StatusConflict,
	ErrBucketNotEmpty:        http.StatusConflict,
	ErrInvalidBucketName:     http.StatusBadRequest,
	ErrInvalidObjectName:     http.StatusBadRequest,
	ErrAccessDenied:          http.StatusForbidden,
	ErrInvalidRequest:        http.StatusBadRequest,
	ErrIncompleteBody:        http.StatusBadRequest,
	ErrInternalError:         http.StatusInternalServerError,
	ErrNotImplemented:        http.StatusNotImplemented,
	ErrMissingContentLength:  http.StatusLengthRequired,
	ErrRequestTimeout:        http.StatusRequestTimeout,
	ErrBadDigest:             http.StatusBadRequest,
	ErrEntityTooLarge:        http.StatusRequestEntityTooLarge,
	ErrMalformedXML:          http.StatusBadRequest,
	ErrMethodNotAllowed:      http.StatusMethodNotAllowed,
	ErrSignatureDoesNotMatch: http.StatusForbidden,
}

// WriteS3Error writes an S3-compatible error response
func WriteS3Error(w http.ResponseWriter, code string, resource string, requestID string) {
	message, exists := errorMessages[code]
	if !exists {
		message = "Unknown error"
		code = ErrInternalError
	}

	statusCode, exists := errorStatusCodes[code]
	if !exists {
		statusCode = http.StatusInternalServerError
	}

	errResp := S3Error{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestID: requestID,
	}

	xmlData, err := xml.MarshalIndent(errResp, "", "  ")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)
}

// generateRequestID creates a unique request ID
func generateRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().Unix(), time.Now().Nanosecond())
}
