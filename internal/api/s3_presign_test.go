package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	testAccessKey       = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey       = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testPresignTenantID = "tenant-presign-test"
	testRegion          = "us-east-1"
)

func signPresignedURL(method, path, host, accessKey, secretKey string, expiresSec int, amzDate time.Time) url.Values {
	date := amzDate.Format(presignDateFormat)
	amzDateStr := amzDate.Format(presignTimeFormat)
	credentialScope := fmt.Sprintf("%s/%s/%s/%s", date, testRegion, presignService, presignAWS4Request)
	credential := fmt.Sprintf("%s/%s", accessKey, credentialScope)

	q := url.Values{}
	q.Set("X-Amz-Algorithm", presignAlgorithm)
	q.Set("X-Amz-Credential", credential)
	q.Set("X-Amz-Date", amzDateStr)
	q.Set("X-Amz-Expires", strconv.Itoa(expiresSec))
	q.Set("X-Amz-SignedHeaders", "host")

	canonicalQueryString := buildTestCanonicalQuery(q)
	canonicalURI := uriEncodePath(path)

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryString,
		"host:" + host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		presignAlgorithm,
		amzDateStr,
		credentialScope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := testDeriveKey(secretKey, date, testRegion)
	signature := hex.EncodeToString(testHMACSign(signingKey, []byte(stringToSign)))

	q.Set("X-Amz-Signature", signature)
	return q
}

