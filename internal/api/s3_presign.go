package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	presignAlgorithm     = "AWS4-HMAC-SHA256"
	presignTimeFormat    = "20060102T150405Z"
	presignDateFormat    = "20060102"
	presignService       = "s3"
	presignAWS4Request   = "aws4_request"
	presignMaxExpires    = 604800
	presignDefaultRegion = "us-east-1"
)

func isPresignedRequest(r *http.Request) bool {
	return r.URL.Query().Get("X-Amz-Algorithm") == presignAlgorithm
}

func (s *Server) verifyPresignedURL(r *http.Request) (string, error) {
	q := r.URL.Query()

	algorithm := q.Get("X-Amz-Algorithm")
	credential := q.Get("X-Amz-Credential")
	amzDate := q.Get("X-Amz-Date")
	expiresStr := q.Get("X-Amz-Expires")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	signature := q.Get("X-Amz-Signature")

	if algorithm == "" || credential == "" || amzDate == "" ||
		expiresStr == "" || signedHeaders == "" || signature == "" {
		return "", fmt.Errorf("%s", ErrAuthorizationQueryParametersError)
	}

	expires, err := strconv.Atoi(expiresStr)
	if err != nil || expires < 1 || expires > presignMaxExpires {
		return "", fmt.Errorf("%s", ErrInvalidPresignExpires)
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 || credParts[3] != presignService || credParts[4] != presignAWS4Request {
		return "", fmt.Errorf("%s", ErrAuthorizationQueryParametersError)
	}
	accessKey := credParts[0]
	credDate := credParts[1]
	region := credParts[2]

	reqTime, err := time.Parse(presignTimeFormat, amzDate)
	if err != nil {
		return "", fmt.Errorf("%s", ErrAuthorizationQueryParametersError)
	}

	if time.Now().UTC().After(reqTime.Add(time.Duration(expires) * time.Second)) {
		return "", fmt.Errorf("%s", ErrExpiredPresignedRequest)
	}

	if s.db == nil {
		return "", fmt.Errorf("%s: database not available", ErrAccessDenied)
	}

	var secretKey, tenantID string
	err = s.db.QueryRow(
		`SELECT secret_key, id FROM tenants WHERE access_key = $1`, accessKey,
	).Scan(&secretKey, &tenantID)
	if err != nil {
		return "", fmt.Errorf("%s", ErrAccessDenied)
	}

	canonicalURI := uriEncodePath(r.URL.Path)

	canonicalQueryString := buildPresignCanonicalQuery(q)

	headers := strings.Split(signedHeaders, ";")
	sort.Strings(headers)
	var canonicalHeadersBuf strings.Builder
	for _, h := range headers {
		key := strings.ToLower(strings.TrimSpace(h))
		var val string
		if key == "host" {
			val = r.Host
		} else {
			val = r.Header.Get(h)
		}
		canonicalHeadersBuf.WriteString(key)
		canonicalHeadersBuf.WriteByte(':')
		canonicalHeadersBuf.WriteString(strings.TrimSpace(val))
		canonicalHeadersBuf.WriteByte('\n')
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeadersBuf.String(),
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/%s", credDate, region, presignService, presignAWS4Request)

	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		presignAlgorithm,
		amzDate,
		credentialScope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := presignDeriveKey(secretKey, credDate, region)
	expectedSig := hex.EncodeToString(presignHMAC(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return "", fmt.Errorf("%s", ErrSignatureDoesNotMatch)
	}

	return tenantID, nil
}

func buildPresignCanonicalQuery(values url.Values) string {
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

func uriEncodePath(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = uriEncodeSegment(seg)
	}
	return strings.Join(segments, "/")
}

func uriEncodeSegment(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isURIUnreserved(c) {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

func isURIUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

func presignDeriveKey(secretKey, date, region string) []byte {
	kDate := presignHMAC([]byte("AWS4"+secretKey), []byte(date))
	kRegion := presignHMAC(kDate, []byte(region))
	kService := presignHMAC(kRegion, []byte(presignService))
	return presignHMAC(kService, []byte(presignAWS4Request))
}

func presignHMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func generatePresignedS3URL(baseURL, accessKey, secretKey, bucket, key, method string, expiresSec int) (string, time.Time) {
	now := time.Now().UTC()
	date := now.Format(presignDateFormat)
	amzDate := now.Format(presignTimeFormat)
	expiresAt := now.Add(time.Duration(expiresSec) * time.Second)

	credentialScope := fmt.Sprintf("%s/%s/%s/%s", date, presignDefaultRegion, presignService, presignAWS4Request)
	credential := fmt.Sprintf("%s/%s", accessKey, credentialScope)

	path := "/" + bucket + "/" + key
	canonicalURI := uriEncodePath(path)

	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")

	q := url.Values{}
	q.Set("X-Amz-Algorithm", presignAlgorithm)
	q.Set("X-Amz-Credential", credential)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(expiresSec))
	q.Set("X-Amz-SignedHeaders", "host")

	canonicalQueryString := buildPresignCanonicalQuery(q)

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
		amzDate,
		credentialScope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := presignDeriveKey(secretKey, date, presignDefaultRegion)
	signature := hex.EncodeToString(presignHMAC(signingKey, []byte(stringToSign)))

	q.Set("X-Amz-Signature", signature)

	fullURL := baseURL + path + "?" + q.Encode()
	return fullURL, expiresAt
}
