package drivers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type S3Signer struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string
}

func NewS3Signer(accessKey, secretKey, region string) *S3Signer {
	return &S3Signer{
		AccessKey: accessKey,
		SecretKey: secretKey,
		Region:    region,
		Service:   "s3",
	}
}

// SignV4 implements AWS Signature Version 4
func (s *S3Signer) SignV4(req *http.Request, payload []byte) error {
	now := time.Now().UTC()
	req.Header.Set("x-amz-date", now.Format("20060102T150405Z"))

	payloadHash := sha256.Sum256(payload)
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(payloadHash[:]))

	canonicalRequest := s.buildCanonicalRequest(req, hex.EncodeToString(payloadHash[:]))
	stringToSign := s.buildStringToSign(canonicalRequest, now)
	signature := s.calculateSignature(stringToSign, now)

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey,
		s.credentialScope(now),
		s.signedHeaders(req),
		signature,
	)
	req.Header.Set("Authorization", auth)
	return nil
}

func (s *S3Signer) buildCanonicalRequest(req *http.Request, payloadHash string) string {
	// 1. HTTP method
	method := req.Method

	// 2. Canonical URI (path)
	uri := req.URL.Path
	if uri == "" {
		uri = "/"
	}

	// 3. Canonical query string
	query := req.URL.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var queryParts []string
	for _, k := range keys {
		for _, v := range query[k] {
			queryParts = append(queryParts, fmt.Sprintf("%s=%s",
				url.QueryEscape(k), url.QueryEscape(v)))
		}
	}
	canonicalQuery := strings.Join(queryParts, "&")

	// 4. Canonical headers
	headerKeys := make([]string, 0)
	headers := make(map[string]string)

	for k, v := range req.Header {
		lower := strings.ToLower(k)
		if lower == "host" || strings.HasPrefix(lower, "x-amz-") {
			headerKeys = append(headerKeys, lower)
			headers[lower] = strings.TrimSpace(v[0])
		}
	}

	// Add host header if not present
	if _, ok := headers["host"]; !ok {
		headers["host"] = req.Host
		headerKeys = append(headerKeys, "host")
	}

	sort.Strings(headerKeys)

	var headerParts []string
	for _, k := range headerKeys {
		headerParts = append(headerParts, fmt.Sprintf("%s:%s", k, headers[k]))
	}
	canonicalHeaders := strings.Join(headerParts, "\n")

	// 5. Signed headers
	signedHeaders := strings.Join(headerKeys, ";")

	// 6. Build the canonical request
	return fmt.Sprintf("%s\n%s\n%s\n%s\n\n%s\n%s",
		method,
		uri,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	)
}

func (s *S3Signer) buildStringToSign(canonicalRequest string, t time.Time) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	return fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		t.Format("20060102T150405Z"),
		s.credentialScope(t),
		hex.EncodeToString(hash[:]))
}

func (s *S3Signer) calculateSignature(stringToSign string, t time.Time) string {
	date := hmacSHA256([]byte("AWS4"+s.SecretKey), t.Format("20060102"))
	region := hmacSHA256(date, s.Region)
	service := hmacSHA256(region, s.Service)
	signing := hmacSHA256(service, "aws4_request")
	signature := hmacSHA256(signing, stringToSign)
	return hex.EncodeToString(signature)
}

func (s *S3Signer) credentialScope(t time.Time) string {
	return fmt.Sprintf("%s/%s/%s/aws4_request", t.Format("20060102"), s.Region, s.Service)
}

func (s *S3Signer) signedHeaders(req *http.Request) string {
	headers := []string{"host"}
	for k := range req.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			headers = append(headers, strings.ToLower(k))
		}
	}
	sort.Strings(headers)
	return strings.Join(headers, ";")
}

// GeneratePresignedURL creates a properly signed presigned URL
func (s *S3Signer) GeneratePresignedURL(method, bucket, key string, expires time.Duration) (string, error) {
	now := time.Now().UTC()

	u := &url.URL{
		Scheme: "https",
		Host:   fmt.Sprintf("%s.s3.%s.amazonaws.com", bucket, s.Region),
		Path:   "/" + key,
	}

	query := u.Query()
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", fmt.Sprintf("%s/%s", s.AccessKey, s.credentialScope(now)))
	query.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	query.Set("X-Amz-Expires", fmt.Sprintf("%d", int(expires.Seconds())))
	query.Set("X-Amz-SignedHeaders", "host")
	u.RawQuery = query.Encode()

	// Build canonical request for presigned URL
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\nhost:%s\n\nhost\nUNSIGNED-PAYLOAD",
		method,
		u.Path,
		u.RawQuery,
		u.Host,
	)

	stringToSign := s.buildStringToSign(canonicalRequest, now)
	signature := s.calculateSignature(stringToSign, now)

	query.Set("X-Amz-Signature", signature)
	u.RawQuery = query.Encode()

	return u.String(), nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