func buildTestCanonicalQuery(values url.Values) string {
	var keys []string
	for k := range values {
		if k == "X-Amz-Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		vs := make([]string, len(values[k]))
		copy(vs, values[k])
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(pairs, "&")
}

func testDeriveKey(secretKey, date, region string) []byte {
	kDate := testHMACSign([]byte("AWS4"+secretKey), []byte(date))
	kRegion := testHMACSign(kDate, []byte(region))
	kService := testHMACSign(kRegion, []byte(presignService))
	return testHMACSign(kService, []byte(presignAWS4Request))
}

func testHMACSign(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func newMockDB(t *testing.T) (*Server, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	s := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		db:       db,
		testMode: false,
		config:   &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	cleanup := func() { _ = db.Close() }
	return s, mock, cleanup
}

func expectTenantLookup(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT secret_key, id FROM tenants WHERE access_key`).
		WithArgs(testAccessKey).
		WillReturnRows(sqlmock.NewRows([]string{"secret_key", "id"}).
			AddRow(testSecretKey, testPresignTenantID))
}

func TestIsPresignedRequest(t *testing.T) {
	t.Run("with algorithm param", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256", nil)
		assert.True(t, isPresignedRequest(r))
	})

	t.Run("without algorithm param", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/bucket/key", nil)
		assert.False(t, isPresignedRequest(r))
	})

	t.Run("wrong algorithm", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/bucket/key?X-Amz-Algorithm=WRONG", nil)
		assert.False(t, isPresignedRequest(r))
	})
}

func TestVerifyPresignedURL_MissingParams(t *testing.T) {
	s, _, cleanup := newMockDB(t)
	defer cleanup()

	r := httptest.NewRequest("GET", "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256", nil)
	r.Host = "localhost:8000"

	_, err := s.verifyPresignedURL(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ErrAuthorizationQueryParametersError)
}

func TestVerifyPresignedURL_ExpiresOutOfRange(t *testing.T) {
	s, _, cleanup := newMockDB(t)
	defer cleanup()

	tests := []struct {
		name    string
		expires string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too large", "999999"},
		{"non-numeric", "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{}
			q.Set("X-Amz-Algorithm", presignAlgorithm)
			q.Set("X-Amz-Credential", "key/20260429/us-east-1/s3/aws4_request")
			q.Set("X-Amz-Date", "20260429T120000Z")
			q.Set("X-Amz-Expires", tt.expires)
			q.Set("X-Amz-SignedHeaders", "host")
			q.Set("X-Amz-Signature", "deadbeef")

			r := httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
			r.Host = "localhost:8000"

			_, err := s.verifyPresignedURL(r)
			require.Error(t, err)
			assert.Contains(t, err.Error(), ErrInvalidPresignExpires)
		})
	}
}

func TestVerifyPresignedURL_InvalidAccessKey(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT secret_key, id FROM tenants WHERE access_key`).
		WithArgs("NONEXISTENT000000000").
		WillReturnRows(sqlmock.NewRows([]string{"secret_key", "id"}))

	now := time.Now().UTC()
	q := signPresignedURL("GET", "/bucket/key", "localhost:8000", "NONEXISTENT000000000", testSecretKey, 3600, now)

	r := httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
	r.Host = "localhost:8000"

	_, err := s.verifyPresignedURL(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ErrAccessDenied)
}

func TestVerifyPresignedURL_Expired(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()

	expectTenantLookup(mock)

	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	q := signPresignedURL("GET", "/bucket/key", "localhost:8000", testAccessKey, testSecretKey, 1, pastTime)

	r := httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
	r.Host = "localhost:8000"

	_, err := s.verifyPresignedURL(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ErrExpiredPresignedRequest)
}

func TestVerifyPresignedURL_InvalidSignature(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()

	expectTenantLookup(mock)

	now := time.Now().UTC()
	q := signPresignedURL("GET", "/bucket/key", "localhost:8000", testAccessKey, testSecretKey, 3600, now)
	q.Set("X-Amz-Signature", "0000000000000000000000000000000000000000000000000000000000000000")

	r := httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
	r.Host = "localhost:8000"

	_, err := s.verifyPresignedURL(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ErrSignatureDoesNotMatch)
}

func TestVerifyPresignedURL_ValidGET(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()

	expectTenantLookup(mock)

	now := time.Now().UTC()
	q := signPresignedURL("GET", "/my-bucket/my-object.txt", "localhost:8000", testAccessKey, testSecretKey, 3600, now)

	r := httptest.NewRequest("GET", "/my-bucket/my-object.txt?"+q.Encode(), nil)
	r.Host = "localhost:8000"

	tenantID, err := s.verifyPresignedURL(r)
	require.NoError(t, err)
	assert.Equal(t, testPresignTenantID, tenantID)
}

func TestVerifyPresignedURL_ValidPUT(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()

	expectTenantLookup(mock)

	now := time.Now().UTC()
	q := signPresignedURL("PUT", "/upload-bucket/data.bin", "localhost:8000", testAccessKey, testSecretKey, 3600, now)

	r := httptest.NewRequest("PUT", "/upload-bucket/data.bin?"+q.Encode(), bytes.NewReader([]byte("file content")))
	r.Host = "localhost:8000"

	tenantID, err := s.verifyPresignedURL(r)
	require.NoError(t, err)
	assert.Equal(t, testPresignTenantID, tenantID)
}

func TestVerifyPresignedURL_PathWithSpecialChars(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()

	expectTenantLookup(mock)

	now := time.Now().UTC()
	// Sign with the decoded path since verifier uses r.URL.Path (decoded by Go)
	path := "/my-bucket/my-file_v2.0~final.txt"
	q := signPresignedURL("GET", path, "localhost:8000", testAccessKey, testSecretKey, 3600, now)

	r := httptest.NewRequest("GET", path+"?"+q.Encode(), nil)
	r.Host = "localhost:8000"

	tenantID, err := s.verifyPresignedURL(r)
	require.NoError(t, err)
	assert.Equal(t, testPresignTenantID, tenantID)
}

func TestPresignedURL_Integration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tempDir, err := os.MkdirTemp("", "presign-integration-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	logger, _ := zap.NewDevelopment()
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	s := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: false,
		config:   &config.Config{Server: config.ServerConfig{Port: 8000}},
	}
	s.router.HandleFunc("/*", s.handleS3Request)

	bucket := "integration-test"
	bucketPath := filepath.Join(tempDir, testPresignTenantID+"_"+bucket)
	require.NoError(t, os.MkdirAll(bucketPath, 0755))

	testContent := "hello from presigned upload"
	objectKey := "test-upload.txt"

	// PUT
	mock.ExpectQuery(`SELECT secret_key, id FROM tenants WHERE access_key`).
		WithArgs(testAccessKey).
		WillReturnRows(sqlmock.NewRows([]string{"secret_key", "id"}).
			AddRow(testSecretKey, testPresignTenantID))
	mock.ExpectQuery(`SELECT suspended_at FROM tenants WHERE id`).
		WithArgs(testPresignTenantID).
		WillReturnRows(sqlmock.NewRows([]string{"suspended_at"}))

	now := time.Now().UTC()
	putQ := signPresignedURL("PUT", "/"+bucket+"/"+objectKey, "localhost:8000", testAccessKey, testSecretKey, 3600, now)

	putReq := httptest.NewRequest("PUT", "/"+bucket+"/"+objectKey+"?"+putQ.Encode(),
		bytes.NewReader([]byte(testContent)))
	putReq.Host = "localhost:8000"
	putReq.ContentLength = int64(len(testContent))

	putW := httptest.NewRecorder()
	s.router.ServeHTTP(putW, putReq)
	assert.Equal(t, http.StatusOK, putW.Code, "PUT via presigned URL should succeed")

	// GET
	mock.ExpectQuery(`SELECT secret_key, id FROM tenants WHERE access_key`).
		WithArgs(testAccessKey).
		WillReturnRows(sqlmock.NewRows([]string{"secret_key", "id"}).
			AddRow(testSecretKey, testPresignTenantID))
	mock.ExpectQuery(`SELECT suspended_at FROM tenants WHERE id`).
		WithArgs(testPresignTenantID).
		WillReturnRows(sqlmock.NewRows([]string{"suspended_at"}))

	now = time.Now().UTC()
	getQ := signPresignedURL("GET", "/"+bucket+"/"+objectKey, "localhost:8000", testAccessKey, testSecretKey, 3600, now)

	getReq := httptest.NewRequest("GET", "/"+bucket+"/"+objectKey+"?"+getQ.Encode(), nil)
	getReq.Host = "localhost:8000"

	getW := httptest.NewRecorder()
	s.router.ServeHTTP(getW, getReq)
	assert.Equal(t, http.StatusOK, getW.Code, "GET via presigned URL should succeed")

	body, err := io.ReadAll(getW.Body)
	require.NoError(t, err)
	assert.Equal(t, testContent, string(body))
}

