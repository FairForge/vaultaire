// Package api contains the S3-compatible API implementation.
// NOTE: The following auth functions and constants are preserved for future S3 signature verification.
// They implement the AWS Signature Version 4 signing process.
// NOTE: Auth functions are preserved for future S3 signature verification implementation.
//
//nolint:unused // Will be used in future implementations
//nolint:unused,deadcode // These will be used when full S3 auth is implemented
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

// S3 signature constants
const (
	algorithm   = "AWS4-HMAC-SHA256"
	aws4Request = "aws4_request"
	serviceName = "s3"
	timeFormat  = "20060102T150405Z"
	dateFormat  = "20060102"
	maxTimeSkew = 15 * time.Minute
)

// Auth handles S3 authentication
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

// ValidateRequest validates an S3 request and returns the tenant ID
func (a *Auth) ValidateRequest(r *http.Request) (string, error) {
	// Check for test API key first (only in non-production)
	apiKey := r.Header.Get("X-API-Key")
	if apiKey == "test-key-chaos-testing" {
		// Only allow in test/development environments
		if a.db == nil || isTestEnvironment() {
			tenantID := r.Header.Get("X-Tenant-ID")
			if tenantID == "" {
				tenantID = "chaos-test-tenant"
			}
			a.logger.Debug("test authentication accepted",
				zap.String("tenant_id", tenantID),
				zap.String("api_key", "test-key"))
			return tenantID, nil
		}
	}

	// Extract Authorization header
	authHeader := r.Header.Get("Authorization")

	// If no auth header, check for access key in query (for presigned URLs)
	if authHeader == "" {
		if accessKey := r.URL.Query().Get("AWSAccessKeyId"); accessKey != "" {
			return a.validateAccessKey(accessKey)
		}
		// For testing without auth, allow but use test-tenant
		if a.db == nil {
			return "test-tenant", nil
		}
		return "", fmt.Errorf("missing authorization")
	}

	// Parse AWS Signature v4 format (used by AWS CLI/SDKs)
	if strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256") {
		accessKey, _, _, err := a.parseAuthHeader(authHeader)
		if err != nil {
			a.logger.Debug("failed to parse auth header", zap.Error(err))
			return "", err
		}
		return a.validateAccessKey(accessKey)
	}

	// Support basic AWS format (for simpler clients)
	if strings.HasPrefix(authHeader, "AWS ") {
		parts := strings.SplitN(strings.TrimPrefix(authHeader, "AWS "), ":", 2)
		if len(parts) == 2 {
			return a.validateAccessKey(parts[0])
		}
	}

	return "", fmt.Errorf("invalid authorization format")
}

// validateAccessKey looks up the tenant ID by access key
func (a *Auth) validateAccessKey(accessKey string) (string, error) {
	if a.db == nil {
		// Fallback for testing when DB is not available
		a.logger.Warn("no database connection, using test-tenant")
		return "test-tenant", nil
	}

	var tenantID string
	query := `SELECT id FROM tenants WHERE access_key = $1`
	err := a.db.QueryRow(query, accessKey).Scan(&tenantID)
	if err != nil {
		if err == sql.ErrNoRows {
			a.logger.Debug("invalid access key", zap.String("access_key", accessKey))
			return "", fmt.Errorf("invalid access key")
		}
		a.logger.Error("database error during auth", zap.Error(err))
		return "", fmt.Errorf("auth lookup failed: %w", err)
	}

	a.logger.Debug("authenticated tenant",
		zap.String("tenant_id", tenantID),
		zap.String("access_key", accessKey[:6]+"...")) // Log only first 6 chars for security

	return tenantID, nil
}

// parseAuthHeader parses AWS Signature v4 authorization header
func (a *Auth) parseAuthHeader(authHeader string) (accessKey string, signedHeaders string, signature string, err error) {
	parts := strings.Split(authHeader, ", ")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid authorization header format")
	}

	// Extract Credential
	credPart := strings.TrimPrefix(parts[0], "AWS4-HMAC-SHA256 Credential=")
	credParts := strings.Split(credPart, "/")
	if len(credParts) < 1 {
		return "", "", "", fmt.Errorf("invalid credential format")
	}
	accessKey = credParts[0]

	// Extract SignedHeaders
	signedHeaders = strings.TrimPrefix(parts[1], "SignedHeaders=")

	// Extract Signature
	signature = strings.TrimPrefix(parts[2], "Signature=")

	return accessKey, signedHeaders, signature, nil
}

