package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/lib/pq"
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

func (s *Server) verifyPresignedURL(r *http.Request) (string, *auth.KeyScope, error) {
	q := r.URL.Query()

	algorithm := q.Get("X-Amz-Algorithm")
	credential := q.Get("X-Amz-Credential")
	amzDate := q.Get("X-Amz-Date")
	expiresStr := q.Get("X-Amz-Expires")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	signature := q.Get("X-Amz-Signature")

	if algorithm == "" || credential == "" || amzDate == "" ||
		expiresStr == "" || signedHeaders == "" || signature == "" {
		return "", nil, fmt.Errorf("%s", ErrAuthorizationQueryParametersError)
	}

	expires, err := strconv.Atoi(expiresStr)
	if err != nil || expires < 1 || expires > presignMaxExpires {
		return "", nil, fmt.Errorf("%s", ErrInvalidPresignExpires)
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 || credParts[3] != presignService || credParts[4] != presignAWS4Request {
		return "", nil, fmt.Errorf("%s", ErrAuthorizationQueryParametersError)
	}
	accessKey := credParts[0]
	credDate := credParts[1]
	region := credParts[2]

	reqTime, err := time.Parse(presignTimeFormat, amzDate)
	if err != nil {
		return "", nil, fmt.Errorf("%s", ErrAuthorizationQueryParametersError)
	}

	if time.Now().UTC().After(reqTime.Add(time.Duration(expires) * time.Second)) {
		return "", nil, fmt.Errorf("%s", ErrExpiredPresignedRequest)
	}

	if s.db == nil {
		return "", nil, fmt.Errorf("%s: database not available", ErrAccessDenied)
	}

	// Look up credentials and scope. Try primary tenant key first, then scoped API keys.
	var secretKey, tenantID string
	var scope *auth.KeyScope

	err = s.db.QueryRow(
		`SELECT secret_key, id FROM tenants WHERE access_key = $1`, accessKey,
	).Scan(&secretKey, &tenantID)
	if err == sql.ErrNoRows {
		// Try scoped API key — requires secret_key stored for signature verification.
		var permJSON []byte
		var bucketScope, ipAllowlist pq.StringArray
		var expiresAtDB sql.NullTime
		err = s.db.QueryRow(`
			SELECT ak.secret_key, t.id,
			       COALESCE(ak.permissions, '["*"]'::jsonb),
			       COALESCE(ak.bucket_scope, '{}'),
			       COALESCE(ak.ip_allowlist, '{}'),
			       ak.expires_at
			FROM api_keys ak
			JOIN users u ON u.id = ak.user_id
			JOIN tenants t ON t.email = u.email
			WHERE ak.key_id = $1
		`, accessKey).Scan(&secretKey, &tenantID, &permJSON, &bucketScope, &ipAllowlist, &expiresAtDB)
		if err == nil {
			if secretKey == "" {
				return "", nil, fmt.Errorf("%s", ErrAccessDenied)
			}
			scope = &auth.KeyScope{
				BucketScope: []string(bucketScope),
				IPAllowlist: []string(ipAllowlist),
			}
			if jsonErr := json.Unmarshal(permJSON, &scope.Permissions); jsonErr != nil {
				scope.Permissions = []string{"*"}
			}
			if expiresAtDB.Valid {
				scope.ExpiresAt = &expiresAtDB.Time
			}
		} else if err == sql.ErrNoRows && len(accessKey) >= 4 && accessKey[:4] == "ASIA" {
			// Try STS temporary credential.
			var stsPermJSON []byte
			var stsBucketScope, stsIPRestrict pq.StringArray
			var stsExpiresAt time.Time
			err = s.db.QueryRow(`
				SELECT secret_key, tenant_id, permissions, bucket_scope, ip_restrict, expires_at
				FROM sts_tokens WHERE access_key = $1
			`, accessKey).Scan(&secretKey, &tenantID, &stsPermJSON, &stsBucketScope, &stsIPRestrict, &stsExpiresAt)
			if err != nil {
				return "", nil, fmt.Errorf("%s", ErrAccessDenied)
			}
			if time.Now().After(stsExpiresAt) {
				return "", nil, fmt.Errorf("%s", ErrExpiredPresignedRequest)
			}
			scope = &auth.KeyScope{
				BucketScope: []string(stsBucketScope),
				IPAllowlist: []string(stsIPRestrict),
				ExpiresAt:   &stsExpiresAt,
			}
			if jsonErr := json.Unmarshal(stsPermJSON, &scope.Permissions); jsonErr != nil {
				scope.Permissions = []string{"*"}
			}
		} else {
			return "", nil, fmt.Errorf("%s", ErrAccessDenied)
		}
	} else if err != nil {
		return "", nil, fmt.Errorf("%s", ErrAccessDenied)
	} else {
		scope = &auth.KeyScope{Permissions: []string{"*"}}
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
		return "", nil, fmt.Errorf("%s", ErrSignatureDoesNotMatch)
	}

	return tenantID, scope, nil
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