func TestPresignedURL_ExpiredIntegration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger, _ := zap.NewDevelopment()
	s := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		db:       db,
		testMode: false,
		config:   &config.Config{Server: config.ServerConfig{Port: 8000}},
	}
	s.router.HandleFunc("/*", s.handleS3Request)

	// Expect tenant lookup, but expiry should fail first
	mock.ExpectQuery(`SELECT secret_key, id FROM tenants WHERE access_key`).
		WithArgs(testAccessKey).
		WillReturnRows(sqlmock.NewRows([]string{"secret_key", "id"}).
			AddRow(testSecretKey, testPresignTenantID))

	pastTime := time.Now().UTC().Add(-1 * time.Hour)
	q := signPresignedURL("GET", "/bucket/key", "localhost:8000", testAccessKey, testSecretKey, 1, pastTime)

	r := httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
	r.Host = "localhost:8000"

	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)

	var s3Err S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &s3Err))
	assert.Equal(t, ErrExpiredPresignedRequest, s3Err.Code)
}

func TestHandleGetPresignedURL(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger, _ := zap.NewDevelopment()
	s := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		db:       db,
		testMode: false,
		config:   &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	mock.ExpectQuery(`SELECT access_key, secret_key FROM tenants WHERE id`).
		WithArgs(testPresignTenantID).
		WillReturnRows(sqlmock.NewRows([]string{"access_key", "secret_key"}).
			AddRow(testAccessKey, testSecretKey))

	r := httptest.NewRequest("GET", "/api/v1/presigned?bucket=mybucket&key=myfile.txt&method=PUT&expires=7200", nil)
	ctx := context.WithValue(r.Context(), tenantIDKey, testPresignTenantID)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	s.handleGetPresignedURL(w, r)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp["url"], "X-Amz-Signature")
	assert.Contains(t, resp["url"], "X-Amz-Algorithm")
	assert.Equal(t, "PUT", resp["method"])
	assert.NotEmpty(t, resp["expires_at"])

	_, err = time.Parse(time.RFC3339, resp["expires_at"])
	require.NoError(t, err)

	// Verify the generated URL can be validated
	parsedURL, err := url.Parse(resp["url"])
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT secret_key, id FROM tenants WHERE access_key`).
		WithArgs(testAccessKey).
		WillReturnRows(sqlmock.NewRows([]string{"secret_key", "id"}).
			AddRow(testSecretKey, testPresignTenantID))

	verifyReq := httptest.NewRequest("PUT", parsedURL.RequestURI(), bytes.NewReader([]byte("data")))
	verifyReq.Host = parsedURL.Host

	verifiedTenantID, err := s.verifyPresignedURL(verifyReq)
	require.NoError(t, err)
	assert.Equal(t, testPresignTenantID, verifiedTenantID)
}

func TestHandleGetPresignedURL_MissingParams(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	s := &Server{
		logger: logger,
		router: chi.NewRouter(),
		config: &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	r := httptest.NewRequest("GET", "/api/v1/presigned?bucket=mybucket", nil)
	ctx := context.WithValue(r.Context(), tenantIDKey, testPresignTenantID)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	s.handleGetPresignedURL(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleGetPresignedURL_InvalidMethod(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	s := &Server{
		logger: logger,
		router: chi.NewRouter(),
		config: &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	r := httptest.NewRequest("GET", "/api/v1/presigned?bucket=mybucket&key=file&method=DELETE", nil)
	ctx := context.WithValue(r.Context(), tenantIDKey, testPresignTenantID)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	s.handleGetPresignedURL(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
