package drivers

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestS3Signer_BuildCanonicalRequest(t *testing.T) {
	signer := NewS3Signer("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1")

	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt?foo=bar&baz=qux", nil)
	req.Header.Set("Host", "examplebucket.s3.amazonaws.com")
	req.Header.Set("x-amz-date", "20130524T000000Z")

	canonical := signer.buildCanonicalRequest(req, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

	// Should contain method, path, query, headers, signed headers, and payload hash
	if !strings.Contains(canonical, "GET") {
		t.Error("Missing method")
	}

	if !strings.Contains(canonical, "/test.txt") {
		t.Error("Missing path")
	}

	if !strings.Contains(canonical, "baz=qux&foo=bar") {
		t.Error("Query string not sorted")
	}

	if !strings.Contains(canonical, "host:examplebucket.s3.amazonaws.com") {
		t.Error("Missing host header")
	}
}

func TestS3Signer_PresignedURL_Structure(t *testing.T) {
	signer := NewS3Signer("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1")

	url, err := signer.GeneratePresignedURL("GET", "mybucket", "mykey", 15*time.Minute)
	if err != nil {
		t.Fatalf("GeneratePresignedURL failed: %v", err)
	}

	// Verify all required query parameters
	required := []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=",
		"X-Amz-Date=",
		"X-Amz-Expires=900", // 15 minutes
		"X-Amz-SignedHeaders=host",
		"X-Amz-Signature=",
	}

	for _, param := range required {
		if !strings.Contains(url, param) {
			t.Errorf("Missing required parameter: %s", param)
		}
	}

	// Verify URL structure
	if !strings.HasPrefix(url, "https://mybucket.s3.us-east-1.amazonaws.com/mykey?") {
		t.Error("Incorrect URL structure")
	}
}