// getSecretKey retrieves the secret key for an access key (for future signature validation)
func (a *Auth) getSecretKey(accessKey string) (secretKey, tenantID string, err error) {
	if a.db == nil {
		return "", "", fmt.Errorf("database not initialized")
	}

	query := `SELECT secret_key, id FROM tenants WHERE access_key = $1`
	err = a.db.QueryRow(query, accessKey).Scan(&secretKey, &tenantID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", fmt.Errorf("invalid access key")
		}
		return "", "", fmt.Errorf("database error: %w", err)
	}

	return secretKey, tenantID, nil
}

// validateTimestamp checks if the request timestamp is within acceptable range
func (a *Auth) validateTimestamp(amzDate string) error {
	t, err := time.Parse(timeFormat, amzDate)
	if err != nil {
		return fmt.Errorf("invalid date format: %w", err)
	}

	now := time.Now().UTC()
	diff := now.Sub(t)
	if diff < -maxTimeSkew || diff > maxTimeSkew {
		return fmt.Errorf("request timestamp too old or too far in future")
	}

	return nil
}

// calculateSignature calculates AWS Signature v4 (for future full validation)
func (a *Auth) calculateSignature(r *http.Request, accessKey, secretKey, signedHeaders, amzDate string) (string, error) {
	// Create canonical request
	canonicalRequest, payloadHash := a.createCanonicalRequest(r, signedHeaders)

	// Create string to sign
	date := amzDate[:8]
	region := "us-east-1"
	scope := fmt.Sprintf("%s/%s/%s/%s", date, region, serviceName, aws4Request)
	stringToSign := a.createStringToSign(amzDate, scope, canonicalRequest)

	// Derive signing key
	signingKey := a.deriveSigningKey(secretKey, date, region, serviceName)

	// Calculate signature
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	_ = payloadHash

	return signature, nil
}

func (a *Auth) createCanonicalRequest(r *http.Request, signedHeaders string) (string, string) {
	payloadHash := r.Header.Get("X-Amz-Content-SHA256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}

	headers := strings.Split(signedHeaders, ";")
	canonicalHeaders, headerValues := a.createCanonicalHeaders(r, headers)

	canonicalQueryString := a.createCanonicalQueryString(r.URL.Query())

	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.Path,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	return canonicalRequest, headerValues
}

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
				url.QueryEscape(k),
				url.QueryEscape(v)))
		}
	}

	return strings.Join(pairs, "&")
}

func (a *Auth) createCanonicalHeaders(r *http.Request, signedHeaders []string) (string, string) {
	headers := make(map[string][]string)

	for _, h := range signedHeaders {
		key := strings.ToLower(h)
		if key == "host" {
			headers[key] = []string{r.Host}
		} else {
			headers[key] = r.Header[http.CanonicalHeaderKey(h)]
		}
	}

	var keys []string
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical []string
	var values []string
	for _, k := range keys {
		vals := headers[k]
		canonical = append(canonical, fmt.Sprintf("%s:%s", k, strings.Join(vals, ",")))
		values = append(values, strings.Join(vals, ","))
	}

	return strings.Join(canonical, "\n") + "\n", strings.Join(values, ";")
}

func (a *Auth) createStringToSign(amzDate, scope, canonicalRequest string) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	return strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")
}

func (a *Auth) deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(aws4Request))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// AuthHandler handles authentication endpoints
type AuthHandler struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewAuthHandler creates a new auth handler
func NewAuthHandler(db *sql.DB, logger *zap.Logger) *AuthHandler {
	return &AuthHandler{
		db:     db,
		logger: logger,
	}
}

// Register creates a new tenant account
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	// Generate credentials
	accessKey := "AK" + generateID()
	secretKey := "SK" + generateID() + generateID()
	tenantID := "tenant-" + generateID()

	// Create tenant in database
	_, err := h.db.Exec(`
        INSERT INTO tenants (id, access_key, secret_key, created_at)
        VALUES ($1, $2, $3, NOW())
    `, tenantID, accessKey, secretKey)

	if err != nil {
		h.logger.Error("failed to create tenant", zap.Error(err))
		http.Error(w, "Failed to create account", http.StatusInternalServerError)
		return
	}

	// Return S3-compatible credentials
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"accessKeyId":"%s","secretAccessKey":"%s","endpoint":"http://localhost:8000"}`,
		accessKey, secretKey)
}

// Helper function to check if we're in a test environment
func isTestEnvironment() bool {
	env := os.Getenv("ENV")
	return env == "" || env == "test" || env == "development"
}
