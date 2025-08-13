// internal/api/auth.go
package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	// AWS Signature V4 constants
	algorithm   = "AWS4-HMAC-SHA256"
	aws4Request = "aws4_request"
	serviceName = "s3"
	timeFormat  = "20060102T150405Z"
	dateFormat  = "20060102"
	maxTimeSkew = 15 * time.Minute // AWS allows 15 minutes of clock skew
)

// Auth handles S3 signature validation
type Auth struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewAuth creates a new Auth handler
func NewAuth(db *sql.DB, logger *zap.Logger) *Auth {
	return &Auth{
		db:     db,
		logger: logger,
	}
}

// ValidateRequest validates an S3 request signature
func (a *Auth) ValidateRequest(r *http.Request) (string, error) {
	// Allow anonymous requests for testing (if no Authorization header)
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		a.logger.Debug("allowing anonymous request (no auth header)")
		return "", nil // anonymous access
	}

	// Parse the authorization header
	if !strings.HasPrefix(authHeader, algorithm) {
		return "", fmt.Errorf("invalid authorization header format")
	}

	// Extract credential info from auth header
	accessKey, signedHeaders, signature, err := a.parseAuthHeader(authHeader)
	if err != nil {
		return "", fmt.Errorf("parse auth header: %w", err)
	}

	// Get the secret key from database
	secretKey, tenantID, err := a.getSecretKey(accessKey)
	if err != nil {
		return "", fmt.Errorf("invalid access key: %w", err)
	}

	// Validate timestamp (prevent replay attacks)
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.Header.Get("Date")
	}
	if err := a.validateTimestamp(amzDate); err != nil {
		return "", fmt.Errorf("timestamp validation failed: %w", err)
	}

	// Calculate the expected signature
	expectedSig, err := a.calculateSignature(r, accessKey, secretKey, signedHeaders, amzDate)
	if err != nil {
		return "", fmt.Errorf("calculate signature: %w", err)
	}

	// Compare signatures
	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		a.logger.Debug("signature mismatch",
			zap.String("expected", expectedSig),
			zap.String("provided", signature))
		return "", fmt.Errorf("signature mismatch")
	}

	a.logger.Debug("request authenticated",
		zap.String("access_key", accessKey),
		zap.String("tenant_id", tenantID))

	return tenantID, nil
}

// parseAuthHeader extracts components from the Authorization header
func (a *Auth) parseAuthHeader(authHeader string) (accessKey, signedHeaders, signature string, err error) {
	// Format: AWS4-HMAC-SHA256 Credential=ACCESS/20231201/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=...

	parts := strings.Split(authHeader, ", ")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid authorization header format")
	}

	// Extract Credential
	credentialPart := strings.TrimPrefix(parts[0], algorithm+" ")
	if !strings.HasPrefix(credentialPart, "Credential=") {
		return "", "", "", fmt.Errorf("missing Credential in auth header")
	}
	credential := strings.TrimPrefix(credentialPart, "Credential=")
	credParts := strings.Split(credential, "/")
	if len(credParts) < 1 {
		return "", "", "", fmt.Errorf("invalid credential format")
	}
	accessKey = credParts[0]

	// Extract SignedHeaders
	signedHeadersPart := parts[1]
	if strings.HasPrefix(signedHeadersPart, "SignedHeaders=") {
		signedHeaders = strings.TrimPrefix(strings.Split(signedHeadersPart, ", ")[0], "SignedHeaders=")
		// Extract Signature
		for _, part := range strings.Split(parts[1], ", ") {
			if strings.HasPrefix(part, "Signature=") {
				signature = strings.TrimPrefix(part, "Signature=")
				break
			}
		}
	} else {
		// Alternative format where SignedHeaders and Signature are separate
		return "", "", "", fmt.Errorf("invalid auth header format")
	}

	if signature == "" {
		return "", "", "", fmt.Errorf("missing Signature in auth header")
	}

	return accessKey, signedHeaders, signature, nil
}

// getSecretKey retrieves the secret key from the database
func (a *Auth) getSecretKey(accessKey string) (secretKey, tenantID string, err error) {
	query := `
		SELECT secret_key, id 
		FROM tenants 
		WHERE access_key = $1 AND active = true
		LIMIT 1
	`

	err = a.db.QueryRow(query, accessKey).Scan(&secretKey, &tenantID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", fmt.Errorf("access key not found")
		}
		return "", "", fmt.Errorf("database error: %w", err)
	}

	return secretKey, tenantID, nil
}

// validateTimestamp checks if the request timestamp is within acceptable range
func (a *Auth) validateTimestamp(amzDate string) error {
	if amzDate == "" {
		return fmt.Errorf("missing X-Amz-Date header")
	}

	requestTime, err := time.Parse(timeFormat, amzDate)
	if err != nil {
		return fmt.Errorf("invalid date format: %w", err)
	}

	now := time.Now().UTC()
	diff := now.Sub(requestTime)
	if diff < -maxTimeSkew || diff > maxTimeSkew {
		return fmt.Errorf("request timestamp too old or too far in future: %v", diff)
	}

	return nil
}

// calculateSignature computes the AWS Signature V4
func (a *Auth) calculateSignature(r *http.Request, accessKey, secretKey, signedHeaders, amzDate string) (string, error) {
	// Step 1: Create canonical request
	canonicalRequest, _ := a.createCanonicalRequest(r, signedHeaders)

	// Step 2: Create string to sign
	date := amzDate[:8]
	scope := fmt.Sprintf("%s/us-east-1/%s/%s", date, serviceName, aws4Request)
	stringToSign := a.createStringToSign(amzDate, scope, canonicalRequest)

	// Step 3: Calculate signing key
	signingKey := a.deriveSigningKey(secretKey, date, "us-east-1", serviceName)

	// Step 4: Calculate signature
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	return signature, nil
}

// createCanonicalRequest builds the canonical request string
func (a *Auth) createCanonicalRequest(r *http.Request, signedHeaders string) (string, string) {
	// Get the payload hash
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		// Calculate payload hash if not provided
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body)) // Reset body
		hash := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(hash[:])
	}

	// Canonical URI (URL-encoded path)
	canonicalURI := r.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	// Canonical query string
	canonicalQuery := a.createCanonicalQueryString(r.URL.Query())

	// Canonical headers
	canonicalHeaders, signedHeadersList := a.createCanonicalHeaders(r, strings.Split(signedHeaders, ";"))

	// Build canonical request
	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeadersList,
		payloadHash,
	}, "\n")

	return canonicalRequest, payloadHash
}

// createCanonicalQueryString creates the canonical query string
func (a *Auth) createCanonicalQueryString(values url.Values) string {
	var keys []string
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		for _, v := range values[k] {
			pairs = append(pairs, fmt.Sprintf("%s=%s",
				url.QueryEscape(k), url.QueryEscape(v)))
		}
	}

	return strings.Join(pairs, "&")
}

// createCanonicalHeaders creates the canonical headers string
func (a *Auth) createCanonicalHeaders(r *http.Request, signedHeaders []string) (string, string) {
	headers := make(map[string]string)

	for _, h := range signedHeaders {
		key := strings.ToLower(strings.TrimSpace(h))
		if key == "host" {
			headers[key] = r.Host
		} else {
			headers[key] = strings.TrimSpace(r.Header.Get(h))
		}
	}

	// Sort header names
	var headerNames []string
	for k := range headers {
		headerNames = append(headerNames, k)
	}
	sort.Strings(headerNames)

	// Build canonical headers
	var canonicalHeaders []string
	for _, k := range headerNames {
		canonicalHeaders = append(canonicalHeaders, fmt.Sprintf("%s:%s", k, headers[k]))
	}

	return strings.Join(canonicalHeaders, "\n") + "\n", strings.Join(headerNames, ";")
}

// createStringToSign creates the string to sign
func (a *Auth) createStringToSign(amzDate, scope, canonicalRequest string) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	return strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")
}

// deriveSigningKey derives the signing key using the secret key
func (a *Auth) deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte(aws4Request))
	return kSigning
}

// hmacSHA256 computes HMAC-SHA256
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// ValidatePresignedURL validates a presigned URL request (simplified for MVP)
func (a *Auth) ValidatePresignedURL(r *http.Request) (string, error) {
	// TODO: Implement presigned URL validation in a future step
	// For now, just check for X-Amz-Signature in query params
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		a.logger.Debug("presigned URLs not yet implemented")
		return "", fmt.Errorf("presigned URLs not yet implemented")
	}
	return "", nil
}
